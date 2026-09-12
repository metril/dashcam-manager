package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/scan"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "ingest-test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return d
}

// mkSegment writes a fake segment_NNNNN.mp4 plus a .timestamp.json sidecar
// under root, matching the recorder's real layout (see
// internal/scan/index_test.go's identically-named helper).
func mkSegment(t *testing.T, root, camera string, date time.Time, recKey string, seq int, start, end time.Time, contents string) {
	t.Helper()
	dir := filepath.Join(root, camera, date.Format("2006/01/02"), recKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, fmt.Sprintf("segment_%05d", seq))
	if err := os.WriteFile(base+".mp4", []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := fmt.Sprintf(`{"segment":"segment_%05d.mp4","camera":%q,"start_time":%q,"end_time":%q}`,
		seq, camera, start.Format("2006-01-02T15:04:05.000"), end.Format("2006-01-02T15:04:05.000"))
	if err := os.WriteFile(base+".timestamp.json", []byte(ts), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupSource builds a tiny two-segment source tree, scans it into store
// under a new "ssd" source, and returns the source id and root path.
func setupSource(t *testing.T, store *db.DB) (sourceID int64, root string) {
	t.Helper()
	root = t.TempDir()
	date := time.Date(2026, 9, 3, 19, 25, 28, 0, time.UTC)
	recKey := "recording_20260903_192528"
	mkSegment(t, root, "front", date, recKey, 0, date, date.Add(30*time.Second), "front-segment-0-bytes")
	mkSegment(t, root, "front", date, recKey, 1, date.Add(30*time.Second), date.Add(60*time.Second), "front-segment-1-bytes-longer")

	id, err := store.SourceCreate(context.Background(), "ssd", root, "test-ssd")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}
	if _, err := scan.Scan(context.Background(), store, id, root, scan.Opts{}); err != nil {
		t.Fatalf("scan.Scan: %v", err)
	}
	return id, root
}

// setupTargetShare creates a kind=local share row pointing mount_point at a
// fresh temp dir, so ResolveTargetMount accepts it via ProbeLocal without a
// real CIFS mount.
func setupTargetShare(t *testing.T, store *db.DB) (shareID int64, mount string) {
	t.Helper()
	mount = t.TempDir()
	id, err := store.ShareCreate(context.Background(), db.ShareCreateParams{
		Name: "test-share", Server: "nas.local", ShareName: "footage", MountPoint: mount, Enabled: true, Kind: db.KindLocal,
	})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	return id, mount
}

func TestRunFullIngestAndDedupeOnRerun(t *testing.T) {
	store := openTestDB(t)
	sourceID, _ := setupSource(t, store)
	shareID, mount := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}
	if target != mount {
		t.Fatalf("target = %q, want %q", target, mount)
	}

	stats, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.SegmentsTotal != 2 || stats.SegmentsCopied != 2 || stats.SegmentsFailed != 0 || stats.SegmentsSkipped != 0 {
		t.Fatalf("first run stats = %+v, want 2 copied, 0 skipped, 0 failed", stats)
	}
	if stats.BytesCopied == 0 {
		t.Fatalf("BytesCopied = 0, want > 0")
	}

	// Every destination file must exist with the expected content.
	entries, _ := store.IngestCandidates(ctx, sourceID, nil)
	for _, seg := range entries {
		destPath := filepath.Join(target, seg.RelPath)
		if _, err := os.Stat(destPath); err != nil {
			t.Fatalf("dest %s: %v", destPath, err)
		}
		row, err := store.IngestGet(ctx, seg.ID, shareID)
		if err != nil || row == nil || row.VerifiedAt == nil {
			t.Fatalf("segment %d: expected a verified ingests row, got %+v (err %v)", seg.ID, row, err)
		}
	}

	// Rerun: every segment should now dedupe-skip rather than recopy.
	stats2, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil)
	if err != nil {
		t.Fatalf("Run (rerun): %v", err)
	}
	if stats2.SegmentsSkipped != 2 || stats2.SegmentsCopied != 0 {
		t.Fatalf("rerun stats = %+v, want 2 skipped, 0 copied", stats2)
	}
}

func TestVerifyFile_DetectsCorruptedDestination(t *testing.T) {
	store := openTestDB(t)
	sourceID, _ := setupSource(t, store)
	shareID, _ := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}
	if _, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	seg := segs[0]
	row, err := store.IngestGet(ctx, seg.ID, shareID)
	if err != nil || row == nil {
		t.Fatalf("IngestGet: %v, %+v", err, row)
	}

	destPath := filepath.Join(target, seg.RelPath)
	if err := VerifyFile(destPath, row.Bytes, row.SHA256, VerifySHA256); err != nil {
		t.Fatalf("VerifyFile before corruption: %v", err)
	}

	// Flip one byte without changing the file's size.
	corruptByteInPlace(t, destPath)

	if err := VerifyFile(destPath, row.Bytes, row.SHA256, VerifySHA256); err == nil {
		t.Fatalf("VerifyFile after corruption: expected an error, got nil")
	}

	// A rerun of Run() should notice the mismatch (its dedupe check calls
	// VerifyFile against the stale verified row) and re-copy rather than
	// silently trusting the corrupted destination.
	stats, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil)
	if err != nil {
		t.Fatalf("Run (after corruption): %v", err)
	}
	if stats.SegmentsCopied == 0 {
		t.Fatalf("Run after corruption: SegmentsCopied = 0, want at least 1 (the corrupted segment should be re-copied)")
	}
	if err := VerifyFile(destPath, row.Bytes, row.SHA256, VerifySHA256); err != nil {
		t.Fatalf("VerifyFile after re-copy: %v", err)
	}
}

func corruptByteInPlace(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRefusesUnverified(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	ctx := context.Background()

	trips, _, err := store.TripsList(ctx, db.TripsFilter{})
	if err != nil {
		t.Fatalf("TripsList: %v", err)
	}
	if len(trips) == 0 {
		t.Fatalf("expected at least one trip from setupSource")
	}

	stats, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, TripIDs: []int64{trips[0].ID}}, nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if stats.SegmentsDeleted != 0 {
		t.Fatalf("SegmentsDeleted = %d, want 0 (nothing ingested/verified yet)", stats.SegmentsDeleted)
	}

	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	for _, seg := range segs {
		if _, err := os.Stat(filepath.Join(root, seg.RelPath)); err != nil {
			t.Fatalf("unverified segment file should still exist: %v", err)
		}
	}
}

func TestCleanupDeletesVerifiedAndEmptiesDirs(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	shareID, _ := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}
	if _, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	recDir := filepath.Dir(filepath.Join(root, segs[0].RelPath))

	stats, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID}, nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if stats.SegmentsDeleted != len(segs) {
		t.Fatalf("SegmentsDeleted = %d, want %d", stats.SegmentsDeleted, len(segs))
	}
	if stats.BytesDeleted == 0 {
		t.Fatalf("BytesDeleted = 0, want > 0")
	}
	if stats.DirsRemoved == 0 {
		t.Fatalf("DirsRemoved = 0, want > 0 (recording dir should now be empty)")
	}

	for _, seg := range segs {
		if _, err := os.Stat(filepath.Join(root, seg.RelPath)); !os.IsNotExist(err) {
			t.Fatalf("segment %s: expected file to be removed, stat err = %v", seg.RelPath, err)
		}
	}
	if _, err := os.Stat(recDir); !os.IsNotExist(err) {
		t.Fatalf("recording dir %s: expected it to be removed once empty, stat err = %v", recDir, err)
	}
}

// TestCleanupUsesSHA256WhenRecorded covers final-fix-wave item 6:
// Cleanup used to re-verify a destination file against size only, even
// when the original ingest was done with Verify: VerifySHA256 (the
// default) — a same-sized but corrupted/bit-rotted destination file would
// pass that check and Cleanup would delete the SSD's only good copy. It
// must use VerifySHA256 whenever the ingests row recorded a sha256.
func TestCleanupUsesSHA256WhenRecorded(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	shareID, mount := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}
	if _, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	candidates, err := store.CleanupCandidates(ctx, sourceID, nil, nil)
	if err != nil {
		t.Fatalf("CleanupCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatalf("expected at least one cleanup candidate")
	}
	if candidates[0].IngestSHA256 == "" {
		t.Fatalf("expected the ingest to have recorded a sha256 (Verify: VerifySHA256)")
	}

	// Corrupt every destination file in place, same size, different bytes,
	// so a size-only check would pass but a sha256 check must fail.
	for _, c := range candidates {
		destPath := filepath.Join(mount, c.DestRelPath)
		info, err := os.Stat(destPath)
		if err != nil {
			t.Fatalf("stat dest: %v", err)
		}
		corrupted := make([]byte, info.Size())
		for i := range corrupted {
			corrupted[i] = 0xFF
		}
		if err := os.WriteFile(destPath, corrupted, 0o644); err != nil {
			t.Fatalf("corrupt dest: %v", err)
		}
	}

	stats, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID}, nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if stats.SegmentsDeleted != 0 {
		t.Fatalf("SegmentsDeleted = %d, want 0 (corrupted destinations must not be trusted)", stats.SegmentsDeleted)
	}
	if stats.SegmentsSkipped != len(candidates) {
		t.Fatalf("SegmentsSkipped = %d, want %d", stats.SegmentsSkipped, len(candidates))
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(root, c.RelPath)); err != nil {
			t.Fatalf("SSD original %s should still exist (corrupted destination refused): %v", c.RelPath, err)
		}
	}
}

func TestCleanupDryRunTouchesNothing(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	shareID, _ := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}
	if _, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: true}, nil)
	if err != nil {
		t.Fatalf("Cleanup dry-run: %v", err)
	}
	if stats.SegmentsDeleted != 2 || len(stats.Items) != 2 {
		t.Fatalf("dry-run stats = %+v, want 2 previewed segments", stats)
	}
	if stats.BytesDeleted == 0 {
		t.Fatalf("dry-run BytesDeleted = 0, want > 0")
	}

	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	for _, seg := range segs {
		if _, err := os.Stat(filepath.Join(root, seg.RelPath)); err != nil {
			t.Fatalf("dry-run must not delete anything, but %s: %v", seg.RelPath, err)
		}
	}
}

// setupSourceWithGPSSidecar is like setupSource but its single segment
// also has a .gps.json sidecar, giving the sidecar-failure tests below
// something to copy/tamper with.
func setupSourceWithGPSSidecar(t *testing.T, store *db.DB) (sourceID int64, root, gpsRelPath string) {
	t.Helper()
	root = t.TempDir()
	date := time.Date(2026, 9, 3, 19, 25, 28, 0, time.UTC)
	recKey := "recording_20260903_192528"
	mkSegment(t, root, "front", date, recKey, 0, date, date.Add(30*time.Second), "front-segment-0-bytes")

	gpsRelPath = filepath.Join("front", date.Format("2006/01/02"), recKey, "segment_00000.gps.json")
	if err := os.WriteFile(filepath.Join(root, gpsRelPath), []byte(`{"points":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := store.SourceCreate(context.Background(), "ssd", root, "test-ssd")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}
	if _, err := scan.Scan(context.Background(), store, id, root, scan.Opts{}); err != nil {
		t.Fatalf("scan.Scan: %v", err)
	}
	return id, root, gpsRelPath
}

// TestIngestSidecarFailureFailsSegment covers M10 review finding #1: a
// sidecar copy failure must fail the whole segment (not just be logged),
// and the segment must not end up with a verified ingests row.
func TestIngestSidecarFailureFailsSegment(t *testing.T) {
	store := openTestDB(t)
	sourceID, _, gpsRelPath := setupSourceWithGPSSidecar(t, store)
	shareID, _ := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}

	// Force the sidecar copy to fail: pre-create a (non-empty) directory
	// at the destination path the gps sidecar would be copied to, so
	// streamCopy's rename-into-place fails (renaming a regular file onto
	// an existing directory is always an error).
	destGPSPath := filepath.Join(target, gpsRelPath)
	if err := os.MkdirAll(destGPSPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destGPSPath, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.SegmentsFailed != 1 || stats.SegmentsCopied != 0 {
		t.Fatalf("stats = %+v, want 1 failed, 0 copied (sidecar copy failure must fail the segment)", stats)
	}

	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	row, err := store.IngestGet(ctx, segs[0].ID, shareID)
	if err != nil {
		t.Fatalf("IngestGet: %v", err)
	}
	if row != nil && row.VerifiedAt != nil {
		t.Fatalf("segment must not be marked verified when its sidecar copy failed, got verified_at = %v", *row.VerifiedAt)
	}
}

// TestCleanupSidecarSizeMismatchNotDeleted covers M10 review finding #1's
// second half: removeSidecars must stat-verify each destination sidecar
// against the SSD original's size before deleting the original, leaving
// it in place if the destination is missing or a different size — even
// though the segment's video itself is still eligible for deletion.
func TestCleanupSidecarSizeMismatchNotDeleted(t *testing.T) {
	store := openTestDB(t)
	sourceID, root, gpsRelPath := setupSourceWithGPSSidecar(t, store)
	shareID, _ := setupTargetShare(t, store)
	ctx := context.Background()

	target, err := ResolveTargetMount(ctx, store, shareID)
	if err != nil {
		t.Fatalf("ResolveTargetMount: %v", err)
	}
	stats, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.SegmentsFailed != 0 || stats.SegmentsCopied != 1 {
		t.Fatalf("setup Run stats = %+v, want 1 copied, 0 failed", stats)
	}

	// Tamper with the destination sidecar's size after a fully successful
	// ingest (simulating a share-side edit/corruption after the fact).
	destGPSPath := filepath.Join(target, gpsRelPath)
	if err := os.WriteFile(destGPSPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	srcVideoPath := filepath.Join(root, segs[0].RelPath)
	srcGPSPath := filepath.Join(root, gpsRelPath)

	cstats, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID}, nil)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if cstats.SegmentsDeleted != 1 {
		t.Fatalf("SegmentsDeleted = %d, want 1 (the video itself still verifies)", cstats.SegmentsDeleted)
	}

	if _, err := os.Stat(srcVideoPath); !os.IsNotExist(err) {
		t.Fatalf("video should have been deleted, stat err = %v", err)
	}
	if _, err := os.Stat(srcGPSPath); err != nil {
		t.Fatalf("sidecar with a mismatched destination must NOT be deleted from the SSD: %v", err)
	}
}

func TestWriteProbeDetectsReadOnly(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProbe(dir); err != nil {
		t.Fatalf("WriteProbe on a writable dir: %v", err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o755) //nolint:errcheck // best-effort cleanup so t.TempDir() can remove it

	if err := WriteProbe(dir); err == nil {
		t.Fatalf("WriteProbe on a read-only dir: expected an error, got nil")
	}
}
