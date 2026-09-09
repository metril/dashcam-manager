package db

import (
	"context"
	"testing"
)

// mkSegForTracks inserts a minimal source/segment/detections/det_objects
// fixture for SegmentsNeedingTracksBackfill, with a single det_objects row
// whose track_id is trackID (nil for NULL).
func mkSegForTracks(t *testing.T, d *DB, sourceID int64, relPath string, trackID any) (segID int64) {
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
	segID = must(`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, has_detections)
		VALUES (?, 'front', ?, 0, '2026-03-01T10:00:00Z', 1)`, sourceID, relPath)
	detID := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 0)`, segID)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2, track_id) VALUES (?, 'car', 0.9, 0, 0, 1, 1, ?)`, detID, trackID)
	return segID
}

func TestSegmentsNeedingTracksBackfill(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	res, err := d.Write().ExecContext(ctx, `INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	needsBackfill := mkSegForTracks(t, d, sourceID, "front/a/segment_00000.mp4", nil)
	alreadyDone := mkSegForTracks(t, d, sourceID, "front/b/segment_00000.mp4", int64(1))

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

func TestSegmentsNeedingTracksBackfillIgnoresSegmentsWithNoBoxes(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	res, err := d.Write().ExecContext(ctx, `INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	// A segment with detections but zero boxes (e.g. only GPS-bearing
	// frames) must never show up — there's nothing to backfill and it
	// would otherwise get reprocessed by every backfill run.
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
	if len(ids) != 0 {
		t.Errorf("got %v, want none", ids)
	}
}
