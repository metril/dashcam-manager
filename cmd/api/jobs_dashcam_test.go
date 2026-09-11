package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/dashcam/dashcamtest"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// dashcamTestFixture bundles everything a dashcam_* job test needs: a
// store+queue (workers not started yet, so a test can seed rows and stub
// package vars before anything runs), a fake recorder, and the
// kind='dashcam'/kind='share' source ids the jobs act on.
type dashcamTestFixture struct {
	store         *db.DB
	queue         *jobs.Queue
	fake          *dashcamtest.Fake
	shareRoot     string
	dashcamSrcID  int64
	shareSrcID    int64
	targetShareID int64
}

// newDashcamTestFixture wires a store/queue with DASHCAM_SECRET_KEY set
// (before registerJobHandlers runs, so dashcam.LoadKey has something to
// decrypt against), a mounted-via-allow_local_share_paths target share,
// a dashcam source pointed at a fresh dashcamtest.Fake, and a saved
// signing key the fake accepts. Workers are not started — call
// t.queue.Start yourself once any further fixture-specific stubbing
// (e.g. dashcamReprocessPollInterval) is in place.
func newDashcamTestFixture(t *testing.T) *dashcamTestFixture {
	t.Helper()
	t.Setenv("DASHCAM_SECRET_KEY", "test-secret-key")

	dbPath := filepath.Join(t.TempDir(), "dashcam.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Setenv("DASHCAM_DB", dbPath)

	ctx := context.Background()
	if err := store.SettingsSet(ctx, "allow_local_share_paths", []byte("true")); err != nil {
		t.Fatalf("SettingsSet allow_local_share_paths: %v", err)
	}

	shareRoot := t.TempDir()
	shareID, shareSrcID, err := store.ShareCreateWithSource(ctx, db.ShareCreateParams{
		Name: "target", Server: "srv", ShareName: "footage", Enabled: true,
	}, func(id int64) string { return shareRoot }, "target share")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}

	fake := dashcamtest.New(t.TempDir())
	t.Cleanup(fake.Close)

	keyID, pub, priv, err := dashcam.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	fake.AddKey(keyID, pub)
	if err := dashcam.SaveKey(ctx, store, "test-secret-key", keyID, priv); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}

	dashcamSrcID, err := store.DashcamCreate(ctx, "test dashcam", fake.URL(), shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	queue := jobs.New(store)
	registerJobHandlers(queue, store)

	return &dashcamTestFixture{
		store: store, queue: queue, fake: fake, shareRoot: shareRoot,
		dashcamSrcID: dashcamSrcID, shareSrcID: shareSrcID, targetShareID: shareID,
	}
}

func (f *dashcamTestFixture) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		// Wait for every worker goroutine to actually return before the
		// store.Close/TempDir-removal cleanups registered by
		// newDashcamTestFixture run (t.Cleanup is LIFO, so those run
		// after this one) — cancel() alone only asks workers to stop;
		// without waiting, a worker can still be mid-DB-op or writing a
		// pulled file when the store closes / the tempdir is removed
		// out from under it, which is what caused the intermittent
		// "sql: database is closed" / "TempDir RemoveAll cleanup:
		// directory not empty" flake.
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelWait()
		if err := f.queue.Wait(waitCtx); err != nil {
			t.Logf("queue.Wait: %v", err)
		}
	})
	if err := f.queue.Start(ctx, 4); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
}

func mustAddSegment(t *testing.T, fake *dashcamtest.Fake, camera, relPath string, content []byte, start, end string) {
	t.Helper()
	if err := fake.AddSegment(camera, relPath, content, start, end, true, true, true, false); err != nil {
		t.Fatalf("AddSegment %s: %v", relPath, err)
	}
}

func decodeResult(t *testing.T, job *jobs.Job) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(job.Result, &out); err != nil {
		t.Fatalf("decode job result %s: %v", job.Result, err)
	}
	return out
}

// -- dashcam_pull ----------------------------------------------------------

func TestDashcamPullJob_LandsFilesLinksAndSetsCursor(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}

	res := decodeResult(t, final)
	if res["pulled"].(float64) != 1 {
		t.Errorf("pulled = %v, want 1", res["pulled"])
	}
	if res["skipped"].(float64) != 0 {
		t.Errorf("skipped = %v, want 0", res["skipped"])
	}
	cursor, _ := res["cursor"].(string)
	if cursor == "" {
		t.Error("expected a non-empty cursor in the result")
	}

	mp4 := filepath.Join(f.shareRoot, "front", "2026", "09", "08", "recording_1", "segment_1.mp4")
	if _, err := os.Stat(mp4); err != nil {
		t.Errorf("expected mp4 at %s: %v", mp4, err)
	}
	for _, suffix := range []string{".timestamp.json", ".detections.json", ".gps.json"} {
		sc := filepath.Join(f.shareRoot, "front", "2026", "09", "08", "recording_1", "segment_1"+suffix)
		if _, err := os.Stat(sc); err != nil {
			t.Errorf("expected sidecar at %s: %v", sc, err)
		}
	}

	src, err := f.store.DashcamGet(context.Background(), f.dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGet: %v", err)
	}
	if src.PullCursor == nil || *src.PullCursor == "" {
		t.Error("expected pull_cursor to be persisted on the source row")
	}

	pull, found, err := f.store.PullGet(context.Background(), f.dashcamSrcID, "front/2026/09/08/recording_1/segment_1.mp4")
	if err != nil {
		t.Fatalf("PullGet: %v", err)
	}
	if !found || pull.VerifiedAt == nil {
		t.Fatalf("expected a verified dashcam_pulls row, got found=%v pull=%+v", found, pull)
	}

	// Re-run: everything already pulled at the same size, so a second
	// full pass must skip rather than re-download.
	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID, Full: true}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull (2nd): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	res2 := decodeResult(t, final2)
	if res2["pulled"].(float64) != 0 {
		t.Errorf("2nd run pulled = %v, want 0", res2["pulled"])
	}
	if res2["skipped"].(float64) != 1 {
		t.Errorf("2nd run skipped = %v, want 1", res2["skipped"])
	}
}

func TestDashcamPullJob_NoKeyConfiguredFailsJob(t *testing.T) {
	// Separate, minimal fixture that never calls SaveKey at all.
	t.Setenv("DASHCAM_SECRET_KEY", "test-secret-key")
	dbPath := filepath.Join(t.TempDir(), "dashcam.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Setenv("DASHCAM_DB", dbPath)

	shareRoot := t.TempDir()
	shareID, _, err := store.ShareCreateWithSource(context.Background(), db.ShareCreateParams{
		Name: "target", Server: "srv", ShareName: "footage", Enabled: true,
	}, func(id int64) string { return shareRoot }, "target share")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}

	dashcamSrcID, err := store.DashcamCreate(context.Background(), "test", "http://127.0.0.1:1", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	job, err := queue.Enqueue(ctx, "dashcam_pull", dashcamPullJobParams{SourceID: dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, queue, job.ID, 10*time.Second)
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed; result=%s", final.State, final.Result)
	}
	if final.Error == "" {
		t.Fatal("expected a non-empty error")
	}
}

// -- dashcam_cleanup ---------------------------------------------------

// pullAndScanForCleanup runs a dashcam_pull then waits for the scan job it
// enqueues to finish too, so the resulting dashcam_pulls row is actually
// linked to an indexed segment (PullCleanupCandidates requires that).
func pullAndScanForCleanup(t *testing.T, f *dashcamTestFixture) {
	t.Helper()
	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("pull job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	scanJobID := int64(res["scan_job_id"].(float64))
	if scanJobID == 0 {
		t.Fatal("expected a scan_job_id in the pull result")
	}
	scanFinal := waitForJobTerminal(t, f.queue, scanJobID, 10*time.Second)
	if scanFinal.State != jobs.StateDone {
		t.Fatalf("scan job state = %s, want done; error=%s", scanFinal.State, scanFinal.Error)
	}
}

func TestDashcamCleanupJob_DryRunThenApply(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_20260908_100000/segment_00001.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	f.start(t)
	pullAndScanForCleanup(t, f)

	// Dry run (default, dry_run omitted): must report candidates without
	// deleting anything off the fake.
	dryJob, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_cleanup (dry run): %v", err)
	}
	dryFinal := waitForJobTerminal(t, f.queue, dryJob.ID, 10*time.Second)
	if dryFinal.State != jobs.StateDone {
		t.Fatalf("dry run job state = %s, want done; error=%s", dryFinal.State, dryFinal.Error)
	}
	dryRes := decodeResult(t, dryFinal)
	if dryRes["candidates"].(float64) != 1 {
		t.Fatalf("dry run candidates = %v, want 1", dryRes["candidates"])
	}

	remotePath := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.mp4")
	if _, err := os.Stat(remotePath); err != nil {
		t.Fatalf("dry run must not delete the remote file: %v", err)
	}

	// Apply: dry_run:false must actually delete the remote copy and mark
	// the pull row deleted_remote_at.
	apply := false
	applyJob, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID, DryRun: &apply}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_cleanup (apply): %v", err)
	}
	applyFinal := waitForJobTerminal(t, f.queue, applyJob.ID, 10*time.Second)
	if applyFinal.State != jobs.StateDone {
		t.Fatalf("apply job state = %s, want done; error=%s", applyFinal.State, applyFinal.Error)
	}
	applyRes := decodeResult(t, applyFinal)
	if applyRes["deleted"].(float64) != 1 {
		t.Fatalf("apply deleted = %v, want 1", applyRes["deleted"])
	}
	if _, err := os.Stat(remotePath); !os.IsNotExist(err) {
		t.Errorf("expected remote file removed, stat err = %v", err)
	}

	pull, found, err := f.store.PullGet(context.Background(), f.dashcamSrcID, "front/2026/09/08/recording_20260908_100000/segment_00001.mp4")
	if err != nil {
		t.Fatalf("PullGet: %v", err)
	}
	if !found || pull.DeletedRemoteAt == nil {
		t.Fatalf("expected deleted_remote_at set, got found=%v pull=%+v", found, pull)
	}
}

func TestDashcamCleanupJob_HonoursSkippedOnChecksumMismatch(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_20260908_100000/segment_00001.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	f.start(t)
	pullAndScanForCleanup(t, f)

	// Corrupt the stored sha256 so the server-side checksum guard refuses
	// to delete — Delete's sha map only skips a path whose *current*
	// remote sha256 doesn't match what's passed, so poisoning our own
	// record of it (rather than the remote file) is enough to trigger
	// "checksum_mismatch" without touching the fake's storage directly.
	pull, found, err := f.store.PullGet(context.Background(), f.dashcamSrcID, "front/2026/09/08/recording_20260908_100000/segment_00001.mp4")
	if err != nil || !found {
		t.Fatalf("PullGet: found=%v err=%v", found, err)
	}
	if err := f.store.PullMarkVerified(context.Background(), pull.ID, "0000000000000000000000000000000000000000000000000000000000000000", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified: %v", err)
	}

	apply := false
	job, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID, DryRun: &apply}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	if res["deleted"].(float64) != 0 {
		t.Fatalf("deleted = %v, want 0 (checksum mismatch)", res["deleted"])
	}
	skipped, ok := res["skipped"].([]any)
	if !ok || len(skipped) != 1 {
		t.Fatalf("skipped = %v, want exactly one entry", res["skipped"])
	}
	entry := skipped[0].(map[string]any)
	if entry["reason"] != "checksum_mismatch" {
		t.Errorf("skipped reason = %v, want checksum_mismatch", entry["reason"])
	}

	remotePath := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.mp4")
	if _, err := os.Stat(remotePath); err != nil {
		t.Errorf("expected remote file to survive a skipped delete: %v", err)
	}
}

// -- dashcam_reprocess ---------------------------------------------------

func TestDashcamReprocessJob_PollsToDoneAndRePulls(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	f.fake.ReprocessTicks = 2

	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Camera: "front", Date: "2026-09-08",
	}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_reprocess: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}

	res := decodeResult(t, final)
	if res["segments_reprocessed"].(float64) != 1 {
		t.Fatalf("segments_reprocessed = %v, want 1", res["segments_reprocessed"])
	}
	if res["sidecars_refreshed"].(float64) != 3 {
		t.Fatalf("sidecars_refreshed = %v, want 3", res["sidecars_refreshed"])
	}

	// Lease must have been released once the job finished. Reuse the
	// fixture's own signing key to probe the fake's maintenance status
	// directly, rather than the dashcam.Client the job built internally.
	keyID, probePriv, ok, err := dashcam.LoadKey(context.Background(), f.store, "test-secret-key")
	if err != nil || !ok {
		t.Fatalf("LoadKey: ok=%v err=%v", ok, err)
	}
	probe := dashcam.New(f.fake.URL(), keyID, probePriv)
	status, err := probe.MaintenanceStatus(context.Background())
	if err != nil {
		t.Fatalf("MaintenanceStatus: %v", err)
	}
	if status.Active {
		t.Error("expected lease to be released after the reprocess job finished")
	}
}

func TestDashcamReprocessJob_RejectsEmptyScope(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed (no scope given)", final.State)
	}
}

func TestDashcamReprocessJob_CancelReleasesLease(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 200 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	// A large tick count so the job is still polling ("running") by the
	// time this test cancels it.
	f.fake.ReprocessTicks = 100

	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Camera: "front", Date: "2026-09-08",
	}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Wait until the job is actually running before cancelling it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, err := f.queue.Get(context.Background(), job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if j.State == jobs.StateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := f.queue.Cancel(context.Background(), job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateCancelled {
		t.Fatalf("job state = %s, want cancelled", final.State)
	}

	keyID, priv, ok, err := dashcam.LoadKey(context.Background(), f.store, "test-secret-key")
	if err != nil || !ok {
		t.Fatalf("LoadKey: ok=%v err=%v", ok, err)
	}
	probe := dashcam.New(f.fake.URL(), keyID, priv)

	// The lease is released asynchronously as part of the handler
	// unwinding after ctx cancellation; poll briefly rather than
	// asserting immediately.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := probe.MaintenanceStatus(context.Background())
		if err != nil {
			t.Fatalf("MaintenanceStatus: %v", err)
		}
		if !status.Active {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("expected lease to be released after cancelling the reprocess job")
}

// TestDashcamReprocessJob_TerminalErrorFailsJob is fix-round-1's covering
// test for the corrected poll loop: the real recorder's reprocess run
// lands on a *terminal* state='error' (with an Error message), never back
// on 'idle' — the job must recognize that state as failure (job error =
// status.Error) rather than looping forever waiting for an 'idle' that
// will never come.
func TestDashcamReprocessJob_TerminalErrorFailsJob(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	f.fake.ReprocessTicks = 2
	f.fake.ReprocessError = "boom: detector crashed"

	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Camera: "front", Date: "2026-09-08",
	}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed; result=%s", final.State, final.Result)
	}
	if !strings.Contains(final.Error, "boom: detector crashed") {
		t.Errorf("job error = %q, want it to contain the recorder's error message", final.Error)
	}

	// The lease must still have been released even though the run failed.
	keyID, priv, ok, err := dashcam.LoadKey(context.Background(), f.store, "test-secret-key")
	if err != nil || !ok {
		t.Fatalf("LoadKey: ok=%v err=%v", ok, err)
	}
	probe := dashcam.New(f.fake.URL(), keyID, priv)
	status, err := probe.MaintenanceStatus(context.Background())
	if err != nil {
		t.Fatalf("MaintenanceStatus: %v", err)
	}
	if status.Active {
		t.Error("expected lease to be released after a failed reprocess run")
	}
}

// TestDashcamPullJob_RedownloadsSegmentWhenSidecarMissingLocally is
// fix-round-1's covering test for the skip fast-path fix: a segment whose
// mp4 already verified at the same size must NOT be skipped if a sidecar
// the manifest currently lists for it (e.g. a .detections.json the
// recorder finished writing after the mp4 was already pulled) hasn't
// landed locally — the whole segment must be re-pulled so the late sidecar
// gets downloaded too.
func TestDashcamPullJob_RedownloadsSegmentWhenSidecarMissingLocally(t *testing.T) {
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_1/segment_1.mp4"
	// First pull: mp4 + timestamp only, no detections sidecar yet (as if
	// the recorder hasn't finished analysis).
	if err := f.fake.AddSegment("front", relPath, []byte("hello world"),
		"2026-09-08T10:00:00", "2026-09-08T10:00:30", true, false, false, false); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("1st job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	if res["pulled"].(float64) != 1 {
		t.Fatalf("1st run pulled = %v, want 1", res["pulled"])
	}

	detPath := filepath.Join(f.shareRoot, "front", "2026", "09", "08", "recording_1", "segment_1.detections.json")
	if _, err := os.Stat(detPath); !os.IsNotExist(err) {
		t.Fatalf("detections sidecar should not exist yet, stat err = %v", err)
	}

	// The recorder finishes analysis: the manifest now advertises a
	// detections sidecar for the *same* mp4 (same size, so the mp4 itself
	// wouldn't trip the existing bytes-mismatch re-download path).
	base := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_1", "segment_1")
	if err := os.WriteFile(base+".detections.json", []byte("{}"), 0o644); err != nil {
		t.Fatalf("write detections fixture: %v", err)
	}

	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID, Full: true}, 0)
	if err != nil {
		t.Fatalf("enqueue (2nd): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	res2 := decodeResult(t, final2)
	if res2["pulled"].(float64) != 1 {
		t.Fatalf("2nd run pulled = %v, want 1 (must not skip — sidecar missing locally)", res2["pulled"])
	}
	if res2["skipped"].(float64) != 0 {
		t.Fatalf("2nd run skipped = %v, want 0", res2["skipped"])
	}
	if _, err := os.Stat(detPath); err != nil {
		t.Errorf("expected detections sidecar to have been pulled: %v", err)
	}
}

// TestDashcamCleanupJob_EarlierBatchStaysMarkedWhenLaterBatchFails is
// fix-round-1's covering test for both the ctx-vs-Background fix and the
// per-batch (not end-of-run) PullMarkDeletedRemote fix: with the delete
// batch size shrunk to 1, a 2nd batch that fails outright must not cost the
// 1st batch's already-recorded deletion.
func TestDashcamCleanupJob_EarlierBatchStaysMarkedWhenLaterBatchFails(t *testing.T) {
	f := newDashcamTestFixture(t)
	relA := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	relB := "front/2026/09/08/recording_20260908_110000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relA, []byte("aaaaaaaaaa"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	mustAddSegment(t, f.fake, "front", relB, []byte("bbbbbbbbbb"), "2026-09-08T11:00:00", "2026-09-08T11:00:30")

	origBatch := dashcamDeleteBatchSize
	dashcamDeleteBatchSize = 1
	t.Cleanup(func() { dashcamDeleteBatchSize = origBatch })

	f.start(t)
	pullAndScanForCleanup(t, f)

	// Whichever of the two segments the manifest happens to page second
	// (candidates are ordered by dashcam_pulls.id, which follows manifest/
	// pull order — start_time ascending, so relA's batch runs first) is
	// forced to fail outright.
	f.fake.ForceDeleteErrorForPath(relB, 500, "boom")

	apply := false
	job, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID, DryRun: &apply}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed (2nd batch errors); result=%s", final.State, final.Result)
	}

	pullA, foundA, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relA)
	if err != nil || !foundA {
		t.Fatalf("PullGet(relA): found=%v err=%v", foundA, err)
	}
	if pullA.DeletedRemoteAt == nil {
		t.Error("expected the 1st (already-succeeded) batch's deletion to still be recorded despite the 2nd batch failing")
	}

	remoteA := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.mp4")
	if _, err := os.Stat(remoteA); !os.IsNotExist(err) {
		t.Errorf("expected the 1st segment's remote file to have actually been deleted, stat err = %v", err)
	}

	pullB, foundB, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relB)
	if err != nil || !foundB {
		t.Fatalf("PullGet(relB): found=%v err=%v", foundB, err)
	}
	if pullB.DeletedRemoteAt != nil {
		t.Error("expected the 2nd (failed) batch's segment to NOT be marked deleted")
	}
}
