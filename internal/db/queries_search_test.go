package db

import (
	"context"
	"testing"
)

// seedSearchFixture inserts one source/trip/segment with a full set of
// exploded detection rows (detections, det_objects, det_plates,
// det_vehicles, search_terms, segment_summary — the shape scan.Scan +
// scan.IndexDetections produce) plus a second, unrelated segment/detection
// so filters have something to exclude. segment_summary rows matter here
// because a class-only search answers from segment_summary's n_<class>
// counters (the fast path), not det_objects — see Search's
// isFastPathEligible. Returns the segment id search hits should land on.
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
	must(`INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates)
		VALUES (?, 1, 0, 0, 0, 0, 0, 1)`, segID)

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
	must(`INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates)
		VALUES (?, 0, 0, 0, 1, 0, 0, 0)`, segID2)

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
	// Plate is the only structured filter set, so hits should be
	// plate-only (finding 11): no stray class/make hits.
	for _, h := range results[0].Hits {
		if h.Kind != "plate" {
			t.Errorf("unexpected non-plate hit with plate-only filter: %+v", h)
		}
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

	// class+make together forces the general (non-fast-path) query.
	results, _, err := d.Search(ctx, SearchFilter{Classes: []string{"car"}, Make: "Toyota"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	// Both class and make filters are set, so both kinds of hits should
	// show, but not plate/type (finding 11).
	sawClass, sawMake := false, false
	for _, h := range results[0].Hits {
		switch h.Kind {
		case "class":
			sawClass = true
		case "make":
			sawMake = true
		case "plate", "type":
			t.Errorf("unexpected %s hit with class+make filter: %+v", h.Kind, h)
		}
	}
	if !sawClass || !sawMake {
		t.Fatalf("expected both class and make hits, got %+v", results[0].Hits)
	}

	// A class that exists but not paired with this make should exclude it.
	results, _, err = d.Search(ctx, SearchFilter{Classes: []string{"car"}, Make: "Ford"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0", len(results))
	}
}

func TestSearchClassOnlyFastPath(t *testing.T) {
	d := openTest(t)
	segID := seedSearchFixture(t, d)
	ctx := context.Background()

	// Class-only (+ no date) must go through segment_summary — verify via
	// EXPLAIN QUERY PLAN that det_objects/detections are never touched.
	plan, err := d.Read().QueryContext(ctx, `EXPLAIN QUERY PLAN
		SELECT s.id FROM segments s JOIN segment_summary ss ON ss.segment_id = s.id
		LEFT JOIN trips t ON t.id = s.trip_id
		WHERE (ss.n_car > 0) ORDER BY s.start_time DESC, s.id DESC LIMIT 31 OFFSET 0`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan.Close()

	results, _, err := d.Search(ctx, SearchFilter{Classes: []string{"car"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Segment.ID != segID {
		t.Fatalf("got %+v, want just segment %d", results, segID)
	}
	// Fast path still resolves class hits (from det_objects) for display.
	foundClassHit := false
	for _, h := range results[0].Hits {
		if h.Kind == "class" && h.Text == "car" {
			foundClassHit = true
		}
	}
	if !foundClassHit {
		t.Fatalf("no class hit in fast-path result: %+v", results[0].Hits)
	}
}

func TestSearchMultiClassOR(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	// car (seg1) OR person (seg2) should return both segments.
	results, _, err := d.Search(ctx, SearchFilter{Classes: []string{"car", "person"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (car OR person): %+v", len(results), results)
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

// TestSearchQAndClassCombination covers final-fix-wave item 9: q + class
// together used to fall through to the slow correlated-EXISTS general
// path (~37s against the real ~39k-segment scratch DB); it must now use
// searchQAndClassFastPath and return correct, still-paginated results.
func TestSearchQAndClassCombination(t *testing.T) {
	d := openTest(t)
	segID := seedSearchFixture(t, d)
	ctx := context.Background()

	// "Toyota" free-text-matches only the first (car) segment's search_terms
	// (its make); combined with class=car it should still match that one
	// segment. The isQAndClassFastPathEligible gate requires this to go
	// through the fast path (Q set, Classes set, nothing else).
	f := SearchFilter{Q: "Toyota", Classes: []string{"car"}}
	if !f.isQAndClassFastPathEligible() {
		t.Fatalf("expected q+class filter to be fast-path eligible")
	}
	results, _, err := d.Search(ctx, f)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Segment.ID != segID {
		t.Errorf("Segment.ID = %d, want %d", results[0].Segment.ID, segID)
	}

	// A class that the free-text match's segment doesn't have (person is
	// on the *other* seeded segment, which "Toyota" doesn't match) must
	// exclude it.
	results, _, err = d.Search(ctx, SearchFilter{Q: "Toyota", Classes: []string{"person"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (Toyota's segment has no person)", len(results))
	}

	// Adding a third filter (e.g. min_conf) takes it out of the fast path
	// but must still behave correctly via the general path.
	f2 := SearchFilter{Q: "Toyota", Classes: []string{"car"}, MinConf: 0.5}
	if f2.isQAndClassFastPathEligible() {
		t.Fatalf("expected q+class+min_conf to NOT be fast-path eligible")
	}
	results, _, err = d.Search(ctx, f2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (general path)", len(results))
	}
}

func TestSearchMinSpeedAndConf(t *testing.T) {
	d := openTest(t)
	seedSearchFixture(t, d)
	ctx := context.Background()

	// speed_mps=15 on the seeded detection; a higher floor excludes it.
	results, _, err := d.Search(ctx, SearchFilter{Classes: []string{"car"}, MinSpeedMS: 20})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (min_speed too high)", len(results))
	}

	results, _, err = d.Search(ctx, SearchFilter{Classes: []string{"car"}, MinSpeedMS: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	results, _, err = d.Search(ctx, SearchFilter{Classes: []string{"person"}, MinConf: 0.9})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (min_conf too high for person@0.6)", len(results))
	}

	// min_conf alone (no structural filter) should still work via the
	// OR'd three-way EXISTS.
	results, _, err = d.Search(ctx, SearchFilter{MinConf: 0.65})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (only car@0.92 clears 0.65)", len(results))
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

	results, _, err := d.Search(ctx, SearchFilter{Classes: []string{"car"}, From: "2026-03-02", To: "2026-03-10"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results, want 0 (car segment is on 2026-03-01, outside range)", len(results))
	}

	results, _, err = d.Search(ctx, SearchFilter{Classes: []string{"car"}, From: "2026-02-25", To: "2026-03-02"})
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
