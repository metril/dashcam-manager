package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dashcam/internal/db"
)

// newShareSource creates a kind=local share plus a kind=share source
// pointed at root, mirroring what a real local-storage scan job sees
// (cmd/api/jobs_runtime.go's scanJobHandler resolves a share-backed
// source's mount point before calling scan.Scan the same way).
func newShareSource(t *testing.T, store *db.DB, root string) (shareID, sourceID int64) {
	t.Helper()
	ctx := context.Background()
	shareID, err := store.ShareCreate(ctx, db.ShareCreateParams{
		Name: "local-test", Kind: db.KindLocal, MountPoint: root, Enabled: true,
	})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	sourceID, err = store.SourceCreate(ctx, "share", root, "local-test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx, `UPDATE sources SET share_id = ? WHERE id = ?`, shareID, sourceID); err != nil {
		t.Fatalf("set source share_id: %v", err)
	}
	return shareID, sourceID
}

func storageFileRelPaths(t *testing.T, store *db.DB, shareID int64) map[string]bool {
	t.Helper()
	rows, err := store.Read().QueryContext(context.Background(),
		`SELECT rel_path FROM storage_files WHERE share_id = ?`, shareID)
	if err != nil {
		t.Fatalf("query storage_files: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatalf("scan rel_path: %v", err)
		}
		out[rel] = true
	}
	return out
}

// TestScanUpsertsStorageFilesForShareBackedSource covers the walker's core
// contract: on a share/local-backed source, every regular file under the
// root — the segment mp4, its sidecar, and a non-segment file (an export
// living outside front/rear) — gets a storage_files row, not just what
// walkRoot's segment-shaped walk finds.
func TestScanUpsertsStorageFilesForShareBackedSource(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()

	frontStart := time.Date(2026, 2, 27, 0, 16, 54, 0, time.Local)
	frontKey := recKey(testDate, frontStart)
	mkSegment(t, root, "front", testDate, frontKey, 0, frontStart, frontStart.Add(30*time.Second), "", "")

	// An export-ish file sitting elsewhere on the storage, not under the
	// recorder's front/rear segment layout at all.
	exportDir := filepath.Join(root, "exports")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exportDir, "clip.mp4"), []byte("export bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	shareID, sourceID := newShareSource(t, store, root)

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	rels := storageFileRelPaths(t, store, shareID)
	wantMP4 := "front/2026/02/27/" + frontKey + "/segment_00000.mp4"
	wantSidecar := "front/2026/02/27/" + frontKey + "/segment_00000.timestamp.json"
	if !rels[wantMP4] {
		t.Fatalf("expected segment mp4 %q in storage_files, got %v", wantMP4, rels)
	}
	if !rels[wantSidecar] {
		t.Fatalf("expected sidecar %q in storage_files, got %v", wantSidecar, rels)
	}
	if !rels["exports/clip.mp4"] {
		t.Fatalf("expected non-segment export file in storage_files, got %v", rels)
	}
}

// TestScanSkipsProbeAndTmpFiles covers the skip list: a
// .dashcam-local-probe-* file (left behind by share.ProbeLocal — should
// never happen in practice since it removes its own file, but a walker
// touching a storage root must not index one if it does) and a hidden
// *.tmp file (the shape of internal/ingest/copy.go's in-progress-copy temp
// file) are never turned into storage_files rows.
func TestScanSkipsProbeAndTmpFiles(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, ".dashcam-local-probe-1234"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ingest-abc123.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	shareID, sourceID := newShareSource(t, store, root)
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	rels := storageFileRelPaths(t, store, shareID)
	if len(rels) != 1 || !rels["keep.txt"] {
		t.Fatalf("expected only keep.txt indexed, got %v", rels)
	}
}

// TestScanStorageFilesUpsertIsIdempotent covers re-scanning an unchanged
// tree: rel_path count and a manually-stamped sha256 both survive a second
// Scan call untouched.
func TestScanStorageFilesUpsertIsIdempotent(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	shareID, sourceID := newShareSource(t, store, root)
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan 1: %v", err)
	}

	var id int64
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id FROM storage_files WHERE share_id = ? AND rel_path = 'keep.txt'`, shareID).Scan(&id); err != nil {
		t.Fatalf("select id: %v", err)
	}
	if err := store.StorageFileSetSHA256(ctx, id, "deadbeef", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set sha256: %v", err)
	}

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan 2: %v", err)
	}

	var count int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_files WHERE share_id = ?`, shareID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after re-scan, got %d", count)
	}
	var sha *string
	if err := store.Read().QueryRowContext(ctx, `SELECT sha256 FROM storage_files WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatalf("select sha256: %v", err)
	}
	if sha == nil || *sha != "deadbeef" {
		t.Fatalf("expected sha256 to survive an unchanged re-scan, got %v", sha)
	}
}

// TestScanPrunesDeletedStorageFile covers the prune half: a file removed
// between two scans loses its storage_files row.
func TestScanPrunesDeletedStorageFile(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()

	keepPath := filepath.Join(root, "keep.txt")
	goingAwayPath := filepath.Join(root, "going-away.txt")
	if err := os.WriteFile(keepPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goingAwayPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	shareID, sourceID := newShareSource(t, store, root)
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan 1: %v", err)
	}
	rels := storageFileRelPaths(t, store, shareID)
	if len(rels) != 2 {
		t.Fatalf("expected 2 rows after first scan, got %v", rels)
	}

	if err := os.Remove(goingAwayPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan 2: %v", err)
	}
	rels = storageFileRelPaths(t, store, shareID)
	if len(rels) != 1 || !rels["keep.txt"] {
		t.Fatalf("expected only keep.txt after prune, got %v", rels)
	}
}

// TestScanSkipsStorageFilesForSSDSource covers the "never for kind=ssd"
// rule: an ssd-kind source (ShareID nil) gets no storage_files rows at
// all, even though it has real files on disk.
func TestScanSkipsStorageFilesForSSDSource(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()

	frontStart := time.Date(2026, 2, 27, 0, 16, 54, 0, time.Local)
	mkSegment(t, root, "front", testDate, recKey(testDate, frontStart), 0, frontStart, frontStart.Add(30*time.Second), "", "")

	sourceID, err := store.SourceCreate(ctx, "ssd", root, "ssd-test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var count int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_files`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 storage_files rows for an ssd source, got %d", count)
	}
}
