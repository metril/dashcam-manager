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
	dashcamPollTick(context.Background(), f.store, f.queue, "test-secret-key", probe)

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

	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", defaultDashcamPollProbe)

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
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe)

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
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe)

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
	dashcamPollTick(ctx, f.store, f.queue, "test-secret-key", probe)

	if probed {
		t.Fatal("poll tick probed a dashcam whose last_pull_at is within the interval")
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
