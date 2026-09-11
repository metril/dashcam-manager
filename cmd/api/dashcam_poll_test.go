package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dashcam/internal/dashcam"
)

func TestDashcamPollTick_DisabledByDefault(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)

	probed := false
	probe := func(ctx context.Context, c *dashcam.Client) error { probed = true; return nil }
	dashcamPollTick(context.Background(), f.store, f.queue, "test-secret-key", probe, newDashcamPollState())

	if probed {
		t.Fatal("poll tick probed a dashcam while dashcam_poll_interval_s is unset/0")
	}
}

func TestDashcamPollTick_EnqueuesWhenDueAndReachable(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)
	ctx := context.Background()
	if err := f.store.SettingsSet(ctx, "dashcam_poll_interval_s", json.RawMessage(`60`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", defaultDashcamPollProbe, newDashcamPollState())

	list, err := f.queue.List(ctx, "queued", 10)
	if err != nil {
		t.Fatalf("List queued: %v", err)
	}
	found := false
	for _, j := range list {
		if j.Type == "dashcam_pull" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a dashcam_pull job to be enqueued, queued jobs = %+v", list)
	}
}

func TestDashcamPollTick_SkipsWhenPullAlreadyActive(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)
	ctx := context.Background()
	if err := f.store.SettingsSet(ctx, "dashcam_poll_interval_s", json.RawMessage(`60`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	if _, err := f.queue.Enqueue(ctx, "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, -1); err != nil {
		t.Fatalf("seed dashcam_pull: %v", err)
	}

	probed := false
	probe := func(ctx context.Context, c *dashcam.Client) error { probed = true; return nil }
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe, newDashcamPollState())

	if probed {
		t.Fatal("poll tick probed a dashcam that already has an active dashcam_pull job")
	}
}

func TestDashcamPollTick_SkipsUnreachableDashcamWithoutEnqueuing(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)
	ctx := context.Background()
	if err := f.store.SettingsSet(ctx, "dashcam_poll_interval_s", json.RawMessage(`60`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	probe := func(ctx context.Context, c *dashcam.Client) error { return context.DeadlineExceeded }
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe, newDashcamPollState())

	list, err := f.queue.List(ctx, "queued", 10)
	if err != nil {
		t.Fatalf("List queued: %v", err)
	}
	for _, j := range list {
		if j.Type == "dashcam_pull" {
			t.Fatalf("dashcam_pull was enqueued for an unreachable dashcam: %+v", j)
		}
	}
}

func TestDashcamPollTick_SkipsWhenNotYetDue(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)
	ctx := context.Background()
	if err := f.store.SettingsSet(ctx, "dashcam_poll_interval_s", json.RawMessage(`3600`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	if err := f.store.DashcamSetCursor(ctx, f.dashcamSrcID, "", time.Now()); err != nil {
		t.Fatalf("DashcamSetCursor: %v", err)
	}

	probed := false
	probe := func(ctx context.Context, c *dashcam.Client) error { probed = true; return nil }
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe, newDashcamPollState())

	if probed {
		t.Fatal("poll tick probed a dashcam whose last_pull_at is within the interval")
	}
}

// TestDashcamPollTick_HotRetryGuardedByInMemoryState is fix-round item 4's
// covering test: a dashcam_pull that fails before ever calling
// DashcamSetCursor (e.g. no client configured) leaves sources.last_pull_at
// untouched, so the DB-side "is it due" check alone would re-enqueue on
// every single tick. Reusing the same dashcamPollState across two ticks
// must suppress the second enqueue even though last_pull_at never moved.
func TestDashcamPollTick_HotRetryGuardedByInMemoryState(t *testing.T) {
	f := newDashcamTestFixture(t)
	// Deliberately not f.start(t): workers must NOT run the enqueued
	// dashcam_pull jobs here — a completed (or still in-flight) job would
	// make activeJobsBySourceOfType's own busy-check the reason a later
	// tick doesn't probe, muddying what this test is actually isolating
	// (the in-memory dashcamPollState guard). Each tick's enqueued job is
	// cancelled immediately below instead, purely so the *next* tick's
	// busy-check sees nothing queued.
	ctx := context.Background()
	if err := f.store.SettingsSet(ctx, "dashcam_poll_interval_s", json.RawMessage(`3600`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	cancelQueuedDashcamPulls := func() {
		t.Helper()
		list, err := f.queue.List(ctx, "queued", 10)
		if err != nil {
			t.Fatalf("List queued: %v", err)
		}
		for _, j := range list {
			if j.Type == "dashcam_pull" {
				if err := f.queue.Cancel(ctx, j.ID); err != nil {
					t.Fatalf("Cancel %d: %v", j.ID, err)
				}
			}
		}
	}

	state := newDashcamPollState()
	probeCalls := 0
	probe := func(ctx context.Context, c *dashcam.Client) error { probeCalls++; return nil }

	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe, state)
	if probeCalls != 1 {
		t.Fatalf("1st tick probeCalls = %d, want 1", probeCalls)
	}
	cancelQueuedDashcamPulls()

	// sources.last_pull_at is still nil/untouched (no dashcam_pull job
	// ran to completion here — this test only drives dashcamPollTick
	// directly), so without the in-memory guard a 2nd tick would probe
	// (and enqueue) again immediately.
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe, state)
	if probeCalls != 1 {
		t.Fatalf("2nd tick (same state, within interval) probeCalls = %d, want still 1 (hot retry not suppressed)", probeCalls)
	}
	cancelQueuedDashcamPulls()

	// A fresh dashcamPollState (as if the process restarted) has no
	// memory of the 1st tick's attempt and must probe again — confirms
	// the suppression really is the in-memory state, not some other
	// accidental gate.
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe, newDashcamPollState())
	if probeCalls != 2 {
		t.Fatalf("tick with fresh state probeCalls = %d, want 2", probeCalls)
	}
}

// TestRunDashcamPollLoop_TicksAndStops is a light integration check that
// runDashcamPollLoop actually wakes on its ticker and enqueues, and
// returns promptly once ctx is cancelled.
func TestRunDashcamPollLoop_TicksAndStops(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)
	ctx := context.Background()
	if err := f.store.SettingsSet(ctx, "dashcam_poll_interval_s", json.RawMessage(`60`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	old := dashcamPollTickInterval
	dashcamPollTickInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamPollTickInterval = old })

	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runDashcamPollLoop(loopCtx, f.store, f.queue, "test-secret-key", defaultDashcamPollProbe)
		close(done)
	}()

	// Workers are running (f.start above), so by the time this polls, the
	// enqueued job may already have finished — check every state, not
	// just "queued".
	deadline := time.After(2 * time.Second)
	for {
		found := false
		for _, state := range []string{"queued", "running", "done", "failed"} {
			list, err := f.queue.List(context.Background(), state, 10)
			if err != nil {
				t.Fatalf("List(%s): %v", state, err)
			}
			for _, j := range list {
				if j.Type == "dashcam_pull" {
					found = true
				}
			}
		}
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runDashcamPollLoop never enqueued a dashcam_pull job")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runDashcamPollLoop did not return after ctx cancellation")
	}
}
