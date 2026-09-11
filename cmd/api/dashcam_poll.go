package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dashcamPollTick(ctx, store, q, secretKey, probe)
		}
	}
}

// dashcamPollTick runs one pass of runDashcamPollLoop's logic; split out
// so a test can call it directly without waiting on a ticker.
func dashcamPollTick(ctx context.Context, store *db.DB, q *jobs.Queue, secretKey string, probe dashcamPollProbe) {
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

		_, client, err := dashcamClientFor(ctx, store, secretKey, src.ID)
		if err != nil {
			log.Printf("[dashcam] poll %d: %v", src.ID, err)
			continue
		}
		if err := probe(ctx, client); err != nil {
			log.Printf("[dashcam] poll %d: probe unreachable: %v", src.ID, err)
			continue
		}

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
