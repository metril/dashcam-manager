package db

import (
	"context"
	"testing"
)

// TestStorageFilesTableSchema covers the DDL half of task 5: a fresh
// InitSchema creates storage_files with its UNIQUE(share_id, rel_path)
// constraint and the sha256/bytes/partial-unhashed indexes.
func TestStorageFilesTableSchema(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "local", Kind: KindLocal, MountPoint: "/mnt/shares/local-test", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO storage_files (share_id, rel_path, bytes, mtime, first_seen_at, last_seen_at)
		 VALUES (?, 'front/a.mp4', 100, 1000, 't1', 't1')`, shareID); err != nil {
		t.Fatalf("insert storage_files: %v", err)
	}

	// UNIQUE(share_id, rel_path) rejects a duplicate.
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO storage_files (share_id, rel_path, bytes, mtime, first_seen_at, last_seen_at)
		 VALUES (?, 'front/a.mp4', 200, 2000, 't2', 't2')`, shareID); err == nil {
		t.Fatalf("expected UNIQUE(share_id, rel_path) violation, got no error")
	}

	// share_id CASCADEs.
	if _, err := d.Write().ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, shareID); err != nil {
		t.Fatalf("delete share: %v", err)
	}
	var count int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_files WHERE share_id = ?`, shareID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected cascade delete, found %d rows", count)
	}

	for _, idx := range []string{"idx_storage_files_sha256", "idx_storage_files_bytes", "idx_storage_files_unhashed"} {
		var name string
		if err := d.Read().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&name); err != nil {
			t.Fatalf("index %s missing: %v", idx, err)
		}
	}
}

// TestStorageFileUpsertIdempotentAndInvalidatesHash covers StorageFileUpsert's
// contract: re-upserting the same (bytes, mtime) keeps a stamped sha256;
// upserting a changed (bytes, mtime) clears it so hash_backfill re-hashes.
func TestStorageFileUpsertIdempotentAndInvalidatesHash(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "local", Kind: KindLocal, MountPoint: "/mnt/shares/local-test", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}

	if err := d.StorageFileUpsert(ctx, shareID, []StorageFileUpsertRow{
		{RelPath: "front/a.mp4", Bytes: 100, Mtime: 1000},
	}, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	var id int64
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM storage_files WHERE share_id = ? AND rel_path = 'front/a.mp4'`, shareID).Scan(&id); err != nil {
		t.Fatalf("select id: %v", err)
	}
	if err := d.StorageFileSetSHA256(ctx, id, "deadbeef", "2026-01-01T00:00:01Z"); err != nil {
		t.Fatalf("set sha256: %v", err)
	}

	// Re-upsert with identical bytes/mtime, later seenAt: sha256 survives,
	// last_seen_at advances, first_seen_at stays put.
	if err := d.StorageFileUpsert(ctx, shareID, []StorageFileUpsertRow{
		{RelPath: "front/a.mp4", Bytes: 100, Mtime: 1000},
	}, "2026-01-02T00:00:00Z"); err != nil {
		t.Fatalf("upsert 2 (unchanged): %v", err)
	}
	var sha256 *string
	var firstSeen, lastSeen string
	if err := d.Read().QueryRowContext(ctx,
		`SELECT sha256, first_seen_at, last_seen_at FROM storage_files WHERE id = ?`, id).
		Scan(&sha256, &firstSeen, &lastSeen); err != nil {
		t.Fatalf("select after unchanged upsert: %v", err)
	}
	if sha256 == nil || *sha256 != "deadbeef" {
		t.Fatalf("expected sha256 to survive an unchanged re-upsert, got %v", sha256)
	}
	if firstSeen != "2026-01-01T00:00:00Z" {
		t.Fatalf("first_seen_at should not move, got %q", firstSeen)
	}
	if lastSeen != "2026-01-02T00:00:00Z" {
		t.Fatalf("last_seen_at should advance, got %q", lastSeen)
	}

	// Re-upsert with a changed size: sha256/hashed_at must be cleared.
	if err := d.StorageFileUpsert(ctx, shareID, []StorageFileUpsertRow{
		{RelPath: "front/a.mp4", Bytes: 999, Mtime: 1000},
	}, "2026-01-03T00:00:00Z"); err != nil {
		t.Fatalf("upsert 3 (changed bytes): %v", err)
	}
	var bytes int64
	if err := d.Read().QueryRowContext(ctx,
		`SELECT bytes, sha256 FROM storage_files WHERE id = ?`, id).Scan(&bytes, &sha256); err != nil {
		t.Fatalf("select after changed upsert: %v", err)
	}
	if bytes != 999 {
		t.Fatalf("bytes should update to 999, got %d", bytes)
	}
	if sha256 != nil {
		t.Fatalf("expected sha256 cleared after a content change, got %v", *sha256)
	}
}

// TestStorageFilesNeedingHashAndPrune covers the hash-backfill query and
// the scan-prune query together: unhashed rows come back oldest-first up
// to limit, and pruning removes only rows not touched by the given seenAt.
func TestStorageFilesNeedingHashAndPrune(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "local", Kind: KindLocal, MountPoint: "/mnt/shares/local-test", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}

	if err := d.StorageFileUpsert(ctx, shareID, []StorageFileUpsertRow{
		{RelPath: "front/a.mp4", Bytes: 100, Mtime: 1000},
		{RelPath: "front/b.mp4", Bytes: 200, Mtime: 2000},
	}, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("upsert scan 1: %v", err)
	}

	cands, err := d.StorageFilesNeedingHash(ctx, 10, 0)
	if err != nil {
		t.Fatalf("StorageFilesNeedingHash: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}
	if cands[0].RelPath != "front/a.mp4" || cands[0].ShareMountPoint != "/mnt/shares/local-test" || cands[0].ShareKind != KindLocal {
		t.Fatalf("unexpected candidate 0: %+v", cands[0])
	}

	if err := d.StorageFileSetSHA256(ctx, cands[0].ID, "hash-a", "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("set sha256: %v", err)
	}
	cands, err = d.StorageFilesNeedingHash(ctx, 10, 0)
	if err != nil {
		t.Fatalf("StorageFilesNeedingHash 2: %v", err)
	}
	if len(cands) != 1 || cands[0].RelPath != "front/b.mp4" {
		t.Fatalf("expected only front/b.mp4 still needing a hash, got %+v", cands)
	}

	// A second scan only sees front/a.mp4 (front/b.mp4 was deleted) —
	// pruning with that scan's seenAt removes the row it didn't touch.
	if err := d.StorageFileUpsert(ctx, shareID, []StorageFileUpsertRow{
		{RelPath: "front/a.mp4", Bytes: 100, Mtime: 1000},
	}, "2026-01-02T00:00:00Z"); err != nil {
		t.Fatalf("upsert scan 2: %v", err)
	}
	removed, err := d.StorageFilesPruneNotSeenSince(ctx, shareID, "2026-01-02T00:00:00Z")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 row pruned, got %d", removed)
	}
	var remaining int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_files WHERE share_id = ?`, shareID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("expected 1 row remaining, got %d", remaining)
	}
}

// TestStoragesListHashPending covers GET /api/storages' hash_pending field
// at the db layer: it's the count of a share's unhashed storage_files rows,
// and the cache pseudo-storage always reports 0 (it has no storage_files).
func TestStoragesListHashPending(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "local", Kind: KindLocal, MountPoint: "/mnt/shares/local-test", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	if err := d.StorageFileUpsert(ctx, shareID, []StorageFileUpsertRow{
		{RelPath: "front/a.mp4", Bytes: 100, Mtime: 1000},
		{RelPath: "front/b.mp4", Bytes: 200, Mtime: 2000},
	}, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var id int64
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM storage_files WHERE share_id = ? AND rel_path = 'front/a.mp4'`, shareID).Scan(&id); err != nil {
		t.Fatalf("select id: %v", err)
	}
	if err := d.StorageFileSetSHA256(ctx, id, "hash-a", "2026-01-01T00:00:02Z"); err != nil {
		t.Fatalf("set sha256: %v", err)
	}

	rows, err := d.StoragesList(ctx)
	if err != nil {
		t.Fatalf("StoragesList: %v", err)
	}
	var sawShare, sawCache bool
	for _, r := range rows {
		if r.ShareID != nil && *r.ShareID == shareID {
			sawShare = true
			if r.HashPending != 1 {
				t.Fatalf("expected share hash_pending=1, got %d", r.HashPending)
			}
		}
		if r.Kind == "cache" {
			sawCache = true
			if r.HashPending != 0 {
				t.Fatalf("expected cache hash_pending=0, got %d", r.HashPending)
			}
		}
	}
	if !sawShare || !sawCache {
		t.Fatalf("expected both the share and cache rows, got %+v", rows)
	}
}
