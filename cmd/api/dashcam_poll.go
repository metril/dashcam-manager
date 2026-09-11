package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// dashcamPollTickInterval is how often runDashcamPollLoop wakes to
// re-check the dashcam_poll_interval_s setting and every dashcam's
// last_pull_at, mirroring share.HealthInterval's role for shares. A var,
// not a const, so a test can shrink it rather than waiting 30s.
var dashcamPollTickInterval = 30 * time.Second

// dashcamPollProbeTimeout bounds a single dashcam's MaintenanceStatus
// probe within one poll tick, so one unreachable dashcam can't stall the
// rest of the tick (or the loop's next one).
const dashcamPollProbeTimeout = 5 * time.Second

// dashcamPollState is per-loop in-memory state shared across ticks by
// runDashcamPollLoop/dashcamPollTick: currently just lastAttemptedAt,
// keyed by dashcam source id — the poll loop's own memory of when it last
// *tried* to enqueue a pull for this dashcam, independent of
// sources.last_pull_at (which a dashcam_pull job only updates once it's
// actually gotten as far as persisting a cursor — see
// dashcamPullJobHandler's persistCursor/DashcamSetCursor calls). Without
// this, a dashcam whose pull job fails immediately — before ever listing
// a manifest page, e.g. dashcamTargetShare failing because the target
// share unmounted — would never move last_pull_at, and every poll tick
// (every dashcamPollTickInterval, 30s by default) would re-enqueue
// another doomed dashcam_pull job: a hot retry loop hammering the
// dashcam/job queue far faster than dashcam_poll_interval_s intends.
type dashcamPollState struct {
	mu              sync.Mutex
	lastAttemptedAt map[int64]time.Time
}

func newDashcamPollState() *dashcamPollState {
	return &dashcamPollState{lastAttemptedAt: map[int64]time.Time{}}
}

// due reports whether id hasn't had a poll-loop enqueue attempt recorded
// within interval of now.
func (s *dashcamPollState) due(id int64, interval time.Duration, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastAttemptedAt[id]
	return !ok || now.Sub(last) >= interval
}

func (s *dashcamPollState) markAttempted(id int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttemptedAt[id] = now
}

// dashcamPollProbe fetches a dashcam's maintenance status to decide
// whether it's reachable enough to be worth enqueuing a pull for.
// Overridable in tests to avoid a real HTTP round trip or to inject a
// failure without tearing down the fake server.
type dashcamPollProbe func(ctx context.Context, client *dashcam.Client) error

// defaultDashcamPollProbe is the production dashcamPollProbe: a signed GET
// /api/ingest/maintenance bounded by dashcamPollProbeTimeout.
func defaultDashcamPollProbe(ctx context.Context, client *dashcam.Client) error {
	pctx, cancel := context.WithTimeout(ctx, dashcamPollProbeTimeout)
	defer cancel()
	_, err := client.MaintenanceStatus(pctx)
	return err
}

// runDashcamPollLoop periodically enqueues a "dashcam_pull" job for every
// configured dashcam that's due (per the dashcam_poll_interval_s setting,
// compared against last_pull_at) and reachable (per probe), skipping any
// dashcam that already has a queued or running dashcam_pull job. A
// dashcam_poll_interval_s of 0 (the default — see main.go's
// seedDefaultSettings) disables the loop entirely, so an operator opts in
// only once they've configured at least one dashcam. Intended to run for
// the lifetime of the process in its own goroutine (see main.go, beside
// share.RunHealthLoop), returning once ctx is done.
func runDashcamPollLoop(ctx context.Context, store *db.DB, q *jobs.Queue, secretKey string, probe dashcamPollProbe) {
	ticker := time.NewTicker(dashcamPollTickInterval)
	defer ticker.Stop()
	state := newDashcamPollState()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dashcamPollTick(ctx, store, q, secretKey, probe, state)
		}
	}
}

// dashcamPollTick runs one pass of runDashcamPollLoop's logic; split out
// so a test can call it directly without waiting on a ticker. state is
// the poll loop's in-memory "last attempted" bookkeeping (see
// dashcamPollState's doc comment) — a caller driving dashcamPollTick
// directly across multiple calls (rather than through
// runDashcamPollLoop) must reuse the same state across those calls for
// the hot-retry guard to have any effect.
func dashcamPollTick(ctx context.Context, store *db.DB, q *jobs.Queue, secretKey string, probe dashcamPollProbe, state *dashcamPollState) {
	interval, ok := dashcamPollInterval(ctx, store)
	if !ok {
		return
	}

	dashcams, err := store.DashcamList(ctx)
	if err != nil {
		log.Printf("[dashcam] poll: list dashcams: %v", err)
		return
	}
	if len(dashcams) == 0 {
		return
	}

	active, err := activeJobsBySourceOfType(ctx, q, map[string]bool{"dashcam_pull": true})
	if err != nil {
		log.Printf("[dashcam] poll: active dashcam_pull jobs: %v", err)
		return
	}

	now := time.Now()
	for _, src := range dashcams {
		if ctx.Err() != nil {
			return
		}
		if _, busy := active[src.ID]; busy {
			continue
		}
		if src.LastPullAt != nil {
			if last, err := time.Parse(time.RFC3339Nano, *src.LastPullAt); err == nil && now.Sub(last) < interval {
				continue
			}
		}
		// The DB-side last_pull_at check above only catches a pull that
		// got at least as far as persisting a cursor — a pull that fails
		// before that (e.g. no client/target share) would leave
		// last_pull_at untouched and get re-enqueued on every tick
		// without this guard. See dashcamPollState's doc comment.
		if !state.due(src.ID, interval, now) {
			continue
		}

		_, client, err := dashcamClientFor(ctx, store, secretKey, src.ID)
		if err != nil {
			log.Printf("[dashcam] poll %d: %v", src.ID, err)
			continue
		}
		if err := probe(ctx, client); err != nil {
			log.Printf("[dashcam] poll %d: probe unreachable: %v", src.ID, err)
			continue
		}

		// Stamp "attempted" before enqueueing, not after: the guard's
		// purpose is to bound how often this loop *tries* to enqueue,
		// regardless of whether the enqueue call itself (or the job it
		// starts) succeeds.
		state.markAttempted(src.ID, now)
		if _, err := q.Enqueue(ctx, "dashcam_pull", dashcamPullJobParams{SourceID: src.ID}, -1); err != nil {
			log.Printf("[dashcam] poll %d: enqueue dashcam_pull: %v", src.ID, err)
			continue
		}
		log.Printf("[dashcam] poll: enqueued dashcam_pull for source %d", src.ID)
	}
}

// dashcamPollInterval reads and validates the dashcam_poll_interval_s
// setting, returning ok=false when it's absent, zero, or negative — the
// "loop disabled" case.
func dashcamPollInterval(ctx context.Context, store *db.DB) (time.Duration, bool) {
	raw, err := store.SettingsGet(ctx, "dashcam_poll_interval_s")
	if errors.Is(err, db.ErrNotFound) {
		return 0, false
	}
	if err != nil {
		log.Printf("[dashcam] poll: read dashcam_poll_interval_s: %v", err)
		return 0, false
	}
	var intervalS int
	if err := json.Unmarshal(raw, &intervalS); err != nil || intervalS <= 0 {
		return 0, false
	}
	return time.Duration(intervalS) * time.Second, true
}
