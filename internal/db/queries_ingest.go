package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IngestSegment is one segment considered by internal/ingest as a copy
// candidate: enough to locate the source file (SourceRoot + RelPath) and
// build the destination's relative path (same RelPath, joined onto the
// target share's mount point).
type IngestSegment struct {
	ID         int64
	TripID     *int64
	SourceRoot string
	RelPath    string
	Bytes      int64
	SHA256     *string
}

// IngestCandidates returns every segment belonging to sourceID, optionally
// restricted to tripIDs (empty = every trip), ordered by trip start time
// then camera/seq (via rel_path, which sorts correctly since seq is
// zero-padded) for deterministic, resumable processing order.
func (d *DB) IngestCandidates(ctx context.Context, sourceID int64, tripIDs []int64) ([]IngestSegment, error) {
	query := `
		SELECT s.id, s.trip_id, src.root_path, s.rel_path, s.bytes, s.sha256
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.source_id = ?`
	args := []any{sourceID}

	if len(tripIDs) > 0 {
		placeholders := make([]string, len(tripIDs))
		for i, id := range tripIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += fmt.Sprintf(" AND s.trip_id IN (%s)", strings.Join(placeholders, ","))
	}
	query += ` ORDER BY COALESCE((SELECT t.started_at FROM trips t WHERE t.id = s.trip_id), s.start_time), s.rel_path`

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: ingest candidates: %w", err)
	}
	defer rows.Close()

	var out []IngestSegment
	for rows.Next() {
		var seg IngestSegment
		var sha sql.NullString
		if err := rows.Scan(&seg.ID, &seg.TripID, &seg.SourceRoot, &seg.RelPath, &seg.Bytes, &sha); err != nil {
			return nil, fmt.Errorf("db: ingest candidates: %w", err)
		}
		if sha.Valid {
			seg.SHA256 = &sha.String
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

// IngestRow is a snapshot of one `ingests` row.
type IngestRow struct {
	ID          int64
	SegmentID   int64
	ShareID     int64
	DestRelPath string
	Bytes       int64
	SHA256      string
	VerifiedAt  *string
}

// IngestGet returns the ingests row for (segmentID, shareID), or
// (nil, nil) if none exists yet (the normal case for a segment's first
// ingest attempt — not treated as an error).
func (d *DB) IngestGet(ctx context.Context, segmentID, shareID int64) (*IngestRow, error) {
	var row IngestRow
	var sha sql.NullString
	var verifiedAt sql.NullString
	err := d.read.QueryRowContext(ctx, `
		SELECT id, segment_id, share_id, dest_rel_path, bytes, sha256, verified_at
		FROM ingests WHERE segment_id = ? AND share_id = ?`, segmentID, shareID,
	).Scan(&row.ID, &row.SegmentID, &row.ShareID, &row.DestRelPath, &row.Bytes, &sha, &verifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: ingest get (segment %d, share %d): %w", segmentID, shareID, err)
	}
	if sha.Valid {
		row.SHA256 = sha.String
	}
	if verifiedAt.Valid {
		row.VerifiedAt = &verifiedAt.String
	}
	return &row, nil
}

// IngestRecordAttempt upserts a (segment_id, share_id) row describing a
// copy attempt, clearing verified_at (an in-progress or just-completed
// copy is not verified until IngestMarkVerified is called separately) —
// so a process crash between the copy and the verify step leaves the row
// correctly showing "attempted but not verified" rather than stale
// verified data from a previous run.
func (d *DB) IngestRecordAttempt(ctx context.Context, segmentID, shareID int64, destRelPath string, bytes int64, sha256 string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at)
			VALUES (?, ?, ?, ?, ?, NULL)
			ON CONFLICT(segment_id, share_id) DO UPDATE SET
				dest_rel_path = excluded.dest_rel_path,
				bytes = excluded.bytes,
				sha256 = excluded.sha256,
				verified_at = NULL`,
			segmentID, shareID, destRelPath, bytes, sha256)
		return err
	})
}

// IngestMarkVerified sets verified_at on an existing (segment_id,
// share_id) row (created first via IngestRecordAttempt).
func (d *DB) IngestMarkVerified(ctx context.Context, segmentID, shareID int64, verifiedAt string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE ingests SET verified_at = ? WHERE segment_id = ? AND share_id = ?`,
			verifiedAt, segmentID, shareID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no ingests row for (segment %d, share %d)", segmentID, shareID)
		}
		return nil
	})
}

// SegmentSetSHA256 records a segment's video file hash (computed during
// ingest) on the segments row itself, independent of any particular
// destination share.
func (d *DB) SegmentSetSHA256(ctx context.Context, segmentID int64, sha256 string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE segments SET sha256 = ? WHERE id = ?`, sha256, segmentID)
		return err
	})
}

// CleanupCandidate is one segment eligible for SSD-cleanup consideration:
// it has a verified ingests row, so its SSD original (SourceRoot+RelPath)
// is a candidate for deletion once the handler re-confirms the
// destination still matches.
type CleanupCandidate struct {
	SegmentID    int64
	TripID       *int64
	SourceRoot   string
	RelPath      string
	Bytes        int64
	ShareID      int64
	DestRelPath  string
	IngestBytes  int64
	IngestSHA256 string
}

// CleanupCandidates returns every segment of sourceID with a verified
// ingests row, optionally further restricted to tripIDs and/or segmentIDs
// (both empty = every verified segment of that source — "all verified" per
// the M10 brief, scoped to one source rather than every source in the DB).
// sourceID is required — a cleanup that could silently span every scanned
// source (SSD and shares alike) would be far too easy to trigger by
// accident, and cleanup only ever makes sense against a specific SSD
// source in the first place.
func (d *DB) CleanupCandidates(ctx context.Context, sourceID int64, tripIDs []int64, segmentIDs []int64) ([]CleanupCandidate, error) {
	if sourceID == 0 {
		return nil, fmt.Errorf("db: cleanup candidates: source_id is required")
	}

	query := `
		SELECT s.id, s.trip_id, src.root_path, s.rel_path, s.bytes,
			i.share_id, i.dest_rel_path, i.bytes, i.sha256
		FROM ingests i
		JOIN segments s ON s.id = i.segment_id
		JOIN sources src ON src.id = s.source_id
		WHERE i.verified_at IS NOT NULL AND s.source_id = ?`
	args := []any{sourceID}
	if len(tripIDs) > 0 {
		placeholders := make([]string, len(tripIDs))
		for i, id := range tripIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += fmt.Sprintf(" AND s.trip_id IN (%s)", strings.Join(placeholders, ","))
	}
	if len(segmentIDs) > 0 {
		placeholders := make([]string, len(segmentIDs))
		for i, id := range segmentIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += fmt.Sprintf(" AND s.id IN (%s)", strings.Join(placeholders, ","))
	}
	query += ` ORDER BY s.rel_path`

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: cleanup candidates: %w", err)
	}
	defer rows.Close()

	var out []CleanupCandidate
	for rows.Next() {
		var c CleanupCandidate
		var sha sql.NullString
		if err := rows.Scan(&c.SegmentID, &c.TripID, &c.SourceRoot, &c.RelPath, &c.Bytes,
			&c.ShareID, &c.DestRelPath, &c.IngestBytes, &sha); err != nil {
			return nil, fmt.Errorf("db: cleanup candidates: %w", err)
		}
		if sha.Valid {
			c.IngestSHA256 = sha.String
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SSDIngestStats summarizes GET /api/ssd/status's segment counters for a
// source (the SSD, normally): SegmentsTotal is every segment indexed for
// it; SegmentsIngested is how many have at least one verified ingests row
// (to any share); SegmentsNotIngested is the remainder (found on disk and
// scanned into `segments`, but not yet successfully ingested anywhere) —
// named for what it actually counts, not "unindexed" (every one of these
// segments is, by definition, already indexed in the DB).
type SSDIngestStats struct {
	SegmentsTotal       int
	SegmentsIngested    int
	SegmentsNotIngested int
}

// SSDIngestStatsGet computes SSDIngestStats for sourceID.
func (d *DB) SSDIngestStatsGet(ctx context.Context, sourceID int64) (SSDIngestStats, error) {
	var stats SSDIngestStats
	err := d.read.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COUNT(*) FILTER (WHERE EXISTS (
				SELECT 1 FROM ingests i WHERE i.segment_id = s.id AND i.verified_at IS NOT NULL
			))
		FROM segments s
		WHERE s.source_id = ?`, sourceID,
	).Scan(&stats.SegmentsTotal, &stats.SegmentsIngested)
	if err != nil {
		return SSDIngestStats{}, fmt.Errorf("db: ssd ingest stats %d: %w", sourceID, err)
	}
	stats.SegmentsNotIngested = stats.SegmentsTotal - stats.SegmentsIngested
	return stats, nil
}

// IngestTripSummary is one trip's ingest progress for a specific target
// share, used by the Ingest page's trip table.
type IngestTripSummary struct {
	TripID        int64  `json:"trip_id"`
	TripKey       string `json:"trip_key"`
	StartedAt     string `json:"started_at"`
	SegmentCount  int    `json:"segment_count"`
	Bytes         int64  `json:"bytes"`
	IngestedCount int    `json:"ingested_count"` // has an ingests row (any state) for target share
	VerifiedCount int    `json:"verified_count"` // verified_at set for target share
}

// IngestTripSummariesGet returns per-trip ingest progress against
// targetShareID for every trip on sourceID, most recent first.
func (d *DB) IngestTripSummariesGet(ctx context.Context, sourceID, targetShareID int64) ([]IngestTripSummary, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT t.id, t.trip_key, t.started_at, t.segment_count, t.bytes,
			COUNT(i.id),
			COUNT(CASE WHEN i.verified_at IS NOT NULL THEN 1 END)
		FROM trips t
		JOIN segments s ON s.trip_id = t.id
		LEFT JOIN ingests i ON i.segment_id = s.id AND i.share_id = ?
		WHERE t.source_id = ?
		GROUP BY t.id
		ORDER BY t.started_at DESC`, targetShareID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("db: ingest trip summaries: %w", err)
	}
	defer rows.Close()

	var out []IngestTripSummary
	for rows.Next() {
		var s IngestTripSummary
		if err := rows.Scan(&s.TripID, &s.TripKey, &s.StartedAt, &s.SegmentCount, &s.Bytes,
			&s.IngestedCount, &s.VerifiedCount); err != nil {
			return nil, fmt.Errorf("db: ingest trip summaries: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SegmentIDsWithVerifiedIngest filters ids down to the ones that have at
// least one ingests row with verified_at set — used by internal/scan
// (final-fix-wave item 7) to decide, for a segment whose file has
// disappeared from its source, whether to archive the row (a verified copy
// exists elsewhere) rather than delete it (losing all provenance of a
// segment that was never actually ingested anywhere).
func (d *DB) SegmentIDsWithVerifiedIngest(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	const chunk = 500
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[i:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		args := make([]any, len(part))
		for j, id := range part {
			args[j] = id
		}
		rows, err := d.read.QueryContext(ctx,
			fmt.Sprintf(`SELECT DISTINCT segment_id FROM ingests WHERE verified_at IS NOT NULL AND segment_id IN (%s)`, placeholders),
			args...)
		if err != nil {
			return nil, fmt.Errorf("db: segment ids with verified ingest: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("db: segment ids with verified ingest: %w", err)
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: segment ids with verified ingest: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// SegmentsArchive stamps archived_at (now, UTC RFC3339Nano) on every
// segment in ids that isn't already archived — see the segments table's
// archived_at doc comment in schema.go. A no-op for an empty ids.
func (d *DB) SegmentsArchive(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		const chunk = 500
		for i := 0; i < len(ids); i += chunk {
			end := i + chunk
			if end > len(ids) {
				end = len(ids)
			}
			part := ids[i:end]
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
			args := make([]any, 0, len(part)+1)
			args = append(args, time.Now().UTC().Format(time.RFC3339Nano))
			for _, id := range part {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE segments SET archived_at = ? WHERE id IN (%s) AND archived_at IS NULL`, placeholders),
				args...); err != nil {
				return fmt.Errorf("db: segments archive: %w", err)
			}
		}
		return nil
	})
}
