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

// setupTargetShare creates a share row pointing mount_point at a fresh
// temp dir and turns on allow_local_share_paths, so ResolveTargetMount
// accepts it without a real CIFS mount.
func setupTargetShare(t *testing.T, store *db.DB) (shareID int64, mount string) {
	t.Helper()
	mount = t.TempDir()
	id, err := store.ShareCreate(context.Background(), db.ShareCreateParams{
		Name: "test-share", Server: "nas.local", ShareName: "footage", MountPoint: mount, Enabled: true,
	})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	if err := store.SettingsSet(context.Background(), allowLocalSharePathsKey, []byte("true")); err != nil {
		t.Fatalf("SettingsSet: %v", err)
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

	stats, err := Cleanup(ctx, store, CleanupOpts{TripIDs: []int64{trips[0].ID}}, nil)
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

	stats, err := Cleanup(ctx, store, CleanupOpts{}, nil)
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

	stats, err := Cleanup(ctx, store, CleanupOpts{DryRun: true}, nil)
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
