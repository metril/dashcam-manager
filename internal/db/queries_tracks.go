package db

import (
	"context"
	"fmt"
)

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
