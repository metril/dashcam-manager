package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// sha256Hex is a small test helper matching ingest.HashFile's output for a
// known byte slice, so tests can assert the exact hash the job stamped.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// newLocalShareWithFiles creates a kind=local share rooted at a fresh temp
// dir, writes each of files (relPath -> content) into it, and upserts a
// matching storage_files row for each (bytes/mtime derived from the real
// file) — the same state a real scan (task 5) would have produced.
func newLocalShareWithFiles(t *testing.T, store *db.DB, files map[string]string) (shareID int64, root string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	var err error
	shareID, err = store.ShareCreate(ctx, db.ShareCreateParams{Name: "hash-test", Kind: db.KindLocal, MountPoint: root, Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	var rows []db.StorageFileUpsertRow
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		info, err := os.Stat(full)
		if err != nil {
			t.Fatalf("stat %s: %v", full, err)
		}
		rows = append(rows, db.StorageFileUpsertRow{RelPath: rel, Bytes: info.Size(), Mtime: info.ModTime().Unix()})
	}
	if err := store.StorageFileUpsert(ctx, shareID, rows, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("StorageFileUpsert: %v", err)
	}
	return shareID, root
}

// runHashBackfill invokes hashBackfillJobHandler directly (bypassing the
// job queue) so tests control ctx/params precisely and can inspect the
// returned result without racing a real worker.
func runHashBackfill(t *testing.T, ctx context.Context, store *db.DB, params hashBackfillJobParams) (hashBackfillResult, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	// hashBackfillJobHandler enqueues a duplicates_refresh job on
	// completion (fix round 1) — a bare queue with no workers started is
	// enough for that Enqueue call to succeed; nothing needs to actually
	// process it for these tests.
	q := jobs.New(store)
	h := hashBackfillJobHandler(store, q)
	result, err := h(ctx, &jobs.Job{Params: raw}, func(float64, string) {})
	if err != nil {
		return hashBackfillResult{}, err
	}
	res, ok := result.(hashBackfillResult)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	return res, nil
}

// storageFileSHA256 reads back one storage_files row's sha256 (nil if
// still unhashed) for assertions.
func storageFileSHA256(t *testing.T, store *db.DB, shareID int64, relPath string) *string {
	t.Helper()
	var sha *string
	if err := store.Read().QueryRowContext(context.Background(),
		`SELECT sha256 FROM storage_files WHERE share_id = ? AND rel_path = ?`, shareID, relPath).Scan(&sha); err != nil {
		t.Fatalf("select sha256: %v", err)
	}
	return sha
}

// TestHashBackfillJobHashesStorageFilesInBatches covers the core job:
// every unhashed storage_files row gets its real sha256 stamped, and the
// batch size is shrunk to 1 so a 3-file run must round-trip the
// query/process loop three times to prove batching itself works, not just
// a single big LIMIT.
func TestHashBackfillJobHashesStorageFilesInBatches(t *testing.T) {
	orig := hashBackfillBatchSize
	hashBackfillBatchSize = 1
	t.Cleanup(func() { hashBackfillBatchSize = orig })

	_, store := newTestMuxWithStore(t)
	shareID, _ := newLocalShareWithFiles(t, store, map[string]string{
		"front/a.mp4": "aaa-content",
		"front/b.mp4": "bbb-content",
		"front/c.mp4": "ccc-content",
	})

	res, err := runHashBackfill(t, context.Background(), store, hashBackfillJobParams{})
	if err != nil {
		t.Fatalf("hashBackfillJobHandler: %v", err)
	}
	if res.Hashed != 3 || res.Skipped != 0 || res.Remaining != 0 {
		t.Fatalf("result = %+v, want {Hashed:3 Skipped:0 Remaining:0}", res)
	}

	for rel, content := range map[string]string{"front/a.mp4": "aaa-content", "front/b.mp4": "bbb-content", "front/c.mp4": "ccc-content"} {
		got := storageFileSHA256(t, store, shareID, rel)
		if got == nil || *got != sha256Hex([]byte(content)) {
			t.Errorf("%s sha256 = %v, want %s", rel, got, sha256Hex([]byte(content)))
		}
	}
}

// TestHashBackfillJobSkipsVanishedFiles covers a storage_files row whose
// file disappeared between being scanned/indexed and being hashed: it
// must be counted as skipped and left with sha256 NULL, and the job must
// still finish successfully rather than failing the whole run.
func TestHashBackfillJobSkipsVanishedFiles(t *testing.T) {
	_, store := newTestMuxWithStore(t)
	shareID, root := newLocalShareWithFiles(t, store, map[string]string{
		"front/a.mp4": "present",
		"front/b.mp4": "gone",
	})
	if err := os.Remove(filepath.Join(root, "front/b.mp4")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	res, err := runHashBackfill(t, context.Background(), store, hashBackfillJobParams{})
	if err != nil {
		t.Fatalf("hashBackfillJobHandler: %v", err)
	}
	if res.Hashed != 1 || res.Skipped != 1 {
		t.Fatalf("result = %+v, want {Hashed:1 Skipped:1 ...}", res)
	}
	if got := storageFileSHA256(t, store, shareID, "front/a.mp4"); got == nil || *got != sha256Hex([]byte("present")) {
		t.Errorf("front/a.mp4 sha256 = %v, want stamped", got)
	}
	if got := storageFileSHA256(t, store, shareID, "front/b.mp4"); got != nil {
		t.Errorf("front/b.mp4 sha256 = %v, want nil (vanished file left unhashed)", *got)
	}
}

// TestHashBackfillJobRespectsContextCancel covers "honor ctx cancel
// between files": hashFileFn is faked to cancel ctx itself after the
// first call, and the job must stop rather than hashing the remaining
// rows or swallowing the cancellation as success.
func TestHashBackfillJobRespectsContextCancel(t *testing.T) {
	orig := hashFileFn
	t.Cleanup(func() { hashFileFn = orig })

	_, store := newTestMuxWithStore(t)
	newLocalShareWithFiles(t, store, map[string]string{
		"front/a.mp4": "aaa",
		"front/b.mp4": "bbb",
		"front/c.mp4": "ccc",
	})

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	hashFileFn = func(path string) (string, int64, error) {
		calls++
		if calls == 1 {
			cancel()
		}
		return orig(path)
	}

	_, err := runHashBackfill(t, ctx, store, hashBackfillJobParams{})
	if err == nil {
		t.Fatal("expected an error from a cancelled context, got nil")
	}
	// The first file's own DB write (StorageFileSetSHA256) is what
	// observes the cancellation here (hashFileFn cancelled ctx from
	// inside the call that produced that file's hash, so the loop's own
	// ctx.Err() checks don't get a chance to fire until the next
	// iteration) — the error is context.Canceled wrapped through both
	// internal/db and this job's own %w-wrapped errors, so unwrap with
	// errors.Is rather than expecting an exact ctx.Err() match.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want to wrap context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("hashFileFn called %d times, want exactly 1 (must stop before a second file)", calls)
	}
}

// TestHashBackfillJobResumesOnSecondRun covers the resume story: a first
// run capped by Limit hashes only that many files, then a second,
// unscoped run (sha256 IS NULL is the only cursor) picks up exactly the
// remaining ones — proving no explicit checkpoint is needed.
func TestHashBackfillJobResumesOnSecondRun(t *testing.T) {
	orig := hashBackfillBatchSize
	hashBackfillBatchSize = 1
	t.Cleanup(func() { hashBackfillBatchSize = orig })

	_, store := newTestMuxWithStore(t)
	shareID, _ := newLocalShareWithFiles(t, store, map[string]string{
		"front/a.mp4": "aaa",
		"front/b.mp4": "bbb",
		"front/c.mp4": "ccc",
	})

	res1, err := runHashBackfill(t, context.Background(), store, hashBackfillJobParams{Limit: 1})
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res1.Hashed != 1 || res1.Remaining != 2 {
		t.Fatalf("run 1 result = %+v, want {Hashed:1 Remaining:2}", res1)
	}

	res2, err := runHashBackfill(t, context.Background(), store, hashBackfillJobParams{ShareID: shareID})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res2.Hashed != 2 || res2.Remaining != 0 {
		t.Fatalf("run 2 result = %+v, want {Hashed:2 Remaining:0} (resume from run 1)", res2)
	}
}

// TestHashBackfillJobHashesShareBackedSegments covers pass 2: a segment
// whose source is kind='share' (not ssd) with sha256 IS NULL gets hashed
// via SegmentSetSHA256, using source.root_path (== the share's mount
// point) joined with the segment's rel_path.
func TestHashBackfillJobHashesShareBackedSegments(t *testing.T) {
	_, store := newTestMuxWithStore(t)
	ctx := context.Background()
	root := t.TempDir()
	shareID, sourceID, err := store.ShareCreateWithSource(ctx, db.ShareCreateParams{
		Name: "seg-hash-test", Kind: db.KindLocal, Enabled: true,
	}, func(int64) string { return root }, "seg-hash-test")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "front", "x"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := []byte("segment-bytes")
	relPath := "front/x/segment_00000.mp4"
	if err := os.WriteFile(filepath.Join(root, relPath), content, 0o644); err != nil {
		t.Fatalf("write segment file: %v", err)
	}
	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, bytes, has_detections, has_gps)
		 VALUES (?, 'front', ?, 0, '2026-03-01T10:00:00Z', ?, 0, 0)`,
		sourceID, relPath, len(content))
	if err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	segID, _ := res.LastInsertId()

	result, err := runHashBackfill(t, ctx, store, hashBackfillJobParams{ShareID: shareID})
	if err != nil {
		t.Fatalf("hashBackfillJobHandler: %v", err)
	}
	if result.Hashed != 1 {
		t.Fatalf("result = %+v, want Hashed:1", result)
	}
	var sha *string
	if err := store.Read().QueryRowContext(ctx, `SELECT sha256 FROM segments WHERE id = ?`, segID).Scan(&sha); err != nil {
		t.Fatalf("select segment sha256: %v", err)
	}
	if sha == nil || *sha != sha256Hex(content) {
		t.Errorf("segment sha256 = %v, want %s", sha, sha256Hex(content))
	}
}
