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
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
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
// shareID, when non-zero, restricts the result to that one share (the
// hash_backfill job's optional share_id param — task 6); zero means every
// share. Joins shares for the mount point/kind the hash job needs to open
// the file; a row whose share was deleted out from under it (shouldn't
// happen — share_id CASCADEs) is simply excluded by the INNER JOIN.
func (d *DB) StorageFilesNeedingHash(ctx context.Context, limit int, shareID int64) ([]StorageFileHashCandidate, error) {
	query := `
		SELECT sf.id, sf.share_id, sf.rel_path, sf.bytes, sh.mount_point, sh.kind
		FROM storage_files sf
		JOIN shares sh ON sh.id = sf.share_id
		WHERE sf.sha256 IS NULL`
	args := []any{}
	if shareID != 0 {
		query += ` AND sf.share_id = ?`
		args = append(args, shareID)
	}
	query += ` ORDER BY sf.first_seen_at LIMIT ?`
	args = append(args, limit)

	rows, err := d.read.QueryContext(ctx, query, args...)
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

// StorageFilesNeedingHashCount is StorageFilesNeedingHash's count-only
// form, used by the hash_backfill job (task 6) to report a "remaining"
// figure in its progress without pulling every remaining row into memory.
func (d *DB) StorageFilesNeedingHashCount(ctx context.Context, shareID int64) (int64, error) {
	query := `SELECT COUNT(*) FROM storage_files WHERE sha256 IS NULL`
	args := []any{}
	if shareID != 0 {
		query += ` AND share_id = ?`
		args = append(args, shareID)
	}
	var n int64
	if err := d.read.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: storage files needing hash count: %w", err)
	}
	return n, nil
}

// SegmentHashCandidate is one segments row still awaiting a sha256 whose
// source is share-backed (kind='share'), as returned by SegmentsNeedingHash
// for the hash_backfill job (task 6). RootPath is the segment's source's
// root_path, which for a kind='share' source is exactly its share's
// mount_point (see handleStoragesScan/SourceCreate) — joined with RelPath
// to get the file's absolute path, the same convention CleanupCandidates
// (queries_ingest.go) uses.
type SegmentHashCandidate struct {
	SegmentID       int64
	RootPath        string
	RelPath         string
	Bytes           int64
	ShareID         int64
	ShareMountPoint string
	ShareKind       string
}

// SegmentsNeedingHash returns up to limit segments rows with sha256 IS
// NULL whose source is share-backed (sources.kind = 'share'; an SSD-backed
// segment is never a candidate — the SSD is ingested, not hashed in
// place), oldest segment id first. shareID, when non-zero, restricts the
// result to segments backed by that one share.
func (d *DB) SegmentsNeedingHash(ctx context.Context, limit int, shareID int64) ([]SegmentHashCandidate, error) {
	query := `
		SELECT s.id, src.root_path, s.rel_path, s.bytes, src.share_id, sh.mount_point, sh.kind
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		JOIN shares sh ON sh.id = src.share_id
		WHERE s.sha256 IS NULL AND src.kind = 'share'`
	args := []any{}
	if shareID != 0 {
		query += ` AND src.share_id = ?`
		args = append(args, shareID)
	}
	query += ` ORDER BY s.id LIMIT ?`
	args = append(args, limit)

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: segments needing hash: %w", err)
	}
	defer rows.Close()

	var out []SegmentHashCandidate
	for rows.Next() {
		var c SegmentHashCandidate
		if err := rows.Scan(&c.SegmentID, &c.RootPath, &c.RelPath, &c.Bytes, &c.ShareID, &c.ShareMountPoint, &c.ShareKind); err != nil {
			return nil, fmt.Errorf("db: segments needing hash: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: segments needing hash: %w", err)
	}
	return out, nil
}

// SegmentsNeedingHashCount is SegmentsNeedingHash's count-only form, used
// by the hash_backfill job (task 6) for progress reporting.
func (d *DB) SegmentsNeedingHashCount(ctx context.Context, shareID int64) (int64, error) {
	query := `
		SELECT COUNT(*)
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.sha256 IS NULL AND src.kind = 'share'`
	args := []any{}
	if shareID != 0 {
		query += ` AND src.share_id = ?`
		args = append(args, shareID)
	}
	var n int64
	if err := d.read.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: segments needing hash count: %w", err)
	}
	return n, nil
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

// ---- duplicate detection (task 7) ----

// fileCopyRow is one row of the file_copies view.
type fileCopyRow struct {
	RefKind    string
	RefID      int64
	ShareID    sql.NullInt64
	SourceID   sql.NullInt64
	StorageKey string
	RelPath    string
	Bytes      int64
	SHA256     sql.NullString
}

// DuplicateGroupKindSHA256/DuplicateGroupKindSizeName are duplicate_groups.kind's
// two values — see DuplicateReportRefresh's doc comment for the exact rule
// each represents.
const (
	DuplicateGroupKindSHA256   = "sha256"
	DuplicateGroupKindSizeName = "size_name"
)

// DuplicateReportRefresh rebuilds duplicate_groups/duplicate_group_members
// from file_copies, in one transaction (catalog_terms' RefreshCatalog is
// the precedent for "precomputed report table rebuilt wholesale", though
// file_copies is small enough — bounded by segments/ingests/pulls/exports/
// storage_files row counts, not a multi-million-row detection table — that
// this doesn't need RefreshCatalog's staged-outside-a-transaction dance).
//
// Grouping rules (the plan's "Duplicate semantics" section):
//   - Group by sha256 first: >=2 file_copies rows sharing a non-empty
//     sha256, on >=2 DISTINCT storage_key values, is a "sha256"-kind
//     (verified) group. Two copies on the very same storage don't count
//     toward that "2" — a group needs cross-storage evidence.
//   - Everything else is grouped by (bytes, lower(basename(rel_path))):
//     a bucket with >=2 rows, on >=2 distinct storage_key values, and at
//     least one member still unhashed (sha256 empty) is a "size_name"-kind
//     (probable) group. A bucket where every member IS hashed is never
//     reported here: if they all agree it was already reported as a
//     verified sha256 group above; if they disagree ("hashed and
//     different") it's dropped — same size and name is a coincidence, not
//     a duplicate, once the hashes prove the content differs.
//
// ingest/pull rows already carry their linked segment's sha256 via
// file_copies' own COALESCE (see the view's doc comment in schema.go), so
// they need no special handling here — they're just file_copies rows like
// any other by the time this function sees them.
func (d *DB) DuplicateReportRefresh(ctx context.Context) error {
	rows, err := d.read.QueryContext(ctx, `
		SELECT ref_kind, ref_id, share_id, source_id, storage_key, rel_path, bytes, sha256
		FROM file_copies
	`)
	if err != nil {
		return fmt.Errorf("db: duplicate report refresh: read file_copies: %w", err)
	}
	var copies []fileCopyRow
	for rows.Next() {
		var r fileCopyRow
		if err := rows.Scan(&r.RefKind, &r.RefID, &r.ShareID, &r.SourceID, &r.StorageKey, &r.RelPath, &r.Bytes, &r.SHA256); err != nil {
			rows.Close()
			return fmt.Errorf("db: duplicate report refresh: scan file_copies: %w", err)
		}
		copies = append(copies, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("db: duplicate report refresh: file_copies: %w", err)
	}
	rows.Close()

	bySHA := map[string][]fileCopyRow{}
	bySizeName := map[string][]fileCopyRow{}
	for _, r := range copies {
		if r.SHA256.Valid && r.SHA256.String != "" {
			bySHA[r.SHA256.String] = append(bySHA[r.SHA256.String], r)
		}
		key := fmt.Sprintf("%d|%s", r.Bytes, strings.ToLower(path.Base(r.RelPath)))
		bySizeName[key] = append(bySizeName[key], r)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	type groupBuild struct {
		key     string
		kind    string
		bytes   int64
		sample  string
		members []fileCopyRow
	}
	var groups []groupBuild

	distinctStorages := func(members []fileCopyRow) int {
		set := map[string]bool{}
		for _, m := range members {
			set[m.StorageKey] = true
		}
		return len(set)
	}
	maxBytes := func(members []fileCopyRow) int64 {
		var b int64
		for _, m := range members {
			if m.Bytes > b {
				b = m.Bytes
			}
		}
		return b
	}

	for sha, members := range bySHA {
		if len(members) < 2 || distinctStorages(members) < 2 {
			continue
		}
		groups = append(groups, groupBuild{
			key: "sha256:" + sha, kind: DuplicateGroupKindSHA256,
			bytes: maxBytes(members), sample: path.Base(members[0].RelPath), members: members,
		})
	}
	for key, members := range bySizeName {
		if len(members) < 2 {
			continue
		}
		hasUnhashed := false
		for _, m := range members {
			if !m.SHA256.Valid || m.SHA256.String == "" {
				hasUnhashed = true
				break
			}
		}
		if !hasUnhashed {
			// All hashed: either already reported as a verified sha256
			// group above (all equal), or hashed-and-different — dropped
			// either way, per the doc comment above.
			continue
		}
		if distinctStorages(members) < 2 {
			continue
		}
		groups = append(groups, groupBuild{
			key: "size_name:" + key, kind: DuplicateGroupKindSizeName,
			bytes: maxBytes(members), sample: path.Base(members[0].RelPath), members: members,
		})
	}

	// Deterministic order isn't required for correctness (the table is
	// keyed by group_key), but sorting keeps DuplicateReportList's default
	// ordering stable across refreshes when nothing changed, and makes
	// test output/diffs predictable.
	sort.Slice(groups, func(i, j int) bool { return groups[i].key < groups[j].key })

	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_group_members`); err != nil {
			return fmt.Errorf("db: duplicate report refresh: clear members: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM duplicate_groups`); err != nil {
			return fmt.Errorf("db: duplicate report refresh: clear groups: %w", err)
		}
		for _, g := range groups {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO duplicate_groups (group_key, kind, copies, storages, bytes, sample_name, refreshed_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				g.key, g.kind, len(g.members), distinctStorages(g.members), g.bytes, g.sample, now,
			); err != nil {
				return fmt.Errorf("db: duplicate report refresh: insert group %s: %w", g.key, err)
			}
			for _, m := range g.members {
				var shareID, sourceID any
				if m.ShareID.Valid {
					shareID = m.ShareID.Int64
				}
				if m.SourceID.Valid {
					sourceID = m.SourceID.Int64
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO duplicate_group_members (group_key, ref_kind, ref_id, share_id, source_id, storage_key, rel_path)
					 VALUES (?, ?, ?, ?, ?, ?, ?)`,
					g.key, m.RefKind, m.RefID, shareID, sourceID, m.StorageKey, m.RelPath,
				); err != nil {
					return fmt.Errorf("db: duplicate report refresh: insert member %s/%s/%d: %w", g.key, m.RefKind, m.RefID, err)
				}
			}
		}
		return nil
	})
}

// DuplicateGroupMember is one member of a DuplicateGroup.
type DuplicateGroupMember struct {
	RefKind    string `json:"ref_kind"`
	RefID      int64  `json:"ref_id"`
	ShareID    *int64 `json:"share_id,omitempty"`
	SourceID   *int64 `json:"source_id,omitempty"`
	StorageKey string `json:"storage_key"`
	RelPath    string `json:"rel_path"`
}

// DuplicateGroup is one duplicate_groups row plus its members, as returned
// by DuplicateReportList.
type DuplicateGroup struct {
	GroupKey    string                 `json:"group_key"`
	Kind        string                 `json:"kind"`
	Copies      int64                  `json:"copies"`
	Storages    int64                  `json:"storages"`
	Bytes       int64                  `json:"bytes"`
	SampleName  string                 `json:"sample_name"`
	RefreshedAt string                 `json:"refreshed_at"`
	Members     []DuplicateGroupMember `json:"members"`
}

// DuplicateReportList returns duplicate_groups rows (newest-first by
// group_key, matching DuplicateReportRefresh's stable sort), each with its
// members, optionally filtered by kind ("" = both) and storageID (0 = no
// filter; matches a group with at least one member on that storage —
// storageID is a share id, since only a shares row or the cache can be a
// storage and the cache is never filterable by id here). limit<=0 defaults
// to 50. Returns the page of groups plus the total group count (for that
// kind/storage filter, ignoring limit/offset) for pagination.
func (d *DB) DuplicateReportList(ctx context.Context, kind string, storageID int64, limit, offset int) ([]DuplicateGroup, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	whereClauses := []string{"1=1"}
	args := []any{}
	if kind != "" {
		whereClauses = append(whereClauses, "g.kind = ?")
		args = append(args, kind)
	}
	if storageID != 0 {
		whereClauses = append(whereClauses, `EXISTS (SELECT 1 FROM duplicate_group_members m WHERE m.group_key = g.group_key AND m.share_id = ?)`)
		args = append(args, storageID)
	}
	where := strings.Join(whereClauses, " AND ")

	var total int64
	if err := d.read.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM duplicate_groups g WHERE %s`, where), args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("db: duplicate report list: count: %w", err)
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT g.group_key, g.kind, g.copies, g.storages, g.bytes, g.sample_name, g.refreshed_at
		FROM duplicate_groups g
		WHERE %s
		ORDER BY g.group_key
		LIMIT ? OFFSET ?
	`, where), pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("db: duplicate report list: %w", err)
	}
	var out []DuplicateGroup
	var keys []string
	byKey := map[string]*DuplicateGroup{}
	for rows.Next() {
		var g DuplicateGroup
		if err := rows.Scan(&g.GroupKey, &g.Kind, &g.Copies, &g.Storages, &g.Bytes, &g.SampleName, &g.RefreshedAt); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("db: duplicate report list: scan: %w", err)
		}
		out = append(out, g)
		keys = append(keys, g.GroupKey)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("db: duplicate report list: %w", err)
	}
	rows.Close()
	for i := range out {
		byKey[out[i].GroupKey] = &out[i]
	}

	if len(keys) > 0 {
		placeholders := make([]string, len(keys))
		memberArgs := make([]any, len(keys))
		for i, k := range keys {
			placeholders[i] = "?"
			memberArgs[i] = k
		}
		memberRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
			SELECT group_key, ref_kind, ref_id, share_id, source_id, storage_key, rel_path
			FROM duplicate_group_members
			WHERE group_key IN (%s)
			ORDER BY group_key, ref_kind, ref_id
		`, strings.Join(placeholders, ",")), memberArgs...)
		if err != nil {
			return nil, 0, fmt.Errorf("db: duplicate report list: members: %w", err)
		}
		for memberRows.Next() {
			var groupKey string
			var m DuplicateGroupMember
			var shareID, sourceID sql.NullInt64
			if err := memberRows.Scan(&groupKey, &m.RefKind, &m.RefID, &shareID, &sourceID, &m.StorageKey, &m.RelPath); err != nil {
				memberRows.Close()
				return nil, 0, fmt.Errorf("db: duplicate report list: scan member: %w", err)
			}
			if shareID.Valid {
				m.ShareID = &shareID.Int64
			}
			if sourceID.Valid {
				m.SourceID = &sourceID.Int64
			}
			if g, ok := byKey[groupKey]; ok {
				g.Members = append(g.Members, m)
			}
		}
		if err := memberRows.Err(); err != nil {
			memberRows.Close()
			return nil, 0, fmt.Errorf("db: duplicate report list: members: %w", err)
		}
		memberRows.Close()
	}

	return out, total, nil
}

// SegmentCopyCount is one segment's cross-storage copy summary, as
// returned by SegmentCopyCounts for the "on N storages"/"not ingested"
// badges (task 9).
type SegmentCopyCount struct {
	Storages int64
	// Verified reports whether this segment itself has a confirmed sha256
	// — i.e. whatever storage count is reported can be trusted as exact
	// content matches (the segment's own copy, plus any verified ingest/
	// pull of it, are already known-identical by construction; a
	// non-empty sha256 additionally means a future sha256-based duplicate
	// group involving this segment is possible). False just means this
	// segment hasn't been hashed yet, not that its copies are wrong.
	Verified bool
}

// SegmentCopyCounts returns, for each id in segmentIDs, the number of
// distinct persistent storages holding a copy of that segment's footage —
// the segment's own storage (if its source is share/local-backed; an
// SSD-sourced segment contributes none, since the SSD is never a storage)
// plus every verified ingests/dashcam_pulls row copied from it — and
// whether the segment has a confirmed sha256. A segment with no entry in
// the returned map has zero storages (never ingested/pulled and not
// itself on a persistent storage).
func (d *DB) SegmentCopyCounts(ctx context.Context, segmentIDs []int64) (map[int64]SegmentCopyCount, error) {
	out := map[int64]SegmentCopyCount{}
	if len(segmentIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(segmentIDs))
	args := make([]any, len(segmentIDs))
	for i, id := range segmentIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(placeholders, ",")

	verified := map[int64]bool{}
	vRows, err := d.read.QueryContext(ctx,
		fmt.Sprintf(`SELECT id FROM segments WHERE id IN (%s) AND sha256 IS NOT NULL AND sha256 != ''`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copy counts: verified: %w", err)
	}
	for vRows.Next() {
		var id int64
		if err := vRows.Scan(&id); err != nil {
			vRows.Close()
			return nil, fmt.Errorf("db: segment copy counts: verified: %w", err)
		}
		verified[id] = true
	}
	if err := vRows.Err(); err != nil {
		vRows.Close()
		return nil, fmt.Errorf("db: segment copy counts: verified: %w", err)
	}
	vRows.Close()

	storageKeys := map[int64]map[string]bool{}
	addKey := func(segID int64, key string) {
		set, ok := storageKeys[segID]
		if !ok {
			set = map[string]bool{}
			storageKeys[segID] = set
		}
		set[key] = true
	}

	// The segment's own storage, only when it's itself share/local-backed.
	rows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id, src.share_id
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.id IN (%s) AND src.share_id IS NOT NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copy counts: own storage: %w", err)
	}
	for rows.Next() {
		var id, shareID int64
		if err := rows.Scan(&id, &shareID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: segment copy counts: own storage: %w", err)
		}
		addKey(id, fmt.Sprintf("share:%d", shareID))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("db: segment copy counts: own storage: %w", err)
	}
	rows.Close()

	// Verified ingests.
	iRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT segment_id, share_id FROM ingests WHERE segment_id IN (%s) AND verified_at IS NOT NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copy counts: ingests: %w", err)
	}
	for iRows.Next() {
		var id, shareID int64
		if err := iRows.Scan(&id, &shareID); err != nil {
			iRows.Close()
			return nil, fmt.Errorf("db: segment copy counts: ingests: %w", err)
		}
		addKey(id, fmt.Sprintf("share:%d", shareID))
	}
	if err := iRows.Err(); err != nil {
		iRows.Close()
		return nil, fmt.Errorf("db: segment copy counts: ingests: %w", err)
	}
	iRows.Close()

	// Verified dashcam_pulls linked back to one of these segments.
	pRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT indexed_segment_id, share_id FROM dashcam_pulls
		WHERE indexed_segment_id IN (%s) AND verified_at IS NOT NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copy counts: pulls: %w", err)
	}
	for pRows.Next() {
		var id, shareID int64
		if err := pRows.Scan(&id, &shareID); err != nil {
			pRows.Close()
			return nil, fmt.Errorf("db: segment copy counts: pulls: %w", err)
		}
		addKey(id, fmt.Sprintf("share:%d", shareID))
	}
	if err := pRows.Err(); err != nil {
		pRows.Close()
		return nil, fmt.Errorf("db: segment copy counts: pulls: %w", err)
	}
	pRows.Close()

	for _, id := range segmentIDs {
		out[id] = SegmentCopyCount{Storages: int64(len(storageKeys[id])), Verified: verified[id]}
	}
	return out, nil
}

// ---- copy detail lists (task 9) ----

// CopyRef is one persistent storage holding a copy of a segment's footage
// or an export's file, as returned by SegmentCopies/ExportCopies for the
// segment detail/catalog/trip-detail/exports API responses' "copies"
// field. StorageID mirrors GET /api/storages' id shape: a shares.id
// formatted as a decimal string, or the literal "cache" for the export
// cache pseudo-storage — never the SSD, which is not a storage.
type CopyRef struct {
	StorageID string `json:"storage_id"`
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	RelPath   string `json:"rel_path"`
	// Verified reports whether this specific copy is known byte-identical
	// (the segment's own file, a verified ingest/pull, or a sha256-kind
	// duplicate group member) as opposed to only a probable size+basename
	// match still awaiting a hash.
	Verified bool `json:"verified"`
}

// shareLabelKind batches share id -> (name, kind) lookups for building
// CopyRef entries without a query per row.
func (d *DB) shareLabelKind(ctx context.Context, shareIDs []int64) (map[int64][2]string, error) {
	out := map[int64][2]string{}
	if len(shareIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(shareIDs))
	args := make([]any, len(shareIDs))
	for i, id := range shareIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := d.read.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, name, kind FROM shares WHERE id IN (%s)`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("db: share label kind: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, kind string
		if err := rows.Scan(&id, &name, &kind); err != nil {
			return nil, fmt.Errorf("db: share label kind: %w", err)
		}
		out[id] = [2]string{name, kind}
	}
	return out, rows.Err()
}

// SegmentCopies returns, for each id in segmentIDs, the list of persistent
// storages holding a copy of that segment's footage — the segment's own
// storage (only when its source is share/local-backed; an SSD-sourced
// segment contributes none) plus every verified ingests/dashcam_pulls row
// copied from it. This is SegmentCopyCounts' member-list counterpart —
// same provenance-based rule (no cross-content sha256/size+basename
// matching; that's ExportCopies' job, since exports have no provenance
// table of their own). A segment absent from the returned map has no
// copies.
func (d *DB) SegmentCopies(ctx context.Context, segmentIDs []int64) (map[int64][]CopyRef, error) {
	out := map[int64][]CopyRef{}
	if len(segmentIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(segmentIDs))
	args := make([]any, len(segmentIDs))
	for i, id := range segmentIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(placeholders, ",")

	type rawCopy struct {
		segID   int64
		shareID int64
		relPath string
	}
	var raw []rawCopy
	shareIDSet := map[int64]bool{}
	seen := map[int64]map[int64]bool{} // segID -> shareID -> already added

	addRaw := func(segID, shareID int64, relPath string) {
		set, ok := seen[segID]
		if !ok {
			set = map[int64]bool{}
			seen[segID] = set
		}
		if set[shareID] {
			return
		}
		set[shareID] = true
		raw = append(raw, rawCopy{segID: segID, shareID: shareID, relPath: relPath})
		shareIDSet[shareID] = true
	}

	ownRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id, src.share_id, s.rel_path
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.id IN (%s) AND src.share_id IS NOT NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copies: own storage: %w", err)
	}
	for ownRows.Next() {
		var segID, shareID int64
		var relPath string
		if err := ownRows.Scan(&segID, &shareID, &relPath); err != nil {
			ownRows.Close()
			return nil, fmt.Errorf("db: segment copies: own storage: %w", err)
		}
		addRaw(segID, shareID, relPath)
	}
	if err := ownRows.Err(); err != nil {
		ownRows.Close()
		return nil, fmt.Errorf("db: segment copies: own storage: %w", err)
	}
	ownRows.Close()

	iRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT segment_id, share_id, dest_rel_path FROM ingests
		WHERE segment_id IN (%s) AND verified_at IS NOT NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copies: ingests: %w", err)
	}
	for iRows.Next() {
		var segID, shareID int64
		var relPath string
		if err := iRows.Scan(&segID, &shareID, &relPath); err != nil {
			iRows.Close()
			return nil, fmt.Errorf("db: segment copies: ingests: %w", err)
		}
		addRaw(segID, shareID, relPath)
	}
	if err := iRows.Err(); err != nil {
		iRows.Close()
		return nil, fmt.Errorf("db: segment copies: ingests: %w", err)
	}
	iRows.Close()

	pRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT indexed_segment_id, share_id, dest_rel_path FROM dashcam_pulls
		WHERE indexed_segment_id IN (%s) AND verified_at IS NOT NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment copies: pulls: %w", err)
	}
	for pRows.Next() {
		var segID, shareID int64
		var relPath string
		if err := pRows.Scan(&segID, &shareID, &relPath); err != nil {
			pRows.Close()
			return nil, fmt.Errorf("db: segment copies: pulls: %w", err)
		}
		addRaw(segID, shareID, relPath)
	}
	if err := pRows.Err(); err != nil {
		pRows.Close()
		return nil, fmt.Errorf("db: segment copies: pulls: %w", err)
	}
	pRows.Close()

	shareIDs := make([]int64, 0, len(shareIDSet))
	for id := range shareIDSet {
		shareIDs = append(shareIDs, id)
	}
	labels, err := d.shareLabelKind(ctx, shareIDs)
	if err != nil {
		return nil, err
	}

	for _, r := range raw {
		lk := labels[r.shareID]
		out[r.segID] = append(out[r.segID], CopyRef{
			StorageID: strconv.FormatInt(r.shareID, 10),
			Label:     lk[0],
			Kind:      lk[1],
			RelPath:   r.relPath,
			Verified:  true,
		})
	}
	return out, nil
}

// SegmentsOnSSD returns, for each id in segmentIDs, whether that segment's
// footage is still present on the recorder's SSD: either the segment's
// own source is kind=ssd and it hasn't been archived (archived_at IS
// NULL), or a different segment sharing its sha256 is an un-archived
// ssd-sourced row. A segment absent from the returned map is not on the
// SSD (either never was, or is now archived with no matching sha256).
func (d *DB) SegmentsOnSSD(ctx context.Context, segmentIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(segmentIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(segmentIDs))
	args := make([]any, len(segmentIDs))
	for i, id := range segmentIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(placeholders, ",")

	ownRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.id IN (%s) AND src.kind = 'ssd' AND s.archived_at IS NULL
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segments on ssd: own: %w", err)
	}
	for ownRows.Next() {
		var id int64
		if err := ownRows.Scan(&id); err != nil {
			ownRows.Close()
			return nil, fmt.Errorf("db: segments on ssd: own: %w", err)
		}
		out[id] = true
	}
	if err := ownRows.Err(); err != nil {
		ownRows.Close()
		return nil, fmt.Errorf("db: segments on ssd: own: %w", err)
	}
	ownRows.Close()

	shaRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, sha256 FROM segments
		WHERE id IN (%s) AND sha256 IS NOT NULL AND sha256 != ''
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: segments on ssd: sha lookup: %w", err)
	}
	idToSha := map[int64]string{}
	shaSet := map[string]bool{}
	for shaRows.Next() {
		var id int64
		var sha string
		if err := shaRows.Scan(&id, &sha); err != nil {
			shaRows.Close()
			return nil, fmt.Errorf("db: segments on ssd: sha lookup: %w", err)
		}
		idToSha[id] = sha
		shaSet[sha] = true
	}
	if err := shaRows.Err(); err != nil {
		shaRows.Close()
		return nil, fmt.Errorf("db: segments on ssd: sha lookup: %w", err)
	}
	shaRows.Close()

	if len(shaSet) == 0 {
		return out, nil
	}
	shas := make([]string, 0, len(shaSet))
	for sha := range shaSet {
		shas = append(shas, sha)
	}
	shaPlaceholders := make([]string, len(shas))
	shaArgs := make([]any, len(shas))
	for i, sha := range shas {
		shaPlaceholders[i] = "?"
		shaArgs[i] = sha
	}
	matchRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT DISTINCT s.sha256 FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE src.kind = 'ssd' AND s.archived_at IS NULL AND s.sha256 IN (%s)
	`, strings.Join(shaPlaceholders, ",")), shaArgs...)
	if err != nil {
		return nil, fmt.Errorf("db: segments on ssd: match: %w", err)
	}
	matchedSha := map[string]bool{}
	for matchRows.Next() {
		var sha string
		if err := matchRows.Scan(&sha); err != nil {
			matchRows.Close()
			return nil, fmt.Errorf("db: segments on ssd: match: %w", err)
		}
		matchedSha[sha] = true
	}
	if err := matchRows.Err(); err != nil {
		matchRows.Close()
		return nil, fmt.Errorf("db: segments on ssd: match: %w", err)
	}
	matchRows.Close()

	for id, sha := range idToSha {
		if matchedSha[sha] {
			out[id] = true
		}
	}
	return out, nil
}

// ExportCopies returns, for each id in exportIDs, the list of persistent
// storages holding a copy of that export's file: its own storage (the
// share it was sent to, or the cache) plus any other storage holding a
// file the precomputed duplicate report already links it to by sha256 or
// size+basename (see DuplicateReportRefresh) — exports have no provenance
// table of their own, so cross-storage copies can only be found through
// that report, which callers should keep reasonably fresh (a scan/hash
// job refreshes it; POST /api/duplicates/refresh does so on demand). An
// export absent from the returned map either doesn't exist or has no path
// yet (still running/failed).
func (d *DB) ExportCopies(ctx context.Context, exportIDs []int64) (map[int64][]CopyRef, error) {
	out := map[int64][]CopyRef{}
	if len(exportIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(exportIDs))
	args := make([]any, len(exportIDs))
	for i, id := range exportIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(placeholders, ",")

	type ownRow struct {
		shareID *int64
		path    string
	}
	own := map[int64]ownRow{}
	rows, err := d.read.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, share_id, path FROM exports WHERE id IN (%s) AND path IS NOT NULL`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: export copies: own: %w", err)
	}
	for rows.Next() {
		var id int64
		var shareID sql.NullInt64
		var p string
		if err := rows.Scan(&id, &shareID, &p); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: export copies: own: %w", err)
		}
		r := ownRow{path: p}
		if shareID.Valid {
			r.shareID = &shareID.Int64
		}
		own[id] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("db: export copies: own: %w", err)
	}
	rows.Close()

	shareIDSet := map[int64]bool{}
	for _, r := range own {
		if r.shareID != nil {
			shareIDSet[*r.shareID] = true
		}
	}
	shareIDs := make([]int64, 0, len(shareIDSet))
	for id := range shareIDSet {
		shareIDs = append(shareIDs, id)
	}
	labels, err := d.shareLabelKind(ctx, shareIDs)
	if err != nil {
		return nil, err
	}

	ownStorageKey := map[int64]string{}
	for id, r := range own {
		var ref CopyRef
		var key string
		if r.shareID != nil {
			lk := labels[*r.shareID]
			key = "share:" + strconv.FormatInt(*r.shareID, 10)
			ref = CopyRef{StorageID: strconv.FormatInt(*r.shareID, 10), Label: lk[0], Kind: lk[1], RelPath: path.Base(r.path), Verified: true}
		} else {
			key = "cache"
			ref = CopyRef{StorageID: "cache", Label: "cache", Kind: "cache", RelPath: path.Base(r.path), Verified: true}
		}
		ownStorageKey[id] = key
		out[id] = append(out[id], ref)
	}

	// Cross-storage members via the precomputed duplicate report: find
	// which group(s) each export belongs to, then pull every other member
	// of those groups.
	groupRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT m.ref_id, m.group_key, g.kind FROM duplicate_group_members m
		JOIN duplicate_groups g ON g.group_key = m.group_key
		WHERE m.ref_kind = 'export' AND m.ref_id IN (%s)
	`, inClause), args...)
	if err != nil {
		return nil, fmt.Errorf("db: export copies: groups: %w", err)
	}
	exportGroups := map[int64][]string{}
	groupKindByKey := map[string]string{}
	var groupKeys []string
	seenGroupKey := map[string]bool{}
	for groupRows.Next() {
		var refID int64
		var groupKey, kind string
		if err := groupRows.Scan(&refID, &groupKey, &kind); err != nil {
			groupRows.Close()
			return nil, fmt.Errorf("db: export copies: groups: %w", err)
		}
		exportGroups[refID] = append(exportGroups[refID], groupKey)
		groupKindByKey[groupKey] = kind
		if !seenGroupKey[groupKey] {
			seenGroupKey[groupKey] = true
			groupKeys = append(groupKeys, groupKey)
		}
	}
	if err := groupRows.Err(); err != nil {
		groupRows.Close()
		return nil, fmt.Errorf("db: export copies: groups: %w", err)
	}
	groupRows.Close()

	if len(groupKeys) == 0 {
		return out, nil
	}

	gPlaceholders := make([]string, len(groupKeys))
	gArgs := make([]any, len(groupKeys))
	for i, k := range groupKeys {
		gPlaceholders[i] = "?"
		gArgs[i] = k
	}
	memberRows, err := d.read.QueryContext(ctx, fmt.Sprintf(`
		SELECT group_key, share_id, storage_key, rel_path FROM duplicate_group_members
		WHERE group_key IN (%s)
	`, strings.Join(gPlaceholders, ",")), gArgs...)
	if err != nil {
		return nil, fmt.Errorf("db: export copies: members: %w", err)
	}
	type member struct {
		shareID    sql.NullInt64
		storageKey string
		relPath    string
	}
	membersByGroup := map[string][]member{}
	memberShareIDSet := map[int64]bool{}
	for memberRows.Next() {
		var groupKey, storageKey, relPath string
		var shareID sql.NullInt64
		if err := memberRows.Scan(&groupKey, &shareID, &storageKey, &relPath); err != nil {
			memberRows.Close()
			return nil, fmt.Errorf("db: export copies: members: %w", err)
		}
		membersByGroup[groupKey] = append(membersByGroup[groupKey], member{shareID: shareID, storageKey: storageKey, relPath: relPath})
		if shareID.Valid {
			memberShareIDSet[shareID.Int64] = true
		}
	}
	if err := memberRows.Err(); err != nil {
		memberRows.Close()
		return nil, fmt.Errorf("db: export copies: members: %w", err)
	}
	memberRows.Close()

	var memberShareIDs []int64
	for id := range memberShareIDSet {
		if _, ok := labels[id]; !ok {
			memberShareIDs = append(memberShareIDs, id)
		}
	}
	if len(memberShareIDs) > 0 {
		more, err := d.shareLabelKind(ctx, memberShareIDs)
		if err != nil {
			return nil, err
		}
		for id, lk := range more {
			labels[id] = lk
		}
	}

	for exportID, keys := range exportGroups {
		added := map[string]bool{ownStorageKey[exportID]: true}
		for _, key := range keys {
			verified := groupKindByKey[key] == DuplicateGroupKindSHA256
			for _, m := range membersByGroup[key] {
				if added[m.storageKey] {
					continue
				}
				added[m.storageKey] = true
				var ref CopyRef
				if m.storageKey == "cache" {
					ref = CopyRef{StorageID: "cache", Label: "cache", Kind: "cache", RelPath: m.relPath, Verified: verified}
				} else if m.shareID.Valid {
					lk := labels[m.shareID.Int64]
					ref = CopyRef{StorageID: strconv.FormatInt(m.shareID.Int64, 10), Label: lk[0], Kind: lk[1], RelPath: m.relPath, Verified: verified}
				} else {
					continue
				}
				out[exportID] = append(out[exportID], ref)
			}
		}
	}
	return out, nil
}
