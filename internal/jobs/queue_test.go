package jobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dashcam/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "jobs-test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return d
}

// sleepParams is the params shape for the test-only "sleep" job type below.
type sleepParams struct {
	Millis int `json:"millis"`
}

// sleepHandler completes after params.Millis, reporting progress once
// partway through, but returns early (as a cancelled context) if the job's
// cancel_requested flag is set — used to exercise the cancel-mid-run path
// without a real long-running operation.
func sleepHandler(ctx context.Context, job *Job, report ReportFunc) (any, error) {
	var p sleepParams
	_ = json.Unmarshal(job.Params, &p)
	report(0.5, "halfway")
	select {
	case <-time.After(time.Duration(p.Millis) * time.Millisecond):
		return map[string]string{"slept": "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitForState(t *testing.T, q *Queue, id int64, want State, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *Job
	for time.Now().Before(deadline) {
		j, err := q.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get(%d): %v", id, err)
		}
		last = j
		if j.State == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d did not reach state %q within %s (last state %q)", id, want, timeout, last.State)
	return nil
}

func TestEnqueueRunComplete(t *testing.T) {
	d := openTestDB(t)
	q := New(d)
	q.RegisterHandler("sleep", sleepHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := q.Enqueue(context.Background(), "sleep", sleepParams{Millis: 20}, 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job.State != StateQueued {
		t.Fatalf("new job state = %q, want queued", job.State)
	}

	final := waitForState(t, q, job.ID, StateDone, 2*time.Second)
	if final.Progress != 1 {
		t.Errorf("finished progress = %v, want 1", final.Progress)
	}
	if final.Result == nil {
		t.Errorf("finished result is nil, want the handler's map")
	}
	if final.StartedAt == nil || final.FinishedAt == nil {
		t.Errorf("started_at/finished_at not set: %+v", final)
	}
}

func TestCancelMidRun(t *testing.T) {
	d := openTestDB(t)
	q := New(d)
	q.RegisterHandler("sleep", sleepHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := q.Enqueue(context.Background(), "sleep", sleepParams{Millis: 10_000}, 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitForState(t, q, job.ID, StateRunning, 2*time.Second)

	if err := q.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// watchCancel polls every second in the queue, so allow enough slack.
	final := waitForState(t, q, job.ID, StateCancelled, 3*time.Second)
	if !final.CancelRequested {
		t.Errorf("finished job cancel_requested = false, want true")
	}
}

func TestCancelAlreadyFinishedIsNoop(t *testing.T) {
	d := openTestDB(t)
	q := New(d)
	q.RegisterHandler("sleep", sleepHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := q.Enqueue(context.Background(), "sleep", sleepParams{Millis: 1}, 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitForState(t, q, job.ID, StateDone, 2*time.Second)

	if err := q.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel on finished job should be a no-op, got: %v", err)
	}
	final, err := q.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != StateDone {
		t.Errorf("state after no-op cancel = %q, want done", final.State)
	}
}

func TestCancelUnknownJobReturnsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	q := New(d)
	if err := q.Cancel(context.Background(), 99999); err != ErrNotFound {
		t.Errorf("Cancel(unknown) = %v, want ErrNotFound", err)
	}
}

func TestRestartResetsRunningJobs(t *testing.T) {
	d := openTestDB(t)

	// Simulate a crash: insert a job directly in 'running' state, as if a
	// previous process had claimed it and then died before finishing.

	res, err := d.Write().ExecContext(context.Background(),
		`INSERT INTO jobs (type, state, priority, started_at) VALUES ('sleep', 'running', 0, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("insert running job: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	q := New(d)
	q.RegisterHandler("sleep", sleepHandler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start() resets running->queued synchronously before launching
	// workers, so a lone worker will then pick it up and run it to
	// completion (sleep(0) params -> immediate).
	final := waitForState(t, q, id, StateDone, 2*time.Second)
	if final.StartedAt == nil {
		t.Errorf("restarted job never got a fresh started_at")
	}
}

func TestPerTypeSemaphoreLimitsConcurrency(t *testing.T) {
	d := openTestDB(t)
	q := New(d)

	var current, maxSeen int32
	var mu sync.Mutex
	handler := func(ctx context.Context, job *Job, report ReportFunc) (any, error) {
		n := atomic.AddInt32(&current, 1)
		mu.Lock()
		if n > maxSeen {
			maxSeen = n
		}
		mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil, nil
	}
	q.RegisterHandler("limited", handler)
	q.SetLimit("limited", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 4 workers, but the "limited" type's semaphore caps concurrent runs at 1.
	if err := q.Start(ctx, 4); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var ids []int64
	for i := 0; i < 4; i++ {
		job, err := q.Enqueue(context.Background(), "limited", nil, 0)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		ids = append(ids, job.ID)
	}

	for _, id := range ids {
		waitForState(t, q, id, StateDone, 3*time.Second)
	}

	mu.Lock()
	got := maxSeen
	mu.Unlock()
	if got > 1 {
		t.Errorf("max concurrent 'limited' jobs observed = %d, want <= 1 (semaphore not enforced)", got)
	}
}

// TestCancelQueuedJobNeverRuns covers M7 review finding #1: cancelling a
// job that's still queued must prevent it from ever running at all (not
// just mark it cancelled after the fact).
func TestCancelQueuedJobNeverRuns(t *testing.T) {
	d := openTestDB(t)
	q := New(d)

	var ran atomic.Bool
	q.RegisterHandler("sleep", func(ctx context.Context, job *Job, report ReportFunc) (any, error) {
		ran.Store(true)
		return nil, nil
	})

	job, err := q.Enqueue(context.Background(), "sleep", sleepParams{Millis: 1}, 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := q.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, err := q.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateCancelled {
		t.Fatalf("state right after cancelling a queued job = %q, want cancelled immediately", got.State)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(200 * time.Millisecond) // let workers poll a few times
	if ran.Load() {
		t.Errorf("handler ran for a job that was cancelled while still queued")
	}
	final, err := q.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != StateCancelled {
		t.Errorf("final state = %q, want cancelled", final.State)
	}
}

// TestRestartSweepsStaleCancelledQueuedJob covers the restart race behind
// finding #1: a job cancelled while running, whose cancel never got
// persisted as 'cancelled' before the process died (Start()'s
// running->queued reset would otherwise requeue it as if nothing had
// happened), must come back as 'cancelled' — and never run — on the next
// Start(), not silently re-run.
func TestRestartSweepsStaleCancelledQueuedJob(t *testing.T) {
	d := openTestDB(t)

	var ran atomic.Bool
	res, err := d.Write().ExecContext(context.Background(), `
		INSERT INTO jobs (type, state, priority, cancel_requested, started_at)
		VALUES ('sleep', 'running', 0, 1, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`)
	if err != nil {
		t.Fatalf("insert stale running+cancelled job: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	q := New(d)
	q.RegisterHandler("sleep", func(ctx context.Context, job *Job, report ReportFunc) (any, error) {
		ran.Store(true)
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForState(t, q, id, StateCancelled, 2*time.Second)
	if ran.Load() {
		t.Errorf("handler ran for a job that had cancel_requested set before the restart")
	}
}

// TestShutdownRequeuesInFlightJobWithoutCancelling covers M7 review finding
// #2: an in-flight job whose worker ctx is cancelled by app shutdown (not
// by a user Cancel() call) must come back as 'queued' for the next
// process, not be mislabeled 'cancelled'.
func TestShutdownRequeuesInFlightJobWithoutCancelling(t *testing.T) {
	d := openTestDB(t)
	q := New(d)

	started := make(chan struct{})
	q.RegisterHandler("sleep", func(ctx context.Context, job *Job, report ReportFunc) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := q.Enqueue(context.Background(), "sleep", nil, 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}
	waitForState(t, q, job.ID, StateRunning, 2*time.Second)

	// Simulate app shutdown: cancel the queue's own ctx (as main.go would
	// via signal.NotifyContext), not a user Cancel() call.
	cancel()
	time.Sleep(300 * time.Millisecond) // let the worker notice and requeue

	final, err := q.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != StateQueued {
		t.Errorf("job state after shutdown mid-run = %q, want queued (must not be mislabeled cancelled)", final.State)
	}
	if final.CancelRequested {
		t.Errorf("cancel_requested = true after a shutdown with no user cancel, want false")
	}
	if final.StartedAt != nil {
		t.Errorf("started_at not cleared on shutdown requeue: %v", *final.StartedAt)
	}
}

// TestWaitReturnsAfterWorkersStop covers finding #3's Queue.Wait: it must
// unblock once every worker goroutine has actually exited.
func TestWaitReturnsAfterWorkersStop(t *testing.T) {
	d := openTestDB(t)
	q := New(d)
	q.RegisterHandler("sleep", sleepHandler)

	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Start(ctx, 3); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := q.Wait(waitCtx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestWaitTimesOutIfWorkerStuck ensures Wait is actually bounded by its own
// ctx rather than blocking forever if a worker never exits.
func TestWaitTimesOutIfWorkerStuck(t *testing.T) {
	d := openTestDB(t)
	q := New(d)

	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // let the stuck goroutine finish, don't leak past the test

	q.RegisterHandler("sleep", func(ctx context.Context, job *Job, report ReportFunc) (any, error) {
		<-block // deliberately ignores ctx cancellation, simulating a stuck handler
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := q.Enqueue(context.Background(), "sleep", nil, 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitForState(t, q, job.ID, StateRunning, 2*time.Second)

	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer waitCancel()
	if err := q.Wait(waitCtx); err == nil {
		t.Fatalf("Wait returned nil, want a timeout error since the handler ignores ctx cancellation")
	}
}

// TestResizeGrowsAndShrinksWithoutDroppingJobs resizes the worker pool up
// then down while jobs are running: every job still finishes, concurrency
// follows the new size, and a shrink never interrupts a running job.
func TestResizeGrowsAndShrinksWithoutDroppingJobs(t *testing.T) {
	d := openTestDB(t)
	q := New(d)

	var current, maxSeen atomic.Int32
	var finished atomic.Int32
	q.RegisterHandler("work", func(ctx context.Context, job *Job, report ReportFunc) (any, error) {
		n := current.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		current.Add(-1)
		finished.Add(1)
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	enqueue := func(n int) (ids []int64) {
		for i := 0; i < n; i++ {
			job, err := q.Enqueue(context.Background(), "work", nil, 0)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			ids = append(ids, job.ID)
		}
		return ids
	}

	ids := enqueue(6)
	q.Resize(4) // grow while the first job is running
	for _, id := range ids {
		waitForState(t, q, id, StateDone, 5*time.Second)
	}
	if got := maxSeen.Load(); got < 2 || got > 4 {
		t.Errorf("max concurrency after growing to 4 = %d, want 2..4", got)
	}

	q.Resize(1)
	time.Sleep(3 * pollInterval) // let the excess workers notice and retire
	maxSeen.Store(0)
	ids = enqueue(4)
	for _, id := range ids {
		waitForState(t, q, id, StateDone, 5*time.Second)
	}
	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrency after shrinking to 1 = %d, want 1", got)
	}
	if got := finished.Load(); got != 10 {
		t.Errorf("finished = %d, want 10 (no job dropped)", got)
	}
}
