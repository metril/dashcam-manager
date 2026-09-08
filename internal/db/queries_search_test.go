package db

import (
	"context"
	"testing"
)

// seedSearchFixture inserts one source/trip/segment with a full set of
// exploded detection rows (detections, det_objects, det_plates,
// det_vehicles, search_terms — the shape scan.IndexDetections produces) plus
// a second, unrelated segment/detection so filters have something to
// exclude. Returns the segment id search hits should land on.
func seedSearchFixture(t *testing.T, d *DB) (segID int64) {
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

	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	tripID := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260301_100000', '2026-03-01T10:00:00Z', 'recording_20260301_100000', 1, 1000)`, sourceID)
	segID = must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/01/recording_20260301_100000/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', '2026-03-01T10:00:30Z', 30, 1000, 1, 0)`, sourceID, tripID)

	detID := must(`INSERT INTO detections (segment_id, pts_offset, speed_mps) VALUES (?, 12.5, 15.0)`, segID)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2) VALUES (?, 'car', 0.92, 0.1, 0.1, 0.5, 0.5)`, detID)
	must(`INSERT INTO det_plates (det_id, text, confidence, x1, y1, x2, y2) VALUES (?, 'ABC123', 0.81, 0.2, 0.2, 0.4, 0.4)`, detID)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence, x1, y1, x2, y2) VALUES (?, 'sedan', 'Toyota', 0.7, 0.1, 0.1, 0.5, 0.5)`, detID)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'class', 'car')`, segID, detID)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'plate', 'ABC123')`, segID, detID)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'make', 'Toyota')`, segID, detID)

	// An unrelated segment/detection with a different class/make/plate/date
	// so filters have something real to exclude.
	tripID2 := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260305_090000', '2026-03-05T09:00:00Z', 'recording_20260305_090000', 1, 1000)`, sourceID)
	segID2 := must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/05/recording_20260305_090000/segment_00000.mp4', 0, '2026-03-05T09:00:00Z', '2026-03-05T09:00:30Z', 30, 1000, 1, 0)`, sourceID, tripID2)
	detID2 := must(`INSERT INTO detections (segment_id, pts_offset, speed_mps) VALUES (?, 5.0, 2.0)`, segID2)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2) VALUES (?, 'person', 0.6, 0.1, 0.1, 0.2, 0.2)`, detID2)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence, x1, y1, x2, y2) VALUES (?, 'truck', 'Ford', 0.5, 0.1, 0.1, 0.5, 0.5)`, detID2)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'class', 'person')`, segID2, detID2)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'make', 'Ford')`, segID2, detID2)

	// GPS track for the first trip.
	must(`INSERT INTO segment_gps (segment_id, t, lat, lon, speed_mps, heading) VALUES (?, '2026-03-01T10:00:00Z', 37.1, -122.1, 10.0, 90)`, segID)
	must(`INSERT INTO segment_gps (segment_id, t, lat, lon, speed_mps, heading) VALUES (?, '2026-03-01T10:00:05Z', 37.2, -122.2, 15.0, 91)`, segID)

	return segID
}

func TestSearchByPlate(t *testing.T) {
	d := openTest(t)
	segID := seedSearchFixture(t, d)
	ctx := context.Background()

	results, _, err := d.Search(ctx, SearchFilter{Plate: "ABC"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(results), results)
	}
	if results[0].Segment.ID != segID {
		t.Fatalf("got segment %d, want %d", results[0].Segment.ID, segID)
	}
	if results[0].Trip == nil || results[0].Trip.TripKey != "recording_20260301_100000" {
		t.Fatalf("unexpected trip: %+v", results[0].Trip)
	}
	foundPlateHit := false
	for _, h := range results[0].Hits {
		if h.Kind == "plate" && h.Text == "ABC123" {
			foundPlateHit = true
			if h.PtsOffset != 12.5 {
				t.Errorf("plate hit pts_offset = %v, want 12.5", h.PtsOffset)
			}
		}
	}
	if !foundPlateHit {
		t.Fatalf("no plate hit in %+v", results[0].Hits)
	}
}

func TestSearchByClassAndMake(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	results, _, err := d.Search(ctx, SearchFilter{Class: "car", Make: "Toyota"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	// A class that exists but not paired with this make should exclude it.
	results, _, err = d.Search(ctx, SearchFilter{Class: "car", Make: "Ford"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestSearchFreeTextQ(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	results, _, err := d.Search(ctx, SearchFilter{Q: "Toyota"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestSearchMinSpeedAndConf(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	// speed_mps=15 on the seeded detection; a higher floor excludes it.
	results, _, err := d.Search(ctx, SearchFilter{Class: "car", MinSpeedMS: 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (min_speed too high)", len(results))
	}

	results, _, err = d.Search(ctx, SearchFilter{Class: "car", MinSpeedMS: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	results, _, err = d.Search(ctx, SearchFilter{Class: "person", MinConf: 0.9})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (min_conf too high for person@0.6)", len(results))
	}
}

func TestSearchByType(t *testing.T) {
	d := openTest(t)
	segID := seedSearchFixture(t, d)
	ctx := context.Background()

	results, _, err := d.Search(ctx, SearchFilter{Type: "sedan"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Segment.ID != segID {
		t.Fatalf("got %+v, want segment %d", results, segID)
	}
	foundTypeHit := false
	for _, h := range results[0].Hits {
		if h.Kind == "type" && h.Text == "sedan" {
			foundTypeHit = true
		}
	}
	if !foundTypeHit {
		t.Fatalf("no type hit in %+v", results[0].Hits)
	}

	results, _, err = d.Search(ctx, SearchFilter{Type: "truck"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (the unrelated segment)", len(results))
	}
}

func TestSearchDateRange(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	results, _, err := d.Search(ctx, SearchFilter{Class: "car", From: "2026-03-02", To: "2026-03-10"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (car segment is on 2026-03-01, outside range)", len(results))
	}

	results, _, err = d.Search(ctx, SearchFilter{Class: "car", From: "2026-02-25", To: "2026-03-02"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestTripGPSTrack(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	// tripID isn't returned by the fixture helper, so look it up.
	var tripID int64
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM trips WHERE trip_key = 'recording_20260301_100000'`).Scan(&tripID); err != nil {
		t.Fatalf("lookup trip: %v", err)
	}

	pts, err := d.TripGPSTrack(ctx, tripID)
	if err != nil {
		t.Fatalf("TripGPSTrack: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2", len(pts))
	}
	if pts[0].Lat != 37.1 || pts[1].Lat != 37.2 {
		t.Fatalf("unexpected points: %+v", pts)
	}
}
