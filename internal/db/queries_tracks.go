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

// DetTrackIDsForSegment returns, per kind, the track_id column of every
// det_objects/det_plates/det_vehicles row belonging to segID, each ordered
// by that row's own id ascending. Row id order is also insertion order
// (scan.IndexDetections builds objRows/plateRows/vehRows by iterating
// d.DetectionFrames in order and appending each frame's objects/plates/
// vehicles in their on-disk order, then flushes them as ordered multi-row
// INSERTs), so a caller zipping these slices onto scan.ParseDetectionsBytes'
// flattened Objects/Plates/Vehicles (walked in that same frame/index order)
// recovers each box's track_id with no shared key against the sidecar
// JSON, which carries no persistent per-box id. A box BuildTracks assigned
// to no track keeps a nil entry rather than being dropped, so slice length
// always matches row count.
func (d *DB) DetTrackIDsForSegment(ctx context.Context, segID int64) (objects, plates, vehicles []*int64, err error) {
	query := func(table string) ([]*int64, error) {
		rows, err := d.Read().QueryContext(ctx, fmt.Sprintf(
			`SELECT o.track_id FROM %s o JOIN detections dt ON dt.id = o.det_id
			 WHERE dt.segment_id = ? ORDER BY o.id`, table), segID)
		if err != nil {
			return nil, fmt.Errorf("db: det track ids for segment %d (%s): %w", segID, table, err)
		}
		defer rows.Close()

		out := []*int64{}
		for rows.Next() {
			var tid sql.NullInt64
			if err := rows.Scan(&tid); err != nil {
				return nil, fmt.Errorf("db: det track ids for segment %d (%s): scan: %w", segID, table, err)
			}
			if tid.Valid {
				v := tid.Int64
				out = append(out, &v)
			} else {
				out = append(out, nil)
			}
		}
		return out, rows.Err()
	}

	if objects, err = query("det_objects"); err != nil {
		return nil, nil, nil, err
	}
	if plates, err = query("det_plates"); err != nil {
		return nil, nil, nil, err
	}
	if vehicles, err = query("det_vehicles"); err != nil {
		return nil, nil, nil, err
	}
	return objects, plates, vehicles, nil
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
