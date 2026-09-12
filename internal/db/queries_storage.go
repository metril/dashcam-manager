// queries_storage.go backs GET /api/storages: the list of persistent
// storages (every shares row, of either kind, plus the export cache
// pseudo-storage) with usage totals. Later tasks (5-10 of the local
// storage plan) extend this file with storage_files/duplicate queries.
package db

import (
	"context"
	"fmt"
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
}

// StoragesList returns every persistent storage — every shares row (kind
// cifs|local), ordered by id, followed by the cache pseudo-storage
// (ShareID nil, Kind "cache") — each with its segment/export usage.
func (d *DB) StoragesList(ctx context.Context) ([]StorageRow, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT sh.id, sh.kind, sh.name, sh.mount_point, sh.enabled, sh.status,
			COALESCE(seg.cnt, 0), COALESCE(seg.bytes, 0),
			COALESCE(exp.cnt, 0), COALESCE(exp.bytes, 0)
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
			&r.SegmentCount, &r.SegmentBytes, &r.ExportCount, &r.ExportBytes); err != nil {
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
