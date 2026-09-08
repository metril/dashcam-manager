package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"dashcam/internal/db"
)

// ErrNotFound is returned by Get/Cancel/Delete when no job with the given
// id exists.
var ErrNotFound = errors.New("jobs: not found")

// EventFunc is called (from whichever goroutine touched the job) after
// every state or progress change. cmd/api wires this to an SSE broker.
type EventFunc func(Job)

// cancelPollInterval is how often a running job's context is checked
// against the row's cancel_requested flag.
const cancelPollInterval = time.Second

// progressThrottle caps how often a handler's progress reports are
// persisted and emitted — the M7 brief asks for at most ~4/s per job.
const progressThrottle = 250 * time.Millisecond

// pollInterval is the fallback cadence workers re-check for queued work on,
// in case a wake-up notification was missed (e.g. all workers busy when
// Enqueue fired it).
const pollInterval = 500 * time.Millisecond

// claimBatch bounds how many queued candidates a worker inspects per pass
// when the head of the queue is blocked on a per-type concurrency limit.
const claimBatch = 20

const jobSelectCols = `SELECT id, type, state, priority, params_json, progress, message, result_json, error, created_at, started_at, finished_at, cancel_requested`

// Queue is a DB-backed job queue over the `jobs` table. Zero value is not
// usable; construct with New.
type Queue struct {
	db *db.DB

	mu       sync.Mutex
	handlers map[string]Handler
	limits   map[string]chan struct{} // per-type semaphore; absent = unlimited
	onEvent  EventFunc

	wake chan struct{}
}

// New returns a Queue over database. Call RegisterHandler for each job type
// before Start, and Start to launch worker goroutines.
func New(database *db.DB) *Queue {
	return &Queue{
		db:       database,
		handlers: map[string]Handler{},
		limits:   map[string]chan struct{}{},
		wake:     make(chan struct{}, 1),
	}
}

// OnEvent sets the callback invoked after every job state/progress change.
// Not safe to call concurrently with itself; call once before Start.
func (q *Queue) OnEvent(fn EventFunc) {
	q.mu.Lock()
	q.onEvent = fn
	q.mu.Unlock()
}

// RegisterHandler associates jobType with h. Must be called before Start
// picks up jobs of that type (jobs of an unregistered type simply sit
// queued until a handler is registered and the queue is nudged again).
func (q *Queue) RegisterHandler(jobType string, h Handler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[jobType] = h
}

// SetLimit caps how many jobs of jobType may run concurrently across all
// workers (e.g. ffmpeg-heavy types limited by settings.max_ffmpeg_jobs).
// n <= 0 removes any limit (unlimited, bounded only by worker count).
func (q *Queue) SetLimit(jobType string, n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n <= 0 {
		delete(q.limits, jobType)
		return
	}
	q.limits[jobType] = make(chan struct{}, n)
}

// Enqueue inserts a new queued job of jobType with params (JSON-marshalled)
// and priority (higher runs first) and returns the created row.
func (q *Queue) Enqueue(ctx context.Context, jobType string, params any, priority int) (*Job, error) {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("jobs: enqueue %s: marshal params: %w", jobType, err)
	}

	var job Job
	err = q.db.Tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			INSERT INTO jobs (type, state, priority, params_json)
			VALUES (?, 'queued', ?, ?)
			RETURNING id, type, state, priority, params_json, progress, message, result_json, error, created_at, started_at, finished_at, cancel_requested`,
			jobType, priority, string(paramsJSON))
		return scanJob(row, &job)
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: enqueue %s: %w", jobType, err)
	}

	q.emit(job)
	q.nudge()
	return &job, nil
}

// Get returns the job with id, or ErrNotFound.
func (q *Queue) Get(ctx context.Context, id int64) (*Job, error) {
	row := q.db.Read().QueryRowContext(ctx, jobSelectCols+` FROM jobs WHERE id = ?`, id)
	var j Job
	if err := scanJob(row, &j); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("jobs: get %d: %w", id, err)
	}
	return &j, nil
}

// List returns up to limit jobs (default/cap 100/500), optionally filtered
// by state, newest first.
func (q *Queue) List(ctx context.Context, state string, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	query := jobSelectCols + ` FROM jobs`
	var args []any
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := q.db.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("jobs: list: %w", err)
	}
	defer rows.Close()

	var out []*Job
	for rows.Next() {
		var j Job
		if err := scanJob(rows, &j); err != nil {
			return nil, fmt.Errorf("jobs: list: %w", err)
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

// Cancel sets cancel_requested on a queued or running job so its worker
// (if any) stops at the next cancellation check, or so a still-queued job
// is skipped when a worker gets to it. A no-op (not an error) on a job
// that has already finished; ErrNotFound if the id doesn't exist at all.
func (q *Queue) Cancel(ctx context.Context, id int64) error {
	res, err := q.db.Write().ExecContext(ctx,
		`UPDATE jobs SET cancel_requested = 1 WHERE id = ? AND state IN ('queued','running')`, id)
	if err != nil {
		return fmt.Errorf("jobs: cancel %d: %w", id, err)
	}
	n, _ := res.RowsAffected()

	j, getErr := q.Get(ctx, id)
	if errors.Is(getErr, ErrNotFound) {
		return ErrNotFound
	}
	if getErr != nil {
		return getErr
	}
	if n > 0 {
		q.emit(*j)
		q.nudge() // a still-queued job can be reaped immediately
	}
	return nil
}

// Delete removes a finished (done/failed/cancelled) job row. Returns an
// error (not ErrNotFound) if the job exists but hasn't finished yet.
func (q *Queue) Delete(ctx context.Context, id int64) error {
	res, err := q.db.Write().ExecContext(ctx,
		`DELETE FROM jobs WHERE id = ? AND state IN ('done','failed','cancelled')`, id)
	if err != nil {
		return fmt.Errorf("jobs: delete %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	if _, err := q.Get(ctx, id); errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	return fmt.Errorf("jobs: delete %d: job has not finished", id)
}

// Start resets any job left 'running' from a previous process (crash or
// restart) back to 'queued', then launches n worker goroutines that poll
// for queued work until ctx is done.
func (q *Queue) Start(ctx context.Context, n int) error {
	if _, err := q.db.Write().ExecContext(ctx,
		`UPDATE jobs SET state = 'queued', started_at = NULL WHERE state = 'running'`); err != nil {
		return fmt.Errorf("jobs: reset running jobs: %w", err)
	}

	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		go q.workerLoop(ctx)
	}
	return nil
}

func (q *Queue) nudge() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
		}
		for q.runNext(ctx) {
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// runNext claims and synchronously runs at most one queued job whose type
// has a registered handler and available concurrency, returning true if it
// ran one (so the caller can immediately look for more) or false if there
// was nothing runnable right now.
func (q *Queue) runNext(ctx context.Context) bool {
	rows, err := q.db.Read().QueryContext(ctx,
		jobSelectCols+` FROM jobs WHERE state = 'queued' ORDER BY priority DESC, id ASC LIMIT ?`, claimBatch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[jobs] list queued: %v", err)
		}
		return false
	}
	var candidates []*Job
	for rows.Next() {
		var j Job
		if err := scanJob(rows, &j); err != nil {
			log.Printf("[jobs] scan queued: %v", err)
			continue
		}
		candidates = append(candidates, &j)
	}
	rows.Close()

	for _, j := range candidates {
		q.mu.Lock()
		h, hasHandler := q.handlers[j.Type]
		sem := q.limits[j.Type]
		q.mu.Unlock()
		if !hasHandler {
			continue
		}

		if sem != nil {
			select {
			case sem <- struct{}{}:
			default:
				continue // this type is at its concurrency limit; try the next candidate
			}
		}

		claimed, err := q.claim(ctx, j.ID)
		if err != nil {
			log.Printf("[jobs] claim %d: %v", j.ID, err)
		}
		if !claimed {
			if sem != nil {
				<-sem
			}
			continue
		}

		q.run(ctx, j, h, sem)
		return true
	}
	return false
}

// claim atomically transitions a queued job to running (guarding against
// two workers picking up the same row).
func (q *Queue) claim(ctx context.Context, id int64) (bool, error) {
	res, err := q.db.Write().ExecContext(ctx,
		`UPDATE jobs SET state = 'running', started_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ? AND state = 'queued'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if j, err := q.Get(ctx, id); err == nil {
			q.emit(*j)
		}
	}
	return n > 0, nil
}

// run executes h for job on this worker's goroutine, blocking until it
// returns, then persists the outcome. sem (if non-nil) is released when
// done, freeing a concurrency slot for that job type.
func (q *Queue) run(ctx context.Context, job *Job, h Handler, sem chan struct{}) {
	defer func() {
		if sem != nil {
			<-sem
		}
	}()

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	watchDone := make(chan struct{})
	go q.watchCancel(job.ID, cancel, watchDone)
	defer close(watchDone)

	var mu sync.Mutex
	var lastReport time.Time
	report := func(progress float64, msg string) {
		mu.Lock()
		now := time.Now()
		if !lastReport.IsZero() && now.Sub(lastReport) < progressThrottle {
			mu.Unlock()
			return
		}
		lastReport = now
		mu.Unlock()
		q.updateProgress(context.Background(), job.ID, progress, msg)
	}

	result, err := func() (result any, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("jobs: handler panic: %v", p)
			}
		}()
		return h(jobCtx, job, report)
	}()

	q.finish(context.Background(), job.ID, result, err, jobCtx.Err() != nil)
}

// watchCancel polls the job's cancel_requested flag and cancels the
// handler's context as soon as it's set (or when done is closed, meaning
// the handler already returned).
func (q *Queue) watchCancel(id int64, cancel context.CancelFunc, done chan struct{}) {
	ticker := time.NewTicker(cancelPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			var flag int
			err := q.db.Read().QueryRowContext(context.Background(),
				`SELECT cancel_requested FROM jobs WHERE id = ?`, id).Scan(&flag)
			if err == nil && flag != 0 {
				cancel()
				return
			}
		}
	}
}

func (q *Queue) updateProgress(ctx context.Context, id int64, progress float64, msg string) {
	if _, err := q.db.Write().ExecContext(ctx,
		`UPDATE jobs SET progress = ?, message = ? WHERE id = ? AND state = 'running'`, progress, msg, id); err != nil {
		log.Printf("[jobs] update progress %d: %v", id, err)
		return
	}
	if j, err := q.Get(ctx, id); err == nil {
		q.emit(*j)
	}
}

// finish persists a job's terminal state: cancelled (if the context was
// cancelled via cancel_requested), failed (handler returned an error),
// or done (handler returned a result, possibly nil).
func (q *Queue) finish(ctx context.Context, id int64, result any, err error, wasCancelled bool) {
	var state State
	var errStr sql.NullString
	var resultJSON sql.NullString

	switch {
	case wasCancelled || errors.Is(err, context.Canceled):
		state = StateCancelled
	case err != nil:
		state = StateFailed
		errStr = sql.NullString{String: err.Error(), Valid: true}
	default:
		state = StateDone
		if result != nil {
			if b, mErr := json.Marshal(result); mErr == nil {
				resultJSON = sql.NullString{String: string(b), Valid: true}
			} else {
				log.Printf("[jobs] marshal result %d: %v", id, mErr)
			}
		}
	}

	var dbErr error
	if state == StateDone {
		_, dbErr = q.db.Write().ExecContext(ctx,
			`UPDATE jobs SET state = ?, progress = 1, result_json = ?, error = ?, finished_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
			string(state), resultJSON, errStr, id)
	} else {
		_, dbErr = q.db.Write().ExecContext(ctx,
			`UPDATE jobs SET state = ?, result_json = ?, error = ?, finished_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`,
			string(state), resultJSON, errStr, id)
	}
	if dbErr != nil {
		log.Printf("[jobs] finish %d: %v", id, dbErr)
	}
	if j, gErr := q.Get(ctx, id); gErr == nil {
		q.emit(*j)
	}
}

func (q *Queue) emit(j Job) {
	q.mu.Lock()
	fn := q.onEvent
	q.mu.Unlock()
	if fn != nil {
		fn(j)
	}
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner, j *Job) error {
	var state string
	var paramsJSON, message, resultJSON, errStr, startedAt, finishedAt sql.NullString
	var cancelReq int

	if err := row.Scan(&j.ID, &j.Type, &state, &j.Priority, &paramsJSON, &j.Progress,
		&message, &resultJSON, &errStr, &j.CreatedAt, &startedAt, &finishedAt, &cancelReq); err != nil {
		return err
	}

	j.State = State(state)
	if paramsJSON.Valid {
		j.Params = json.RawMessage(paramsJSON.String)
	}
	j.Message = message.String
	if resultJSON.Valid {
		j.Result = json.RawMessage(resultJSON.String)
	}
	j.Error = errStr.String
	if startedAt.Valid {
		s := startedAt.String
		j.StartedAt = &s
	}
	if finishedAt.Valid {
		s := finishedAt.String
		j.FinishedAt = &s
	}
	j.CancelRequested = cancelReq != 0
	return nil
}
