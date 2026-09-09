package db

import (
	"context"
	"testing"
)

// mkSegForTracks inserts a minimal source/segment/detections fixture for
// SegmentsNeedingTracksBackfill. tracksBuiltAt is the segment's
// tracks_built_at value (nil for NULL, i.e. never processed). objTrackID
// is the track_id on a single det_objects row attached to the segment's
// one detection (nil for NULL, e.g. a box whose track was dropped as
// noise).
func mkSegForTracks(t *testing.T, d *DB, sourceID int64, relPath string, tracksBuiltAt, objTrackID any) (segID int64) {
	t.Helper()
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	segID = must(`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, has_detections, tracks_built_at)
		VALUES (?, 'front', ?, 0, '2026-03-01T10:00:00Z', 1, ?)`, sourceID, relPath, tracksBuiltAt)
	detID := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 0)`, segID)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2, track_id) VALUES (?, 'car', 0.9, 0, 0, 1, 1, ?)`, detID, objTrackID)
	return segID
}

func insertSourceForTracks(t *testing.T, d *DB) int64 {
	t.Helper()
	res, err := d.Write().ExecContext(context.Background(),
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestSegmentsNeedingTracksBackfill(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	sourceID := insertSourceForTracks(t, d)

	needsBackfill := mkSegForTracks(t, d, sourceID, "front/a/segment_00000.mp4", nil, nil)
	alreadyDone := mkSegForTracks(t, d, sourceID, "front/b/segment_00000.mp4", "2026-03-01T10:05:00Z", int64(1))

	ids, err := d.SegmentsNeedingTracksBackfill(ctx, 0)
	if err != nil {
		t.Fatalf("SegmentsNeedingTracksBackfill: %v", err)
	}
	if len(ids) != 1 || ids[0] != needsBackfill {
		t.Fatalf("got %v, want [%d] (not %d)", ids, needsBackfill, alreadyDone)
	}

	// Scoped to a different source: no results.
	otherIDs, err := d.SegmentsNeedingTracksBackfill(ctx, sourceID+999)
	if err != nil {
		t.Fatalf("SegmentsNeedingTracksBackfill (other source): %v", err)
	}
	if len(otherIDs) != 0 {
		t.Errorf("got %v for unrelated source, want none", otherIDs)
	}
}

// TestSegmentsNeedingTracksBackfillZeroBoxSegmentConverges covers a
// segment with detections rows but zero boxes at all (e.g. only
// GPS-bearing frames, no objects/plates/vehicles ever detected): before
// tracks_built_at is stamped it still needs one IndexDetections pass (to
// stamp the marker, even though BuildTracks will produce nothing), so it
// must be reported; once stamped, it must never be reported again — it
// converges rather than being reprocessed forever.
func TestSegmentsNeedingTracksBackfillZeroBoxSegmentConverges(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	sourceID := insertSourceForTracks(t, d)

	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	segID := must(`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, has_detections)
		VALUES (?, 'front', 'front/c/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', 1)`, sourceID)
	must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 0)`, segID)

	ids, err := d.SegmentsNeedingTracksBackfill(ctx, 0)
	if err != nil {
		t.Fatalf("SegmentsNeedingTracksBackfill: %v", err)
	}
	if len(ids) != 1 || ids[0] != segID {
		t.Fatalf("got %v, want [%d] (unprocessed zero-box segment still needs its tracks_built_at stamped)", ids, segID)
	}

	if _, err := d.Write().ExecContext(ctx, `UPDATE segments SET tracks_built_at = '2026-03-01T10:05:00Z' WHERE id = ?`, segID); err != nil {
		t.Fatalf("stamp tracks_built_at: %v", err)
	}

	ids, err = d.SegmentsNeedingTracksBackfill(ctx, 0)
	if err != nil {
		t.Fatalf("SegmentsNeedingTracksBackfill (after stamping): %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %v after stamping tracks_built_at, want none", ids)
	}
}

// TestSegmentsNeedingTracksBackfillConvergesOnNoiseOnlySegments is the
// fix-round-1 regression test for the CRITICAL convergence bug: a segment
// whose only box's track was dropped as noise (n_frames < minTrackFrames)
// keeps a permanently NULL det_objects.track_id — but once IndexDetections
// has stamped tracks_built_at for it, it must NOT be reported as needing
// backfill, or "tracks_backfill" would reprocess it (and every segment
// like it) forever without making progress.
func TestSegmentsNeedingTracksBackfillConvergesOnNoiseOnlySegments(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	sourceID := insertSourceForTracks(t, d)

	// tracks_built_at is set (processed), but the box's track_id is still
	// NULL (its track was dropped as noise) — this must converge, not
	// keep showing up.
	mkSegForTracks(t, d, sourceID, "front/noise/segment_00000.mp4", "2026-03-01T10:05:00Z", nil)

	ids, err := d.SegmentsNeedingTracksBackfill(ctx, 0)
	if err != nil {
		t.Fatalf("SegmentsNeedingTracksBackfill: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("got %v, want none (a NULL track_id alone must not force reprocessing once tracks_built_at is set)", ids)
	}
}

// TestTracksForSegment covers the shape TracksForSegment serves on GET
// /api/segments/{id}/detections: rows ordered by first_pts, nullable
// best_text/best_conf/bbox columns coming back as nil pointers rather than
// zero values, and "no tracks" coming back as an empty (non-nil) slice
// rather than an error.
func TestTracksForSegment(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	sourceID := insertSourceForTracks(t, d)
	segID := mkSegForTracks(t, d, sourceID, "front/tr/segment_00000.mp4", "2026-03-01T10:05:00Z", int64(1))

	// No det_tracks rows yet: empty, non-nil slice.
	tracks, err := d.TracksForSegment(ctx, segID)
	if err != nil {
		t.Fatalf("TracksForSegment (empty): %v", err)
	}
	if tracks == nil || len(tracks) != 0 {
		t.Fatalf("got %v, want empty non-nil slice", tracks)
	}

	must := func(query string, args ...any) {
		if _, err := d.Write().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
	}
	must(`INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf, best_text, x1, y1, x2, y2)
		VALUES (?, 'plate', '', 1.0, 2.0, 5, 0.87, 'ABC123', 0.1, 0.2, 0.3, 0.4)`, segID)
	must(`INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf, best_text, x1, y1, x2, y2)
		VALUES (?, 'object', 'car', 0.0, 3.0, 8, 0.92, NULL, 0.0, 0.0, 1.0, 1.0)`, segID)

	tracks, err = d.TracksForSegment(ctx, segID)
	if err != nil {
		t.Fatalf("TracksForSegment: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2: %+v", len(tracks), tracks)
	}
	// Ordered by first_pts: the object track (first_pts=0) before the
	// plate track (first_pts=1).
	if tracks[0].Kind != "object" || tracks[0].Class != "car" {
		t.Fatalf("tracks[0] = %+v, want object/car first", tracks[0])
	}
	if tracks[0].BestText != nil {
		t.Fatalf("tracks[0].BestText = %v, want nil (object tracks never carry text)", *tracks[0].BestText)
	}
	if tracks[0].BestConf == nil || *tracks[0].BestConf != 0.92 {
		t.Fatalf("tracks[0].BestConf = %v, want 0.92", tracks[0].BestConf)
	}
	if tracks[0].BBox == nil || *tracks[0].BBox != [4]float64{0.0, 0.0, 1.0, 1.0} {
		t.Fatalf("tracks[0].BBox = %v, want [0,0,1,1]", tracks[0].BBox)
	}

	if tracks[1].Kind != "plate" || tracks[1].BestText == nil || *tracks[1].BestText != "ABC123" {
		t.Fatalf("tracks[1] = %+v, want plate/ABC123", tracks[1])
	}
}
