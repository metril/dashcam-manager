package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// TestScanJobRefusesUnmountedShareSource exercises the guard added for
// final-findings item 1: a "share"-kind source must not be scanned while
// its share isn't actually mounted at root_path, since an unmounted share
// root looks like (or is) an empty directory and scanning it could wipe an
// already-indexed source. allow_local_share_paths is deliberately left off
// here so ResolveTargetMount falls through to the real share.IsMounted
// check, which will report false for a plain temp directory.
func TestScanJobRefusesUnmountedShareSource(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_ = mux

	root := t.TempDir()
	shareID, sourceID, err := store.ShareCreateWithSource(context.Background(), db.ShareCreateParams{
		Name: "test", Server: "srv", ShareName: "footage", Enabled: true,
	}, func(id int64) string { return root }, "test share")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}
	_ = shareID

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	job, err := queue.Enqueue(ctx, "scan", scanJobParams{SourceID: sourceID}, 0)
	if err != nil {
		t.Fatalf("enqueue scan: %v", err)
	}

	final := waitForJobTerminal(t, queue, job.ID, 10*time.Second)
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want error (share not mounted); result=%s", final.State, final.Result)
	}
	if !strings.Contains(final.Error, "not mounted") && !strings.Contains(final.Error, "not available") {
		t.Errorf("job error = %q, want mention of the share not being mounted/available", final.Error)
	}
}
