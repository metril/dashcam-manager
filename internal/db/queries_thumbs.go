package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ThumbSegment is one segment row plus its source's root_path, as needed
// to locate the source video file and to attribute a generated thumbnail
// back to a trip cover (internal/media.Thumbnail/Sprite operate on
// filesystem paths, not segment ids).
type ThumbSegment struct {
	ID        int64
	Camera    string
	RelPath   string
	RootPath  string
	TripID    *int64
	Seq       int
	DurationS *float64
}

// SegmentsForThumbs returns segments with thumb_status not in ('done',
// 'unreadable'), either every such segment for sourceID (segmentIDs empty)
// or the intersection of segmentIDs with that condition (sourceID ignored
// when segmentIDs is non-empty). Ordered by id for deterministic progress
// reporting. A segment already classified 'unreadable' (see
// SegmentMarkUnreadable) is excluded so a background thumbs job doesn't
// keep re-attempting a file that can never decode; POST /api/segments/
// unreadable/recheck clears the flag (thumb_status back to 'pending')
// before enqueueing a thumbs job for exactly those ids, so a recheck still
// picks them up.
func (d *DB) SegmentsForThumbs(ctx context.Context, sourceID int64, segmentIDs []int64) ([]ThumbSegment, error) {
	query := `
		SELECT s.id, s.camera, s.rel_path, src.root_path, s.trip_id, s.seq, s.duration_s
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.thumb_status NOT IN ('done', 'unreadable')`
	var args []any

	if len(segmentIDs) > 0 {
		placeholders := make([]string, len(segmentIDs))
		for i, id := range segmentIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += fmt.Sprintf(" AND s.id IN (%s)", strings.Join(placeholders, ","))
	} else if sourceID != 0 {
		query += " AND s.source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY s.id"

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: segments for thumbs: %w", err)
	}
	defer rows.Close()

	var out []ThumbSegment
	for rows.Next() {
		var s ThumbSegment
		if err := rows.Scan(&s.ID, &s.Camera, &s.RelPath, &s.RootPath, &s.TripID, &s.Seq, &s.DurationS); err != nil {
			return nil, fmt.Errorf("db: segments for thumbs: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SegmentThumbInfoGet returns the ThumbSegment for a single segment id, or
// ErrNotFound. Used by the on-demand thumbnail/sprite HTTP handlers, which
// don't filter on thumb_status (a caller asking for /thumb.jpg wants it
// regardless of the background job's bookkeeping state).
func (d *DB) SegmentThumbInfoGet(ctx context.Context, id int64) (ThumbSegment, error) {
	var s ThumbSegment
	err := d.read.QueryRowContext(ctx, `
		SELECT s.id, s.camera, s.rel_path, src.root_path, s.trip_id, s.seq, s.duration_s
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.id = ?
	`, id).Scan(&s.ID, &s.Camera, &s.RelPath, &s.RootPath, &s.TripID, &s.Seq, &s.DurationS)
	if errors.Is(err, sql.ErrNoRows) {
		return ThumbSegment{}, ErrNotFound
	}
	if err != nil {
		return ThumbSegment{}, fmt.Errorf("db: segment thumb info %d: %w", id, err)
	}
	return s, nil
}

// SegmentSetThumbStatus updates a segment's thumb_status ('pending',
// 'done', or 'failed').
func (d *DB) SegmentSetThumbStatus(ctx context.Context, id int64, status string) error {
	_, err := d.write.ExecContext(ctx, `UPDATE segments SET thumb_status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("db: segment set thumb status %d: %w", id, err)
	}
	return nil
}

// SegmentSetDurationS corrects a segment's duration_s — used when
// media.Sprite/Thumbnail discover (via ffprobe) that the DB's recorded
// duration is stale relative to the file on disk (e.g. a seek overshooting
// the file's real, shorter duration), so later thumbnail/sprite/seek
// calculations for the same segment use the right value instead of
// repeating the same overshoot.
func (d *DB) SegmentSetDurationS(ctx context.Context, id int64, durationS float64) error {
	_, err := d.write.ExecContext(ctx, `UPDATE segments SET duration_s = ? WHERE id = ?`, durationS, id)
	if err != nil {
		return fmt.Errorf("db: segment set duration_s %d: %w", id, err)
	}
	return nil
}

// TripCoverCandidate returns the id of the lowest-seq front-camera segment
// of tripID whose thumbnail has been generated (thumb_status = 'done'), or
// ok=false if none qualifies yet.
func (d *DB) TripCoverCandidate(ctx context.Context, tripID int64) (segmentID int64, ok bool, err error) {
	err = d.read.QueryRowContext(ctx, `
		SELECT id FROM segments
		WHERE trip_id = ? AND camera = 'front' AND thumb_status = 'done'
		ORDER BY seq ASC LIMIT 1
	`, tripID).Scan(&segmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("db: trip cover candidate %d: %w", tripID, err)
	}
	return segmentID, true, nil
}

// TripSetCoverThumb sets trips.cover_thumb to url, but only if it isn't
// already set — the first front segment thumbnail to land for a trip wins,
// and later thumbs jobs (e.g. a re-scan) never clobber it.
func (d *DB) TripSetCoverThumb(ctx context.Context, tripID int64, url string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE trips SET cover_thumb = ? WHERE id = ? AND cover_thumb IS NULL`, url, tripID)
	if err != nil {
		return fmt.Errorf("db: trip set cover thumb %d: %w", tripID, err)
	}
	return nil
}
