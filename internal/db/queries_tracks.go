package db

import (
	"context"
	"fmt"
)

// SegmentsNeedingTracksBackfill returns ids of segments that have at least
// one det_objects/det_plates/det_vehicles row with a NULL track_id — i.e.
// segments whose detections were exploded (by an older IndexDetections, or
// a database from before schema v5) before the IoU tracker (schema v5,
// internal/scan/tracks.go) existed. Segments with detections but
// genuinely zero boxes never appear here (there's nothing to backfill),
// so the "tracks_backfill" job never reprocesses them on every run.
// Optionally scoped to sourceID (0 = all sources).
func (d *DB) SegmentsNeedingTracksBackfill(ctx context.Context, sourceID int64) ([]int64, error) {
	query := `
		SELECT DISTINCT s.id FROM segments s
		WHERE (
			EXISTS (SELECT 1 FROM detections det JOIN det_objects o ON o.det_id = det.id WHERE det.segment_id = s.id AND o.track_id IS NULL)
			OR EXISTS (SELECT 1 FROM detections det JOIN det_plates p ON p.det_id = det.id WHERE det.segment_id = s.id AND p.track_id IS NULL)
			OR EXISTS (SELECT 1 FROM detections det JOIN det_vehicles v ON v.det_id = det.id WHERE det.segment_id = s.id AND v.track_id IS NULL)
		)`
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
