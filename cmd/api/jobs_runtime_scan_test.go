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

// TestScanJobRefusesUnmountedSSDSource is the "ssd"-kind analog of
// TestScanJobRefusesUnmountedShareSource: an SSD source must not be
// scanned while its root_path isn't actually mounted (drive unplugged but
// the mount point directory still sitting there empty), and the index
// must be left untouched.
func TestScanJobRefusesUnmountedSSDSource(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_ = mux

	root := t.TempDir()
	sourceID, err := store.SourceCreate(context.Background(), "ssd", root, "SSD")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	orig := ssdMountedFn
	ssdMountedFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { ssdMountedFn = orig })

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
		t.Fatalf("job state = %s, want error (ssd not mounted); result=%s", final.State, final.Result)
	}
	wantMsg := "is not mounted (SSD unplugged?)"
	if !strings.Contains(final.Error, wantMsg) {
		t.Errorf("job error = %q, want it to contain %q", final.Error, wantMsg)
	}

	count, err := store.SourceSegmentCount(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("SourceSegmentCount: %v", err)
	}
	if count != 0 {
		t.Errorf("SourceSegmentCount = %d, want 0 (index must not be modified)", count)
	}
}

// TestScanJobAllowsUnmountedSSDSourceWithLocalPathsSetting mirrors the
// allow_local_share_paths escape hatch that ingest.ResolveTargetMount
// already honors for share targets (internal/ingest/ingest.go): with the
// hidden setting on, an "ssd"-kind source's mount check is skipped too, so
// a plain temp directory (never actually mounted) is accepted as the scan
// root instead of failing with "is not mounted (SSD unplugged?)".
func TestScanJobAllowsUnmountedSSDSourceWithLocalPathsSetting(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_ = mux

	root := t.TempDir()
	sourceID, err := store.SourceCreate(context.Background(), "ssd", root, "SSD")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	if err := store.SettingsSet(context.Background(), "allow_local_share_paths", []byte("true")); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	orig := ssdMountedFn
	ssdMountedFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { ssdMountedFn = orig })

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
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want success (allow_local_share_paths should skip the mount check); result=%s error=%s", final.State, final.Result, final.Error)
	}
}
