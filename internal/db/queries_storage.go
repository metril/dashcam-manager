// queries_storage.go backs GET /api/storages: the list of persistent
// storages (every shares row, of either kind, plus the export cache
// pseudo-storage) with usage totals, plus (task 5) the storage_files
// table populated by the scan walker for every share/local-backed source
// (never for kind=ssd — the SSD is never a storage). Later tasks (7-10)
// extend this file with duplicate-detection queries.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// StorageRow is one persistent storage with its usage totals. ShareID is
// nil only for the synthetic cache pseudo-storage (the exports directory
// under DASHCAM_CACHE) — every real shares row, cifs or local, has one.
//
// SegmentCount/SegmentBytes count segments whose source is backed by this
// share (sources.share_id = shares.id) — the recorder's SSD source has
// share_id NULL and is never counted here, matching the plan's rule that
// the SSD is never a storage. ExportCount/ExportBytes count exports.share_id
// (NULL grouping to the cache row instead).
//
// Filesystem-derived fields (free/total bytes, live mount/probe status)
// are deliberately not here — that's cmd/api's job (same share.IsMounted/
// FreeSpace logic GET /api/shares already uses in toShareView), since this
// package has no business doing filesystem syscalls.
type StorageRow struct {
	ShareID      *int64
	Kind         string
	Name         string
	MountPoint   string
	Enabled      bool
	Status       string
	SegmentCount int64
	SegmentBytes int64
	ExportCount  int64
	ExportBytes  int64
	// HashPending is the count of this share's storage_files rows with
	// sha256 IS NULL (0 for the cache pseudo-storage, which has no
	// storage_files rows at all).
	HashPending int64
}

// StoragesList returns every persistent storage — every shares row (kind
// cifs|local), ordered by id, followed by the cache pseudo-storage
// (ShareID nil, Kind "cache") — each with its segment/export usage.
func (d *DB) StoragesList(ctx context.Context) ([]StorageRow, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT sh.id, sh.kind, sh.name, sh.mount_point, sh.enabled, sh.status,
			COALESCE(seg.cnt, 0), COALESCE(seg.bytes, 0),
			COALESCE(exp.cnt, 0), COALESCE(exp.bytes, 0),
			COALESCE(hp.cnt, 0)
		FROM shares sh
		LEFT JOIN (
			SELECT src.share_id AS share_id, COUNT(*) AS cnt, COALESCE(SUM(s.bytes), 0) AS bytes
			FROM segments s
			JOIN sources src ON src.id = s.source_id
			WHERE src.share_id IS NOT NULL
			GROUP BY src.share_id
		) seg ON seg.share_id = sh.id
		LEFT JOIN (
			SELECT share_id, COUNT(*) AS cnt, COALESCE(SUM(bytes), 0) AS bytes
			FROM exports
			WHERE share_id IS NOT NULL
			GROUP BY share_id
		) exp ON exp.share_id = sh.id
		LEFT JOIN (
			SELECT share_id, COUNT(*) AS cnt
			FROM storage_files
			WHERE sha256 IS NULL
			GROUP BY share_id
		) hp ON hp.share_id = sh.id
		ORDER BY sh.id
	`)
	if err != nil {
		return nil, fmt.Errorf("db: storages list: %w", err)
	}
	defer rows.Close()

	var out []StorageRow
	for rows.Next() {
		var r StorageRow
		var shareID int64
		var enabled int
		if err := rows.Scan(&shareID, &r.Kind, &r.Name, &r.MountPoint, &enabled, &r.Status,
			&r.SegmentCount, &r.SegmentBytes, &r.ExportCount, &r.ExportBytes, &r.HashPending); err != nil {
			return nil, fmt.Errorf("db: storages list: %w", err)
		}
		r.ShareID = &shareID
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: storages list: %w", err)
	}

	var cache StorageRow
	cache.Kind = "cache"
	cache.Name = "cache"
	cache.Status = "ok"
	cache.Enabled = true
	if err := d.read.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM exports WHERE share_id IS NULL`).
		Scan(&cache.ExportCount, &cache.ExportBytes); err != nil {
		return nil, fmt.Errorf("db: storages list cache exports: %w", err)
	}
	out = append(out, cache)

	return out, nil
}

// StorageFileUpsertRow is one file the scan walker found under a share/
// local-backed source's root, ready to upsert into storage_files.
type StorageFileUpsertRow struct {
	RelPath string
	Bytes   int64
	// Mtime is a unix timestamp, matching segments.mtime's convention.
	Mtime int64
}

// storageFileUpsertBatchSize caps how many rows go into a single multi-row
// INSERT statement — same reasoning as insertRowsBatch (internal/scan/
// index.go): sqlite's per-statement placeholder limit and diminishing
// returns from an ever-larger single statement.
const storageFileUpsertBatchSize = 300

// StorageFileUpsert upserts rows into storage_files for shareID, all as one
// Tx (the walker calls this once per directory it finishes reading — see
// internal/scan's storage-files walk — so "one Tx per batch" here means one
// Tx per directory's worth of files, not one Tx for an entire share).
//
// A row already present with the same (bytes, mtime) keeps its sha256/
// hashed_at untouched. A row whose bytes or mtime changed since it was last
// seen has its sha256/hashed_at cleared (the old hash no longer describes
// the current file content) so hash_backfill re-hashes it.
// first_seen_at is set once, on insert, and never touched again; last_seen_at
// is always bumped to seenAt — see StorageFilesPruneNotSeenSince, which uses
// last_seen_at to find rows a subsequent scan no longer saw.
func (d *DB) StorageFileUpsert(ctx context.Context, shareID int64, rows []StorageFileUpsertRow, seenAt string) error {
	if len(rows) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		const prefix = `INSERT INTO storage_files
			(share_id, rel_path, bytes, mtime, sha256, hashed_at, first_seen_at, last_seen_at)
			VALUES `
		const suffix = `
			ON CONFLICT(share_id, rel_path) DO UPDATE SET
				bytes = excluded.bytes,
				mtime = excluded.mtime,
				last_seen_at = excluded.last_seen_at,
				sha256 = CASE WHEN storage_files.bytes = excluded.bytes AND storage_files.mtime = excluded.mtime
					THEN storage_files.sha256 ELSE NULL END,
				hashed_at = CASE WHEN storage_files.bytes = excluded.bytes AND storage_files.mtime = excluded.mtime
					THEN storage_files.hashed_at ELSE NULL END`
		for start := 0; start < len(rows); start += storageFileUpsertBatchSize {
			end := start + storageFileUpsertBatchSize
			if end > len(rows) {
				end = len(rows)
			}
			chunk := rows[start:end]
			placeholders := make([]string, len(chunk))
			args := make([]any, 0, len(chunk)*8)
			for i, r := range chunk {
				placeholders[i] = "(?,?,?,?,NULL,NULL,?,?)"
				args = append(args, shareID, r.RelPath, r.Bytes, r.Mtime, seenAt, seenAt)
			}
			query := prefix + strings.Join(placeholders, ",") + suffix
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("db: storage file upsert: %w", err)
			}
		}
		return nil
	})
}

// StorageFilesPruneNotSeenSince deletes storage_files rows for shareID whose
// last_seen_at predates seenAt — i.e. files a scan that started at seenAt
// (every row it touched was stamped with exactly that seenAt by
// StorageFileUpsert above) did not walk this time, meaning they're gone
// from the storage. Returns the number of rows removed.
func (d *DB) StorageFilesPruneNotSeenSince(ctx context.Context, shareID int64, seenAt string) (int64, error) {
	res, err := d.write.ExecContext(ctx,
		`DELETE FROM storage_files WHERE share_id = ? AND last_seen_at < ?`, shareID, seenAt)
	if err != nil {
		return 0, fmt.Errorf("db: storage files prune %d: %w", shareID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: storage files prune %d: %w", shareID, err)
	}
	return n, nil
}

// StorageFileHashCandidate is one storage_files row still awaiting a
// sha256, as returned by StorageFilesNeedingHash for the hash_backfill job
// (task 6) to hash — ShareMountPoint/ShareKind let the caller build the
// file's absolute path without a second query per row.
type StorageFileHashCandidate struct {
	ID              int64
	ShareID         int64
	RelPath         string
	Bytes           int64
	ShareMountPoint string
	ShareKind       string
}

// StorageFilesNeedingHash returns up to limit storage_files rows with
// sha256 IS NULL, oldest first_seen_at first (so a long-running backfill
// makes steady forward progress rather than re-picking the same rows).
// Joins shares for the mount point/kind the hash job needs to open the
// file; a row whose share was deleted out from under it (shouldn't happen —
// share_id CASCADEs) is simply excluded by the INNER JOIN.
func (d *DB) StorageFilesNeedingHash(ctx context.Context, limit int) ([]StorageFileHashCandidate, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT sf.id, sf.share_id, sf.rel_path, sf.bytes, sh.mount_point, sh.kind
		FROM storage_files sf
		JOIN shares sh ON sh.id = sf.share_id
		WHERE sf.sha256 IS NULL
		ORDER BY sf.first_seen_at
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: storage files needing hash: %w", err)
	}
	defer rows.Close()

	var out []StorageFileHashCandidate
	for rows.Next() {
		var c StorageFileHashCandidate
		if err := rows.Scan(&c.ID, &c.ShareID, &c.RelPath, &c.Bytes, &c.ShareMountPoint, &c.ShareKind); err != nil {
			return nil, fmt.Errorf("db: storage files needing hash: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: storage files needing hash: %w", err)
	}
	return out, nil
}

// StorageFileSetSHA256 stamps a storage_files row's sha256/hashed_at once
// the hash_backfill job has computed it.
func (d *DB) StorageFileSetSHA256(ctx context.Context, id int64, sha256, hashedAt string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE storage_files SET sha256 = ?, hashed_at = ? WHERE id = ?`, sha256, hashedAt, id)
	if err != nil {
		return fmt.Errorf("db: storage file set sha256 %d: %w", id, err)
	}
	return nil
}
