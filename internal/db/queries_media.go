package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SegmentMedia is the subset of a segment's row needed to locate and serve
// its underlying video file: the source's root_path (so the handler can
// join+validate rel_path against it) plus enough metadata (bytes, mtime)
// to build a stable ETag without a second stat-like round trip.
type SegmentMedia struct {
	ID         int64
	SourceRoot string
	RelPath    string
	Bytes      int64
	MTime      *int64
	// ArchivedAt is non-nil once a re-scan found this segment's file gone
	// from its source with a verified ingests row backing it (see the
	// segments table's archived_at doc comment in schema.go) — its file is
	// no longer expected to exist at SourceRoot+RelPath, so media/thumbnail
	// handlers should refuse with a clear "archived to <share>" message
	// instead of a generic "not found on disk".
	ArchivedAt *string
	// ArchivedToShare is the name of a share holding a verified copy, for
	// that message — best-effort (picks any one verified ingest if more
	// than one share has a copy) and nil if ArchivedAt is nil.
	ArchivedToShare *string
}

// SegmentMediaGet returns the source root path and rel_path for the
// segment with the given id, or ErrNotFound.
func (d *DB) SegmentMediaGet(ctx context.Context, id int64) (SegmentMedia, error) {
	var m SegmentMedia
	err := d.read.QueryRowContext(ctx, `
		SELECT s.id, src.root_path, s.rel_path, s.bytes, s.mtime, s.archived_at,
			(SELECT sh.name FROM ingests i JOIN shares sh ON sh.id = i.share_id
			 WHERE i.segment_id = s.id AND i.verified_at IS NOT NULL
			 ORDER BY i.verified_at LIMIT 1)
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.id = ?
	`, id).Scan(&m.ID, &m.SourceRoot, &m.RelPath, &m.Bytes, &m.MTime, &m.ArchivedAt, &m.ArchivedToShare)
	if errors.Is(err, sql.ErrNoRows) {
		return SegmentMedia{}, ErrNotFound
	}
	if err != nil {
		return SegmentMedia{}, fmt.Errorf("db: segment media get %d: %w", id, err)
	}
	return m, nil
}

// TimelineSegment is one segment's placement within a trip's timeline
// strip: its absolute start time, offset from the trip's started_at, and a
// cheap per-segment detection-density count (rather than per-second
// buckets — segments are already ~30s units, so a single count per segment
// is enough resolution for a heat bar without a second detections query).
type TimelineSegment struct {
	ID        int64   `json:"id"`
	Camera    string  `json:"camera"`
	Seq       int     `json:"seq"`
	StartTime string  `json:"start_time"`
	OffsetS   float64 `json:"offset_s"`
	DurationS float64 `json:"duration_s"`
	Count     int     `json:"count"`
}

// TripTimeline is the response for GET /api/trips/{id}/timeline.
type TripTimeline struct {
	TripID    int64             `json:"trip_id"`
	StartedAt string            `json:"started_at"`
	DurationS float64           `json:"duration_s"`
	Segments  []TimelineSegment `json:"segments"`
}

// TripTimelineGet builds the detection-density timeline for a trip, or
// ErrNotFound if the trip doesn't exist. OffsetS is seconds from the
// trip's started_at to each segment's start_time, computed in SQL
// (julianday difference) so the frontend doesn't need to parse timestamps
// itself just to position segments on the strip.
func (d *DB) TripTimelineGet(ctx context.Context, tripID int64) (TripTimeline, error) {
	var t TripTimeline
	var durationS sql.NullFloat64
	err := d.read.QueryRowContext(ctx,
		`SELECT id, started_at, duration_s FROM trips WHERE id = ?`, tripID,
	).Scan(&t.TripID, &t.StartedAt, &durationS)
	if errors.Is(err, sql.ErrNoRows) {
		return TripTimeline{}, ErrNotFound
	}
	if err != nil {
		return TripTimeline{}, fmt.Errorf("db: trip timeline %d: %w", tripID, err)
	}
	t.DurationS = durationS.Float64

	rows, err := d.read.QueryContext(ctx, `
		SELECT s.id, s.camera, s.seq, s.start_time, COALESCE(s.duration_s, 0),
			(julianday(s.start_time) - julianday(?)) * 86400.0,
			COALESCE(ss.n_car,0) + COALESCE(ss.n_truck,0) + COALESCE(ss.n_bus,0)
				+ COALESCE(ss.n_person,0) + COALESCE(ss.n_bicycle,0) + COALESCE(ss.n_motorcycle,0)
				+ COALESCE(ss.n_plates,0)
		FROM segments s
		LEFT JOIN segment_summary ss ON ss.segment_id = s.id
		WHERE s.trip_id = ?
		ORDER BY s.camera, s.seq
	`, t.StartedAt, tripID)
	if err != nil {
		return TripTimeline{}, fmt.Errorf("db: trip timeline segments %d: %w", tripID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var seg TimelineSegment
		if err := rows.Scan(&seg.ID, &seg.Camera, &seg.Seq, &seg.StartTime, &seg.DurationS, &seg.OffsetS, &seg.Count); err != nil {
			return TripTimeline{}, fmt.Errorf("db: trip timeline segments %d: %w", tripID, err)
		}
		t.Segments = append(t.Segments, seg)
	}
	if err := rows.Err(); err != nil {
		return TripTimeline{}, fmt.Errorf("db: trip timeline segments %d: %w", tripID, err)
	}
	if t.Segments == nil {
		t.Segments = []TimelineSegment{}
	}
	return t, nil
}
