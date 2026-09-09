package db

import (
	"context"
	"database/sql"
	"fmt"
)

// TrackRow is one det_tracks row (schema.go), served as-is in the `tracks`
// array on GET /api/segments/{id}/detections — unfiltered; the client
// applies its detection_filter live. Kind is "object", "plate", or
// "vehicle"; Class is ” for plate rows (plates aren't classified).
// BestConf/BestText/BBox mirror det_tracks' nullable columns: BestText is
// nil for every non-plate track (only plates carry read text), and
// BestConf/BBox are nil only for a pathological track with zero matched
// boxes (BuildTracks never actually produces one, but the column is
// nullable so this stays defensive rather than panicking on a NULL scan).
type TrackRow struct {
	ID       int64       `json:"id"`
	Kind     string      `json:"kind"`
	Class    string      `json:"class"`
	FirstPts float64     `json:"first_pts"`
	LastPts  float64     `json:"last_pts"`
	NFrames  int         `json:"n_frames"`
	BestConf *float64    `json:"best_conf"`
	BestText *string     `json:"best_text"`
	BBox     *[4]float64 `json:"bbox"`
}

// TracksForSegment returns segID's det_tracks rows, ordered by first_pts
// so a client rendering them in a timeline gets a stable, chronological
// order. Returns an empty (non-nil) slice, not an error, when the segment
// has no tracks (no detections, or every candidate track was dropped as
// noise by BuildTracks) — see handleSegmentDetectionsGet, which always
// serves "tracks": [] rather than null.
func (d *DB) TracksForSegment(ctx context.Context, segID int64) ([]TrackRow, error) {
	rows, err := d.Read().QueryContext(ctx, `
		SELECT id, kind, class, first_pts, last_pts, n_frames, best_conf, best_text, x1, y1, x2, y2
		FROM det_tracks WHERE segment_id = ? ORDER BY first_pts`, segID)
	if err != nil {
		return nil, fmt.Errorf("db: tracks for segment %d: %w", segID, err)
	}
	defer rows.Close()

	out := []TrackRow{}
	for rows.Next() {
		var tr TrackRow
		var bestConf sql.NullFloat64
		var bestText sql.NullString
		var x1, y1, x2, y2 sql.NullFloat64
		if err := rows.Scan(&tr.ID, &tr.Kind, &tr.Class, &tr.FirstPts, &tr.LastPts, &tr.NFrames, &bestConf, &bestText, &x1, &y1, &x2, &y2); err != nil {
			return nil, fmt.Errorf("db: tracks for segment %d: scan: %w", segID, err)
		}
		if bestConf.Valid {
			v := bestConf.Float64
			tr.BestConf = &v
		}
		if bestText.Valid {
			v := bestText.String
			tr.BestText = &v
		}
		if x1.Valid && y1.Valid && x2.Valid && y2.Valid {
			bbox := [4]float64{x1.Float64, y1.Float64, x2.Float64, y2.Float64}
			tr.BBox = &bbox
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// SegmentsNeedingTracksBackfill returns ids of segments that have been
// exploded into detections (at least one `detections` row) but never had
// tracks built for them — segments.tracks_built_at IS NULL — i.e. indexed
// by an older IndexDetections, or a database from before schema v5,
// before the IoU tracker (internal/scan/tracks.go) existed.
//
// This deliberately checks the per-segment tracks_built_at marker rather
// than "any det_objects/det_plates/det_vehicles row has a NULL track_id":
// BuildTracks drops short (noise) tracks entirely, so their boxes keep a
// permanently NULL track_id even after the segment has been fully
// processed — a track_id-based query would never converge, reprocessing
// such segments on every single backfill run. tracks_built_at is stamped
// by IndexDetections unconditionally, whether or not any track survived,
// so it converges correctly. Optionally scoped to sourceID (0 = all
// sources).
func (d *DB) SegmentsNeedingTracksBackfill(ctx context.Context, sourceID int64) ([]int64, error) {
	query := `
		SELECT s.id FROM segments s
		WHERE s.tracks_built_at IS NULL
		AND EXISTS (SELECT 1 FROM detections det WHERE det.segment_id = s.id)`
	var args []any
	if sourceID != 0 {
		query += ` AND s.source_id = ?`
		args = append(args, sourceID)
	}

	rows, err := d.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: segments needing tracks backfill: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: segments needing tracks backfill: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
