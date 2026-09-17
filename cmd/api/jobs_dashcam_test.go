package main

import (
	"context"
	"encoding/json"
	"net/url"
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
// decrypt against), a kind=local target share (so ResolveTargetMount
// accepts it via ProbeLocal without a real CIFS mount), a dashcam source
// pointed at a fresh dashcamtest.Fake, and a saved signing key the fake
// accepts. Workers are not started — call t.queue.Start yourself once any
// further fixture-specific stubbing (e.g. dashcamReprocessPollInterval) is
// in place.
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

	shareRoot := t.TempDir()
	shareID, shareSrcID, err := store.ShareCreateWithSource(ctx, db.ShareCreateParams{
		Name: "target", Server: "srv", ShareName: "footage", Enabled: true, Kind: db.KindLocal,
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

// mustAddSettlingSentinelSegment adds an extra, throwaway segment whose
// start_time is 2h after every other fixture segment already added —
// PullCleanupCandidates (final-fix-wave item 1, second pass) requires
// settled_at IS NOT NULL, and PullMarkSettled only settles a segment once
// the same pull run has observed the recorder produce something >=1h
// newer than it (see dashcamPullJobHandler's cutoff comment). A
// cleanup-job test whose only fixture segment(s) never get a newer
// sibling would never settle and PullCleanupCandidates would stay empty
// forever — this sentinel exists purely to give the pull run something
// newer to settle everything else against. Its own pull is never
// asserted on.
func mustAddSettlingSentinelSegment(t *testing.T, fake *dashcamtest.Fake, camera string) {
	t.Helper()
	if err := fake.AddSegment(camera, camera+"/2026/09/08/zz_sentinel/segment_sentinel.mp4",
		[]byte("sentinel"), "2026-09-08T23:00:00", "2026-09-08T23:00:30", true, true, true, false); err != nil {
		t.Fatalf("AddSegment (sentinel): %v", err)
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
	relPath := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relPath, []byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	mustAddSettlingSentinelSegment(t, f.fake, "front")
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

// TestDashcamCleanupJob_LocalMismatchWhenDetectionsSidecarMissing is
// final-fix-wave item 1c's covering test: a pull whose manifest listing
// reported remote_has_detections=true must not be offered for remote
// cleanup if its local .detections.json sidecar isn't actually present —
// otherwise deleting the remote copy would destroy the only copy of that
// sidecar, since it was never actually pulled down.
func TestDashcamCleanupJob_LocalMismatchWhenDetectionsSidecarMissing(t *testing.T) {
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relPath, []byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	mustAddSettlingSentinelSegment(t, f.fake, "front")
	f.start(t)
	pullAndScanForCleanup(t, f)

	pull, found, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relPath)
	if err != nil || !found {
		t.Fatalf("PullGet: found=%v err=%v", found, err)
	}
	if !pull.RemoteHasDetections {
		t.Fatal("expected RemoteHasDetections to be true (mustAddSegment writes a detections sidecar)")
	}

	// Simulate the local copy having gone missing (disk issue, manual
	// deletion, whatever) after the pull — the manifest still says
	// remote_has_detections=true (recorded on dashcam_pulls), but the
	// local share no longer actually has the sidecar.
	localDetPath := filepath.Join(f.shareRoot, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.detections.json")
	if err := os.Remove(localDetPath); err != nil {
		t.Fatalf("remove local detections sidecar: %v", err)
	}

	dryJob, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_cleanup: %v", err)
	}
	dryFinal := waitForJobTerminal(t, f.queue, dryJob.ID, 10*time.Second)
	if dryFinal.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", dryFinal.State, dryFinal.Error)
	}
	res := decodeResult(t, dryFinal)
	if res["candidates"].(float64) != 0 {
		t.Fatalf("candidates = %v, want 0 (missing local detections sidecar must exclude it)", res["candidates"])
	}
	if res["local_mismatch"].(float64) != 1 {
		t.Fatalf("local_mismatch = %v, want 1", res["local_mismatch"])
	}

	remotePath := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.mp4")
	if _, err := os.Stat(remotePath); err != nil {
		t.Errorf("remote file must survive when its sidecar is locally missing: %v", err)
	}
}

func TestDashcamCleanupJob_HonoursSkippedOnChecksumMismatch(t *testing.T) {
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relPath, []byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	mustAddSettlingSentinelSegment(t, f.fake, "front")
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

// TestDashcamReprocessJob_ReleasesLeaseBeforeRePullManifestWalk covers this
// fix directly: the lease must be released as soon as the recorder reaches
// its terminal state, *before* the re-pull phase's manifest walk starts —
// not after it, which on a real dashcam's ~39k-segment manifest was
// pausing recording on the car for minutes past when the actual reprocess
// work had already finished.
func TestDashcamReprocessJob_ReleasesLeaseBeforeRePullManifestWalk(t *testing.T) {
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

	releaseIdx, manifestIdx := -1, -1
	for i, r := range f.fake.Requests() {
		if releaseIdx == -1 && r.Method == "POST" && r.Path == "/api/ingest/maintenance/release" {
			releaseIdx = i
		}
		if manifestIdx == -1 && r.Method == "GET" && r.Path == "/api/ingest/manifest" {
			manifestIdx = i
		}
	}
	if releaseIdx == -1 {
		t.Fatal("no lease-release request recorded")
	}
	if manifestIdx == -1 {
		t.Fatal("no manifest request recorded")
	}
	if releaseIdx > manifestIdx {
		t.Errorf("lease released at request #%d, after re-pull's first manifest call at #%d; want released first", releaseIdx, manifestIdx)
	}
}

// TestDashcamReprocessJob_ScopesRePullManifestWalk covers this fix's other
// half: the re-pull phase's manifest walk must be scoped to the target
// camera and start near the target's own timestamp (derived from the
// recording directory name), rather than paging the recorder's entire
// manifest from the very beginning.
func TestDashcamReprocessJob_ScopesRePullManifestWalk(t *testing.T) {
	f := newDashcamTestFixture(t)
	relSegment := "front/2026/09/08/recording_20260908_100000/segment_1.mp4"
	mustAddSegment(t, f.fake, "front", relSegment,
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	f.fake.ReprocessTicks = 1

	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Segment: relSegment,
	}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_reprocess: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}

	var manifestQuery url.Values
	for _, r := range f.fake.Requests() {
		if r.Method == "GET" && r.Path == "/api/ingest/manifest" {
			manifestQuery, err = url.ParseQuery(r.RawQuery)
			if err != nil {
				t.Fatalf("parse manifest request query %q: %v", r.RawQuery, err)
			}
			break
		}
	}
	if manifestQuery == nil {
		t.Fatal("no manifest request recorded")
	}
	if got := manifestQuery.Get("camera"); got != "front" {
		t.Errorf("manifest request camera = %q, want %q (derived from the segment's path)", got, "front")
	}
	wantSince := "2026-09-08T09:59:00"
	if got := manifestQuery.Get("since"); got != wantSince {
		t.Errorf("manifest request since = %q, want %q (one minute before the recording dir's own start)", got, wantSince)
	}
}

func TestRecorderScopePath(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"manifest-relative segment", "front/2026/09/11/recording_1/segment_0.mp4", "/recordings/front/2026/09/11/recording_1/segment_0.mp4"},
		{"manifest-relative dir", "front/2026/09/11/recording_1", "/recordings/front/2026/09/11/recording_1"},
		{"already /recordings/", "/recordings/front/2026/09/11/recording_1/segment_0.mp4", "/recordings/front/2026/09/11/recording_1/segment_0.mp4"},
		{"already absolute, other root", "/some/other/path.mp4", "/some/other/path.mp4"},
		{"cleans a trailing slash", "front/2026/09/11/recording_1/", "/recordings/front/2026/09/11/recording_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recorderScopePath(tc.in); got != tc.want {
				t.Errorf("recorderScopePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDashcamReprocessScopeMatches_TolerantOfRecordingsPrefix(t *testing.T) {
	seg := dashcam.Segment{Path: "front/2026/09/11/recording_1/segment_0.mp4"}
	cases := []struct {
		name   string
		params dashcamReprocessJobParams
	}{
		{"manifest-relative path", dashcamReprocessJobParams{Path: "front/2026/09/11/recording_1/segment_0.mp4"}},
		{"/recordings/ form path", dashcamReprocessJobParams{Path: "/recordings/front/2026/09/11/recording_1/segment_0.mp4"}},
		{"manifest-relative segment", dashcamReprocessJobParams{Segment: "front/2026/09/11/recording_1/segment_0.mp4"}},
		{"/recordings/ form segment", dashcamReprocessJobParams{Segment: "/recordings/front/2026/09/11/recording_1/segment_0.mp4"}},
		{"/recordings/ form, basename only", dashcamReprocessJobParams{Segment: "segment_0.mp4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !dashcamReprocessScopeMatches(tc.params, seg) {
				t.Errorf("dashcamReprocessScopeMatches(%+v, %+v) = false, want true", tc.params, seg)
			}
		})
	}
}

func TestDashcamReprocessJob_SendsRecordingsFormPathToRecorder(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	f.fake.ReprocessTicks = 1

	f.start(t)

	relSegment := "front/2026/09/08/recording_1/segment_1.mp4"
	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Segment: relSegment,
	}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_reprocess: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}

	gotPath, gotSegment := f.fake.LastReprocessScope()
	if gotPath != "" {
		t.Errorf("LastReprocessScope path = %q, want empty (job only set segment)", gotPath)
	}
	wantSegment := "/recordings/" + relSegment
	if gotSegment != wantSegment {
		t.Errorf("LastReprocessScope segment = %q, want %q (recorder /recordings/ form)", gotSegment, wantSegment)
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

// TestDashcamReprocessJob_TransientStatusFailureToleratedThenRecovers is
// final-fix-wave item 3's covering test: a handful of consecutive
// ReprocessStatus poll failures (a network blip, a momentary 5xx) must
// not kill an otherwise-healthy reprocess run — the job keeps polling and
// still reaches "done" once the recorder starts answering again.
func TestDashcamReprocessJob_TransientStatusFailureToleratedThenRecovers(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	f.fake.ReprocessTicks = 2
	// Fewer than dashcamReprocessStatusMaxFailures (5) consecutive
	// failures, so the job must ride through them rather than giving up.
	f.fake.ForceReprocessStatusError(3, 503, "boom")

	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Camera: "front", Date: "2026-09-08",
	}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (transient status failures must be tolerated); error=%s", final.State, final.Error)
	}
}

// TestDashcamReprocessJob_TooManyConsecutiveStatusFailuresFailsJob is the
// other half: once ReprocessStatus fails dashcamReprocessStatusMaxFailures
// times in a row, the job must give up rather than polling forever.
func TestDashcamReprocessJob_TooManyConsecutiveStatusFailuresFailsJob(t *testing.T) {
	f := newDashcamTestFixture(t)
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")

	origPoll := dashcamReprocessPollInterval
	dashcamReprocessPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { dashcamReprocessPollInterval = origPoll })
	f.fake.ReprocessTicks = 100 // never legitimately reaches "done" on its own
	f.fake.ForceReprocessStatusError(dashcamReprocessStatusMaxFailures, 503, "boom")

	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_reprocess", dashcamReprocessJobParams{
		SourceID: f.dashcamSrcID, Camera: "front", Date: "2026-09-08",
	}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed after %d consecutive status failures; result=%s",
			final.State, dashcamReprocessStatusMaxFailures, final.Result)
	}

	// The lease must still have been released despite the give-up.
	keyID, priv, ok, err := dashcam.LoadKey(context.Background(), f.store, "test-secret-key")
	if err != nil || !ok {
		t.Fatalf("LoadKey: ok=%v err=%v", ok, err)
	}
	probe := dashcam.New(f.fake.URL(), keyID, priv)
	deadline := time.Now().Add(5 * time.Second)
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
	t.Error("expected lease to be released after giving up on consecutive status failures")
}

// TestDashcamPullJob_RedownloadsSegmentWhenSidecarMissingLocally is
// fix-round-1's covering test for the skip fast-path fix: a segment whose
// mp4 already verified at the same size must NOT be skipped if a sidecar
// the manifest currently lists for it (e.g. a .detections.json the
// recorder finished writing after the mp4 was already pulled) hasn't
// landed locally — the whole segment must be re-pulled so the late sidecar
// gets downloaded too.
// TestDashcamPullJob_SidecarOnlyFastPathWhenSidecarMissingLocally covers
// the sidecar-only fast path: a verified mp4 whose manifest now lists a
// sidecar not yet present locally (e.g. the recorder finished
// .detections.json analysis after the mp4 already pulled) must fetch only
// that sidecar, not re-stream the whole mp4 — counted as "skipped", not
// "pulled", and the mp4's own file-route request count must not grow on
// the 2nd run.
func TestDashcamPullJob_SidecarOnlyFastPathWhenSidecarMissingLocally(t *testing.T) {
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

	mp4Requests := func() int {
		n := 0
		for _, r := range f.fake.Requests() {
			if r.Path == "/api/ingest/file" && strings.Contains(r.RawQuery, url.QueryEscape(relPath)) {
				n++
			}
		}
		return n
	}
	mp4ReqsBefore := mp4Requests()

	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID, Full: true}, 0)
	if err != nil {
		t.Fatalf("enqueue (2nd): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	res2 := decodeResult(t, final2)
	if res2["pulled"].(float64) != 0 {
		t.Fatalf("2nd run pulled = %v, want 0 (sidecar-only fast path, not a full re-download)", res2["pulled"])
	}
	if res2["skipped"].(float64) != 1 {
		t.Fatalf("2nd run skipped = %v, want 1", res2["skipped"])
	}
	if _, err := os.Stat(detPath); err != nil {
		t.Errorf("expected detections sidecar to have been pulled: %v", err)
	}
	if got := mp4Requests(); got != mp4ReqsBefore {
		t.Errorf("mp4 file-route requests after 2nd run = %d, want unchanged from %d (must not re-transfer the mp4)", got, mp4ReqsBefore)
	}
}

// TestDashcamPullJob_IncrementalRewindCatchesLateSidecar is final-fix-wave
// item 1's CRITICAL covering test: a segment pulled before the recorder
// finished writing its .detections.json sidecar advances the persisted
// pull_cursor to exactly its own start_time. A later INCREMENTAL pull
// (no Full) — without the 24h rewind this fix adds — would resume
// exactly at that cursor, and the recorder's manifest filter
// (start_time > since, strict) would exclude that segment from the
// listing entirely, so the pull job would never even consider it again,
// let alone notice the now-available sidecar; the segment (and its
// late sidecar) could then be cleaned up off the dashcam having never
// actually been fully pulled. Rewinding the cursor by 24h before listing
// re-includes the segment so the existing sidecar-presence re-check
// (segmentSidecarsPresent, covered by
// TestDashcamPullJob_SidecarOnlyFastPathWhenSidecarMissingLocally) gets a
// chance to run at all.
func TestDashcamPullJob_IncrementalRewindCatchesLateSidecar(t *testing.T) {
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_1/segment_1.mp4"
	// hasDetections=false: as if the recorder hadn't finished analysis
	// yet when this segment was first listed/pulled.
	if err := f.fake.AddSegment("front", relPath, []byte("hello world"),
		"2026-09-08T10:00:00", "2026-09-08T10:00:30", true, false, false, false); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue (1st): %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("1st job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	if res["pulled"].(float64) != 1 {
		t.Fatalf("1st run pulled = %v, want 1", res["pulled"])
	}

	src, err := f.store.DashcamGet(context.Background(), f.dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGet: %v", err)
	}
	if src.PullCursor == nil || *src.PullCursor != "2026-09-08T10:00:00" {
		t.Fatalf("pull_cursor = %v, want exactly the segment's own start_time (2026-09-08T10:00:00)", src.PullCursor)
	}

	// The recorder finishes analysis: the sidecar file lands on the
	// dashcam's disk. dashcamtest's manifest computes each segment's
	// `sidecars` map live from file presence (not from AddSegment's
	// original hasDetections flag), matching the recorder's own
	// has_detections-flips-after-analysis behavior (resources/ingest.py).
	base := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_1", "segment_1")
	if err := os.WriteFile(base+".detections.json", []byte("{}"), 0o644); err != nil {
		t.Fatalf("write detections fixture: %v", err)
	}

	// An INCREMENTAL pull (Full not set) — this is the case that was
	// broken before the rewind fix.
	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue (2nd, incremental): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	res2 := decodeResult(t, final2)
	// The rewind must re-list the segment at all (otherwise it's neither
	// pulled nor skipped) — once re-listed, the sidecar-only fast path
	// (segmentSidecarsPresent false) fetches just the sidecar, so this
	// counts as "skipped", not "pulled".
	if res2["pulled"].(float64) != 0 {
		t.Fatalf("2nd (incremental) run pulled = %v, want 0 (sidecar-only fast path)", res2["pulled"])
	}
	if res2["skipped"].(float64) != 1 {
		t.Fatalf("2nd (incremental) run skipped = %v, want 1 — the 24h rewind must re-list a segment sitting exactly at the persisted cursor", res2["skipped"])
	}

	detPath := filepath.Join(f.shareRoot, "front", "2026", "09", "08", "recording_1", "segment_1.detections.json")
	if _, err := os.Stat(detPath); err != nil {
		t.Errorf("expected the late detections sidecar to have been pulled locally: %v", err)
	}

	pull, found, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relPath)
	if err != nil || !found {
		t.Fatalf("PullGet: found=%v err=%v", found, err)
	}
	if !pull.RemoteHasDetections {
		t.Error("expected RemoteHasDetections to be stamped true after the incremental re-pull observed the sidecar")
	}
	if pull.LastSeenAt == nil {
		t.Error("expected LastSeenAt to be stamped on the re-pull")
	}
}

// TestDashcamPullJob_UnparseableCursorFallsBackToFullListing covers
// rewindCursorFor24hLateSidecars' "unparseable cursor -> full listing"
// fallback via the job itself: a cursor that doesn't parse as a
// recognizable start_time must not make the incremental pull silently
// list nothing (or crash) — it must behave like Full:true instead.
func TestDashcamPullJob_UnparseableCursorFallsBackToFullListing(t *testing.T) {
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_1/segment_1.mp4"
	mustAddSegment(t, f.fake, "front", relPath, []byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	f.start(t)

	if err := f.store.DashcamSetCursor(context.Background(), f.dashcamSrcID, "not-a-timestamp", time.Now()); err != nil {
		t.Fatalf("DashcamSetCursor: %v", err)
	}

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	if res["pulled"].(float64) != 1 {
		t.Fatalf("pulled = %v, want 1 (unparseable cursor must fall back to a full listing)", res["pulled"])
	}
}

// TestDashcamCleanupJob_IncrementalPullSettlesOlderNotNewest is
// final-fix-wave item 1's (settle-based redesign) covering test: an
// INCREMENTAL pull (non-empty persisted cursor, going through the 24h
// rewind) that observes a newer segment must settle — and so make
// cleanup-eligible — every older segment at least 1h behind it, while
// leaving the newest segment itself unsettled.
func TestDashcamCleanupJob_IncrementalPullSettlesOlderNotNewest(t *testing.T) {
	f := newDashcamTestFixture(t)
	relA := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	relB := "front/2026/09/08/recording_20260908_113000/segment_00001.mp4"
	relC := "front/2026/09/08/recording_20260908_130000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relA, []byte("aaaa"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	f.start(t)

	// 1st pull: full listing (no cursor yet) — only segment A exists, and
	// nothing is newer than it, so it does not settle yet.
	job1, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull (1st): %v", err)
	}
	final1 := waitForJobTerminal(t, f.queue, job1.ID, 10*time.Second)
	if final1.State != jobs.StateDone {
		t.Fatalf("1st pull job state = %s, want done; error=%s", final1.State, final1.Error)
	}
	res1 := decodeResult(t, final1)
	if res1["pulled"].(float64) != 1 {
		t.Fatalf("1st run pulled = %v, want 1", res1["pulled"])
	}
	scan1 := int64(res1["scan_job_id"].(float64))
	if scanFinal := waitForJobTerminal(t, f.queue, scan1, 10*time.Second); scanFinal.State != jobs.StateDone {
		t.Fatalf("1st scan job state = %s, want done; error=%s", scanFinal.State, scanFinal.Error)
	}

	src, err := f.store.DashcamGet(context.Background(), f.dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGet: %v", err)
	}
	if src.PullCursor == nil || *src.PullCursor == "" {
		t.Fatal("expected a non-empty pull_cursor before the 2nd (incremental) pull")
	}

	// The recorder records two more segments — B 1.5h after A, C 3h after
	// A (so C is >=1h newer than both A and B, but B is only 1.5h newer
	// than A — still >=1h, so B should settle too).
	mustAddSegment(t, f.fake, "front", relB, []byte("bbbb"), "2026-09-08T11:30:00", "2026-09-08T12:00:00")
	mustAddSegment(t, f.fake, "front", relC, []byte("cccc"), "2026-09-08T13:00:00", "2026-09-08T13:00:30")

	// 2nd pull: INCREMENTAL (Full not set) — src.PullCursor is non-empty,
	// so this exercises rewindCursorFor24hLateSidecars' rewind path.
	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull (2nd, incremental): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd pull job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	res2 := decodeResult(t, final2)
	scan2 := int64(res2["scan_job_id"].(float64))
	if scanFinal := waitForJobTerminal(t, f.queue, scan2, 10*time.Second); scanFinal.State != jobs.StateDone {
		t.Fatalf("2nd scan job state = %s, want done; error=%s", scanFinal.State, scanFinal.Error)
	}

	pullA, foundA, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relA)
	if err != nil || !foundA {
		t.Fatalf("PullGet A: found=%v err=%v", foundA, err)
	}
	if pullA.SettledAt == nil {
		t.Error("expected segment A (3h older than the newest observed segment) to be settled")
	}
	pullB, foundB, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relB)
	if err != nil || !foundB {
		t.Fatalf("PullGet B: found=%v err=%v", foundB, err)
	}
	if pullB.SettledAt == nil {
		t.Error("expected segment B (1.5h older than the newest observed segment) to be settled")
	}
	pullC, foundC, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relC)
	if err != nil || !foundC {
		t.Fatalf("PullGet C: found=%v err=%v", foundC, err)
	}
	if pullC.SettledAt != nil {
		t.Error("expected the newest segment C to NOT be settled (nothing newer than it has been observed)")
	}

	// dashcam_cleanup dry run: A and B are candidates, C is not.
	dryJob, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_cleanup: %v", err)
	}
	dryFinal := waitForJobTerminal(t, f.queue, dryJob.ID, 10*time.Second)
	if dryFinal.State != jobs.StateDone {
		t.Fatalf("cleanup job state = %s, want done; error=%s", dryFinal.State, dryFinal.Error)
	}
	dryRes := decodeResult(t, dryFinal)
	if dryRes["candidates"].(float64) != 2 {
		t.Fatalf("cleanup candidates = %v, want 2 (A and B, not C)", dryRes["candidates"])
	}
}

// TestDashcamCleanupJob_LateSidecarSettlesAfterNewerSegmentObserved
// combines the "late sidecar" scenario (a segment listed with
// has_detections=0 before the recorder finishes analysis) with the
// settle-based cleanup guard: the segment must not be a cleanup candidate
// until BOTH its sidecar has actually been pulled down AND the recorder
// has produced something newer than it, observed in the same pull run
// that also caught the late sidecar.
func TestDashcamCleanupJob_LateSidecarSettlesAfterNewerSegmentObserved(t *testing.T) {
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	// hasDetections=false: recorder hasn't finished analysis yet.
	if err := f.fake.AddSegment("front", relPath, []byte("hello world"),
		"2026-09-08T10:00:00", "2026-09-08T10:00:30", true, false, false, false); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}
	f.start(t)

	job1, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull (1st): %v", err)
	}
	final1 := waitForJobTerminal(t, f.queue, job1.ID, 10*time.Second)
	if final1.State != jobs.StateDone {
		t.Fatalf("1st pull job state = %s, want done; error=%s", final1.State, final1.Error)
	}

	detPath := filepath.Join(f.shareRoot, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.detections.json")
	if _, err := os.Stat(detPath); !os.IsNotExist(err) {
		t.Fatalf("detections sidecar should not exist yet, stat err = %v", err)
	}

	// The recorder finishes analysis on segment_1 AND records a new
	// segment 2h later.
	base := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001")
	if err := os.WriteFile(base+".detections.json", []byte("{}"), 0o644); err != nil {
		t.Fatalf("write detections fixture: %v", err)
	}
	relNewer := "front/2026/09/08/recording_20260908_120000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relNewer, []byte("newer"), "2026-09-08T12:00:00", "2026-09-08T12:00:30")

	// 2nd pull: INCREMENTAL — the 24h rewind re-lists segment_1 (catching
	// the late sidecar via the sidecar-only fast path, per
	// TestDashcamPullJob_IncrementalRewindCatchesLateSidecar) and also
	// observes segment_2, which is >=1h newer, fully pulled, and settles
	// segment_1.
	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_pull (2nd, incremental): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd pull job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	res2 := decodeResult(t, final2)
	if res2["pulled"].(float64) != 1 {
		t.Fatalf("2nd (incremental) run pulled = %v, want 1 (segment_2 fresh; segment_1 is sidecar-only)", res2["pulled"])
	}
	if res2["skipped"].(float64) != 1 {
		t.Fatalf("2nd (incremental) run skipped = %v, want 1 (segment_1's sidecar-only fast path)", res2["skipped"])
	}
	scan2 := int64(res2["scan_job_id"].(float64))
	if scanFinal := waitForJobTerminal(t, f.queue, scan2, 10*time.Second); scanFinal.State != jobs.StateDone {
		t.Fatalf("2nd scan job state = %s, want done; error=%s", scanFinal.State, scanFinal.Error)
	}

	if _, err := os.Stat(detPath); err != nil {
		t.Errorf("expected the late detections sidecar to have been pulled locally: %v", err)
	}

	pull1, found1, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relPath)
	if err != nil || !found1 {
		t.Fatalf("PullGet segment_1: found=%v err=%v", found1, err)
	}
	if !pull1.RemoteHasDetections {
		t.Error("expected RemoteHasDetections true after the late sidecar was observed")
	}
	if pull1.SettledAt == nil {
		t.Error("expected segment_1 to be settled once a newer segment was observed in the same run")
	}

	pullNewer, foundNewer, err := f.store.PullGet(context.Background(), f.dashcamSrcID, relNewer)
	if err != nil || !foundNewer {
		t.Fatalf("PullGet segment_2: found=%v err=%v", foundNewer, err)
	}
	if pullNewer.SettledAt != nil {
		t.Error("expected segment_2 (the newest observed) to NOT be settled yet")
	}

	dryJob, err := f.queue.Enqueue(context.Background(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue dashcam_cleanup: %v", err)
	}
	dryFinal := waitForJobTerminal(t, f.queue, dryJob.ID, 10*time.Second)
	if dryFinal.State != jobs.StateDone {
		t.Fatalf("cleanup job state = %s, want done; error=%s", dryFinal.State, dryFinal.Error)
	}
	dryRes := decodeResult(t, dryFinal)
	if dryRes["candidates"].(float64) != 1 {
		t.Fatalf("cleanup candidates = %v, want 1 (segment_1 only)", dryRes["candidates"])
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
	mustAddSettlingSentinelSegment(t, f.fake, "front")

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

// -- dashcam_pull: vehicle trips/events sub-step --------------------------

func vehicleTripIDColumn(t *testing.T, store *db.DB, eventID int64) any {
	t.Helper()
	var v any
	if err := store.Read().QueryRowContext(context.Background(),
		`SELECT vehicle_trip_id FROM vehicle_events WHERE id = ?`, eventID).Scan(&v); err != nil {
		t.Fatalf("query vehicle_trip_id: %v", err)
	}
	return v
}

// TestDashcamPullJob_VehicleDataCursorAdvance covers the pull job's
// vehicle-trips/vehicle-events sub-step: trips/events served by the fake
// are upserted and counted in the job result, and vehicle_trips_cursor/
// vehicle_events_cursor persist the last id seen — advancing on a run
// that saw new rows, and NOT regressing on a later run that saw none.
func TestDashcamPullJob_VehicleDataCursorAdvance(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.fake.VehicleTrips = []dashcamtest.VehicleTrip{
		{ID: 1, StartTime: "2026-09-08T09:00:00", EndTime: "2026-09-08T09:30:00"},
		{ID: 2, StartTime: "2026-09-08T10:00:00", EndTime: "2026-09-08T10:30:00"},
	}
	f.fake.VehicleEvents = []dashcamtest.VehicleEvent{
		{ID: 1, EventType: "hard_brake", WallTime: "2026-09-08T09:10:00"},
	}
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	if res["vehicle_trips"].(float64) != 2 {
		t.Errorf("vehicle_trips = %v, want 2", res["vehicle_trips"])
	}
	if res["vehicle_events"].(float64) != 1 {
		t.Errorf("vehicle_events = %v, want 1", res["vehicle_events"])
	}

	tripsCursor, eventsCursor, err := f.store.DashcamGetVehicleCursors(context.Background(), f.dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGetVehicleCursors: %v", err)
	}
	if tripsCursor == nil || *tripsCursor != 2 {
		t.Fatalf("vehicle_trips_cursor = %v, want 2", tripsCursor)
	}
	if eventsCursor == nil || *eventsCursor != 1 {
		t.Fatalf("vehicle_events_cursor = %v, want 1", eventsCursor)
	}

	// 2nd run: no *new* trips/events served by the fake, but with only 2
	// trips/1 event total the cursor-20/cursor-500 tail rewind re-fetches
	// everything from the start again (since_id clamps to 0) — that's
	// expected (idempotent re-upsert of the same rows), so what matters
	// is the persisted cursor doesn't regress below what the 1st run saw.
	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID, Full: true}, 0)
	if err != nil {
		t.Fatalf("enqueue (2nd): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd job state = %s, want done; error=%s", final2.State, final2.Error)
	}
	var cnt int
	if err := f.store.Read().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM vehicle_trips`).Scan(&cnt); err != nil {
		t.Fatalf("count vehicle_trips: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("vehicle_trips count after 2nd run = %d, want 2 (re-upsert, not duplicated)", cnt)
	}
	tripsCursor2, eventsCursor2, err := f.store.DashcamGetVehicleCursors(context.Background(), f.dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGetVehicleCursors (2nd): %v", err)
	}
	if tripsCursor2 == nil || *tripsCursor2 != 2 || eventsCursor2 == nil || *eventsCursor2 != 1 {
		t.Fatalf("cursors after 2nd run = %v/%v, want unchanged 2/1", tripsCursor2, eventsCursor2)
	}
}

// TestDashcamPullJob_VehicleTailReFetchRelinksTripID covers the tail
// re-fetch (since_id = cursor-500 for events, cursor-20 for trips): an
// event pulled before the recorder knew its trip_id gets that trip_id on
// a later re-fetch, and the pull job resolves vehicle_events.
// vehicle_trip_id against the by-then-pulled vehicle_trips row — the
// "previously NULL trip_id gets linked" case.
func TestDashcamPullJob_VehicleTailReFetchRelinksTripID(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.fake.VehicleEvents = []dashcamtest.VehicleEvent{
		{ID: 1, EventType: "stall", WallTime: "2026-09-08T09:10:00"}, // no TripID yet
	}
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue (1st): %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("1st job state = %s, want done; error=%s", final.State, final.Error)
	}

	var eventID int64
	if err := f.store.Read().QueryRowContext(context.Background(),
		`SELECT id FROM vehicle_events WHERE dashcam_source_id = ? AND remote_event_id = '1'`, f.dashcamSrcID,
	).Scan(&eventID); err != nil {
		t.Fatalf("query event id: %v", err)
	}
	if got := vehicleTripIDColumn(t, f.store, eventID); got != nil {
		t.Fatalf("vehicle_trip_id before recorder knows the trip = %v, want nil", got)
	}

	// The recorder now knows the trip: it appears in the trips list, and
	// the event is updated to reference it.
	remoteTripID := int64(5)
	f.fake.VehicleTrips = []dashcamtest.VehicleTrip{
		{ID: remoteTripID, StartTime: "2026-09-08T09:00:00", EndTime: "2026-09-08T09:30:00"},
	}
	f.fake.VehicleEvents[0].TripID = &remoteTripID

	job2, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue (2nd): %v", err)
	}
	final2 := waitForJobTerminal(t, f.queue, job2.ID, 10*time.Second)
	if final2.State != jobs.StateDone {
		t.Fatalf("2nd job state = %s, want done; error=%s", final2.State, final2.Error)
	}

	var vehicleTripPK int64
	if err := f.store.Read().QueryRowContext(context.Background(),
		`SELECT id FROM vehicle_trips WHERE dashcam_source_id = ? AND remote_trip_id = '5'`, f.dashcamSrcID,
	).Scan(&vehicleTripPK); err != nil {
		t.Fatalf("query vehicle_trips id: %v", err)
	}
	got := vehicleTripIDColumn(t, f.store, eventID)
	gotID, ok := got.(int64)
	if !ok || gotID != vehicleTripPK {
		t.Fatalf("vehicle_trip_id after tail re-fetch = %v, want %d", got, vehicleTripPK)
	}
}

// TestDashcamPullJob_VehicleRoutesDisabledStillSucceeds covers an older
// recorder firmware predating the vehicle-telemetry routes (all three
// 404): the pull job must still succeed (files still pulled/scanned),
// just with vehicle_trips/vehicle_events both 0 and cursors left unset.
func TestDashcamPullJob_VehicleRoutesDisabledStillSucceeds(t *testing.T) {
	f := newDashcamTestFixture(t)
	f.fake.VehicleRoutesDisabled = true
	mustAddSegment(t, f.fake, "front", "front/2026/09/08/recording_1/segment_1.mp4",
		[]byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)
	if res["pulled"].(float64) != 1 {
		t.Errorf("pulled = %v, want 1 (vehicle routes being 404 must not affect file pulls)", res["pulled"])
	}
	if res["vehicle_trips"].(float64) != 0 || res["vehicle_events"].(float64) != 0 {
		t.Errorf("vehicle_trips/events = %v/%v, want 0/0", res["vehicle_trips"], res["vehicle_events"])
	}

	tripsCursor, eventsCursor, err := f.store.DashcamGetVehicleCursors(context.Background(), f.dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGetVehicleCursors: %v", err)
	}
	if tripsCursor != nil || eventsCursor != nil {
		t.Errorf("cursors = %v/%v, want nil/nil (404 tolerated, not treated as progress)", tripsCursor, eventsCursor)
	}
}

// TestDashcamPullJob_ScanCompletionLinksVehicleEvents covers the scan
// job's own VehicleEventsLink call (jobs_runtime.go's scanJobHandler):
// once the scan a dashcam_pull enqueues actually indexes the segment a
// vehicle event's wall_time falls in, the event must end up linked to it
// — whether that happens via dashcam_pull's own linking pass or (as here,
// since with several free workers the enqueued scan can easily finish
// before the pull job's own PullsLinkSegments call would next get a
// chance to run) via the scan job's own call.
func TestDashcamPullJob_ScanCompletionLinksVehicleEvents(t *testing.T) {
	t.Setenv("TZ", "UTC")
	f := newDashcamTestFixture(t)
	relPath := "front/2026/09/08/recording_20260908_100000/segment_00001.mp4"
	mustAddSegment(t, f.fake, "front", relPath, []byte("hello world"), "2026-09-08T10:00:00", "2026-09-08T10:00:30")
	// AddSegment's own .timestamp.json fixture is a placeholder "{}" (real
	// start/end only matter to the manifest) — the scanner reads the
	// sidecar itself for segments.start_time/end_time, falling back to an
	// mtime heuristic when it doesn't parse. Give it real content so the
	// resulting segment's [start_time, end_time) actually contains the
	// vehicle event's wall_time below.
	tsPath := filepath.Join(f.fake.Dir, "front", "2026", "09", "08", "recording_20260908_100000", "segment_00001.timestamp.json")
	if err := os.WriteFile(tsPath, []byte(`{"segment":"segment_00001","camera":"front","start_time":"2026-09-08T10:00:00","end_time":"2026-09-08T10:00:30"}`), 0o644); err != nil {
		t.Fatalf("write timestamp fixture: %v", err)
	}
	f.fake.VehicleEvents = []dashcamtest.VehicleEvent{
		{ID: 1, EventType: "hard_brake", WallTime: "2026-09-08T10:00:10"},
	}
	f.start(t)

	job, err := f.queue.Enqueue(context.Background(), "dashcam_pull", dashcamPullJobParams{SourceID: f.dashcamSrcID}, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	final := waitForJobTerminal(t, f.queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("pull job state = %s, want done; error=%s", final.State, final.Error)
	}
	res := decodeResult(t, final)

	var eventID int64
	if err := f.store.Read().QueryRowContext(context.Background(),
		`SELECT id FROM vehicle_events WHERE dashcam_source_id = ? AND remote_event_id = '1'`, f.dashcamSrcID,
	).Scan(&eventID); err != nil {
		t.Fatalf("query event id: %v", err)
	}
	scanJobID := int64(res["scan_job_id"].(float64))
	scanFinal := waitForJobTerminal(t, f.queue, scanJobID, 10*time.Second)
	if scanFinal.State != jobs.StateDone {
		t.Fatalf("scan job state = %s, want done; error=%s", scanFinal.State, scanFinal.Error)
	}

	var segID any
	if err := f.store.Read().QueryRowContext(context.Background(),
		`SELECT segment_id FROM vehicle_events WHERE id = ?`, eventID).Scan(&segID); err != nil {
		t.Fatalf("query segment_id after scan: %v", err)
	}
	if segID == nil {
		t.Fatal("segment_id after scan = nil, want linked")
	}
}
