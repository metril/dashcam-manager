package db

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// seedVocabFixture inserts four segments purpose-built for task 9 round 3's
// q->structural rewrite:
//   - segHonda: a Honda vehicle (make-only vocabulary term).
//   - segPerson: a person object (class-only vocabulary term).
//   - segTruckClass: a truck-classed *object*, no truck-typed vehicle.
//   - segTruckType: a truck-typed *vehicle*, no truck-classed object — the
//     segment a naive "prefer class" rewrite for the class+type-ambiguous
//     term "truck" would wrongly drop; both segTruckClass and segTruckType
//     must be found by q=truck for the union rewrite to be correct.
func seedVocabFixture(t *testing.T, d *DB) (segHonda, segPerson, segTruckClass, segTruckType int64) {
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

	newSeg := func(key, start string) int64 {
		tripID := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
			VALUES (?, ?, ?, ?, 1, 1000)`, sourceID, key, start, key)
		segID := must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
			VALUES (?, ?, 'front', ?, 0, ?, ?, 30, 1000, 1, 0)`,
			sourceID, tripID, "front/"+key+"/segment_00000.mp4", start, start)
		must(`INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates)
			VALUES (?, 0, 0, 0, 0, 0, 0, 0)`, segID)
		return segID
	}

	segHonda = newSeg("recording_v1", "2026-04-01T10:00:00Z")
	det1 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, segHonda)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence, x1, y1, x2, y2) VALUES (?, 'sedan', 'Honda', 0.9, 0.1, 0.1, 0.5, 0.5)`, det1)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'make', 'Honda')`, segHonda, det1)

	segPerson = newSeg("recording_v2", "2026-04-02T10:00:00Z")
	det2 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, segPerson)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2) VALUES (?, 'person', 0.9, 0.1, 0.1, 0.5, 0.5)`, det2)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'class', 'person')`, segPerson, det2)
	must(`UPDATE segment_summary SET n_person = 1 WHERE segment_id = ?`, segPerson)

	segTruckClass = newSeg("recording_v3", "2026-04-03T10:00:00Z")
	det3 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, segTruckClass)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2) VALUES (?, 'truck', 0.9, 0.1, 0.1, 0.5, 0.5)`, det3)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'class', 'truck')`, segTruckClass, det3)
	must(`UPDATE segment_summary SET n_truck = 1 WHERE segment_id = ?`, segTruckClass)

	segTruckType = newSeg("recording_v4", "2026-04-04T10:00:00Z")
	det4 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, segTruckType)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence, x1, y1, x2, y2) VALUES (?, 'truck', 'Ford', 0.9, 0.1, 0.1, 0.5, 0.5)`, det4)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'type', 'truck')`, segTruckType, det4)

	return segHonda, segPerson, segTruckClass, segTruckType
}

// warmVocab synchronously (re)builds and publishes the search vocabulary,
// bypassing the async/debounced machinery in queries_vocabulary.go — tests
// need a deterministic snapshot reflecting whatever's been seeded so far,
// not a race against the background goroutine DB.Open kicks off (task 9
// round 4, IMPORTANT 3) against the pre-seed, empty database.
func warmVocab(t *testing.T, d *DB) {
	t.Helper()
	terms, err := d.buildSearchVocabulary(context.Background())
	if err != nil {
		t.Fatalf("buildSearchVocabulary: %v", err)
	}
	d.vocab.mu.Lock()
	d.vocab.terms = terms
	d.vocab.builtAt = time.Now()
	d.vocab.dirty = false
	d.vocab.mu.Unlock()
}

func TestRewriteQAsStructural(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedVocabFixture(t, d)
	warmVocab(t, d)

	tests := []struct {
		name        string
		q           string
		wantRewrite bool
		wantClasses []string
		wantMake    string
		wantType    string
		wantUnion   bool
	}{
		{name: "honda -> make", q: "honda", wantRewrite: true, wantMake: "Honda"},
		{name: "person -> class", q: "person", wantRewrite: true, wantClasses: []string{"person"}},
		{name: "truck -> class+type union", q: "truck", wantRewrite: true, wantUnion: true},
		{name: "TRUCK (case-insensitive) -> class+type union", q: "TRUCK", wantRewrite: true, wantUnion: true},
		{name: "hond -> no rewrite (prefix only, not a vocabulary word)", q: "hond", wantRewrite: false},
		{name: "honda civic -> no rewrite (multi-word)", q: "honda civic", wantRewrite: false},
		{name: "unknownmodel -> no rewrite (not in vocabulary at all)", q: "unknownmodel", wantRewrite: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, ok, err := d.rewriteQAsStructural(ctx, SearchFilter{Q: tc.q})
			if err != nil {
				t.Fatalf("rewriteQAsStructural(%q): %v", tc.q, err)
			}
			if ok != tc.wantRewrite {
				t.Fatalf("rewrote = %v, want %v (out=%+v)", ok, tc.wantRewrite, out)
			}
			if !ok {
				return
			}
			if out.Q != "" {
				t.Errorf("Q not cleared: %q", out.Q)
			}
			if tc.wantUnion {
				if len(out.QRewriteOr) < 2 {
					t.Fatalf("expected a QRewriteOr union of >=2 alternatives, got %+v", out.QRewriteOr)
				}
				return
			}
			if len(out.QRewriteOr) != 0 {
				t.Fatalf("unexpected QRewriteOr for a single-category rewrite: %+v", out.QRewriteOr)
			}
			if !reflect.DeepEqual(out.Classes, tc.wantClasses) {
				t.Errorf("Classes = %v, want %v", out.Classes, tc.wantClasses)
			}
			if out.Make != tc.wantMake {
				t.Errorf("Make = %q, want %q", out.Make, tc.wantMake)
			}
			if out.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", out.Type, tc.wantType)
			}
		})
	}
}

// TestRewriteQAsStructuralBacksOffOnConflict checks that an existing,
// different structural filter for the only matching category blocks the
// rewrite for that category rather than silently overwriting it.
func TestRewriteQAsStructuralBacksOffOnConflict(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedVocabFixture(t, d)
	warmVocab(t, d)

	out, ok, err := d.rewriteQAsStructural(ctx, SearchFilter{Q: "honda", Make: "Ford"})
	if err != nil {
		t.Fatalf("rewriteQAsStructural: %v", err)
	}
	if ok {
		t.Fatalf("expected no rewrite when Make is already set to something else, got %+v", out)
	}
}

// TestSearchQRewriteMatchesMakeFilter is the requested end-to-end check:
// q=honda (rewritten to a make filter) must return the same segment set as
// an explicit make=Honda search.
func TestSearchQRewriteMatchesMakeFilter(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	segHonda, _, _, _ := seedVocabFixture(t, d)
	warmVocab(t, d)

	byQ, _, err := d.Search(ctx, SearchFilter{Q: "honda"})
	if err != nil {
		t.Fatalf("Search(q=honda): %v", err)
	}
	byMake, _, err := d.Search(ctx, SearchFilter{Make: "Honda"})
	if err != nil {
		t.Fatalf("Search(make=Honda): %v", err)
	}

	idsOf := func(rs []SearchResult) []int64 {
		ids := make([]int64, len(rs))
		for i, r := range rs {
			ids[i] = r.Segment.ID
		}
		return ids
	}
	if !reflect.DeepEqual(idsOf(byQ), idsOf(byMake)) {
		t.Fatalf("q=honda segments %v != make=Honda segments %v", idsOf(byQ), idsOf(byMake))
	}
	if len(byQ) != 1 || byQ[0].Segment.ID != segHonda {
		t.Fatalf("unexpected q=honda results: %+v", byQ)
	}
	// The rewritten filter's chips still show the matched make (legacy
	// det_vehicles make branch, unaffected by the rewrite).
	sawMakeHit := false
	for _, h := range byQ[0].Hits {
		if h.Kind == "make" && h.Text == "Honda" {
			sawMakeHit = true
		}
	}
	if !sawMakeHit {
		t.Errorf("expected a make hit chip on the rewritten q=honda result, got %+v", byQ[0].Hits)
	}
}

// TestSearchQRewriteTruckUnionRecall checks that q=truck (rewritten to a
// class+type union) finds a segment matching *either* category, not just
// one — the recall regression a naive "prefer class" rewrite would cause.
func TestSearchQRewriteTruckUnionRecall(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	_, _, segTruckClass, segTruckType := seedVocabFixture(t, d)
	warmVocab(t, d)

	results, _, err := d.Search(ctx, SearchFilter{Q: "truck"})
	if err != nil {
		t.Fatalf("Search(q=truck): %v", err)
	}
	got := map[int64]bool{}
	for _, r := range results {
		got[r.Segment.ID] = true
	}
	if !got[segTruckClass] {
		t.Errorf("q=truck missed the truck-classed-object-only segment %d: %+v", segTruckClass, results)
	}
	if !got[segTruckType] {
		t.Errorf("q=truck missed the truck-typed-vehicle-only segment %d: %+v", segTruckType, results)
	}
}

// TestSearchQRewriteUnionRespectsOtherFilters covers task 9 round 4
// CRITICAL 1: a q-rewrite union (QRewriteOr) used to leave Q, Classes,
// Make, and Type all empty/unset, which made isPlateFastPathEligible
// think a plate+q search was plate-only and silently drop the q
// constraint entirely via searchPlateFastPath. q=truck&plate=<substring>
// must return only segments matching *both* — the union AND the plate,
// not the union OR a plate-only search.
func TestSearchQRewriteUnionRespectsOtherFilters(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	_, _, segTruckClass, segTruckType := seedVocabFixture(t, d)
	warmVocab(t, d)

	must := func(query string, args ...any) {
		if _, err := d.Write().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
	}
	// segTruckClass's detection gets a plate matching "MATCH";
	// segTruckType's gets a different plate that does not.
	var detTruckClass, detTruckType int64
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM detections WHERE segment_id = ?`, segTruckClass).Scan(&detTruckClass); err != nil {
		t.Fatalf("find segTruckClass detection: %v", err)
	}
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM detections WHERE segment_id = ?`, segTruckType).Scan(&detTruckType); err != nil {
		t.Fatalf("find segTruckType detection: %v", err)
	}
	must(`INSERT INTO det_plates (det_id, text, confidence, x1, y1, x2, y2) VALUES (?, 'MATCH99', 0.9, 0.1, 0.1, 0.5, 0.5)`, detTruckClass)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'plate', 'MATCH99')`, segTruckClass, detTruckClass)
	must(`INSERT INTO det_plates (det_id, text, confidence, x1, y1, x2, y2) VALUES (?, 'NOPE001', 0.9, 0.1, 0.1, 0.5, 0.5)`, detTruckType)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'plate', 'NOPE001')`, segTruckType, detTruckType)

	results, _, err := d.Search(ctx, SearchFilter{Q: "truck", Plate: "MATCH"})
	if err != nil {
		t.Fatalf("Search(q=truck, plate=MATCH): %v", err)
	}
	got := map[int64]bool{}
	for _, r := range results {
		got[r.Segment.ID] = true
	}
	if !got[segTruckClass] {
		t.Errorf("expected segTruckClass (truck class + matching plate) in results: %+v", results)
	}
	if got[segTruckType] {
		t.Errorf("segTruckType (truck type but non-matching plate) must be excluded, got %+v", results)
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want exactly 1: %+v", len(results), results)
	}
}

// seedSingleFrameTrackedSegment inserts one segment with exactly one
// analyzed frame containing a car object, run through the real
// scan.IndexDetections-equivalent shape but built directly here (avoiding
// an internal/scan import — see the same pattern used elsewhere in this
// package's tests) with segments.tracks_built_at stamped, matching what
// IndexDetections does for every segment regardless of whether any track
// survived BuildTracks' minTrackFrames=2 noise filter. Returns the segment
// id and its lone detection id.
func seedSingleFrameTrackedSegment(t *testing.T, d *DB) (segID, detID int64) {
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
		VALUES (?, 'recording_v5', '2026-04-05T10:00:00Z', 'recording_v5', 1, 1000)`, sourceID)
	segID = must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps, tracks_built_at)
		VALUES (?, ?, 'front', 'front/recording_v5/segment_00000.mp4', 0, '2026-04-05T10:00:00Z', '2026-04-05T10:00:30Z', 30, 1000, 1, 0, '2026-04-05T10:05:00Z')`,
		sourceID, tripID)
	must(`INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates)
		VALUES (?, 1, 0, 0, 0, 0, 0, 0)`, segID)

	// A single detection frame with a car object and no det_tracks row at
	// all (track_id NULL) — exactly what IndexDetections leaves behind for
	// a box BuildTracks dropped as noise (fewer than minTrackFrames=2
	// frames): the segment is fully tracked (tracks_built_at is set
	// above), but this box was never assigned to a surviving track.
	detID = must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, segID)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2, track_id) VALUES (?, 'car', 0.9, 0.1, 0.1, 0.5, 0.5, NULL)`, detID)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'class', 'car')`, segID, detID)
	// A plate on the same lone frame — used by
	// TestSearchQHitsTrackedSegmentSingleFrameFallback to exercise
	// searchQHits' FTS path with a genuine (non-exact-vocabulary)
	// substring: "car" itself is too short (3 chars) to have any
	// non-exact 3+-char substring, so the class alone can't test this.
	must(`INSERT INTO det_plates (det_id, text, confidence, x1, y1, x2, y2, track_id) VALUES (?, 'ZZZ777', 0.9, 0.1, 0.1, 0.5, 0.5, NULL)`, detID)
	must(`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, 'plate', 'ZZZ777')`, segID, detID)

	return segID, detID
}

// TestSearchTrackedSegmentSingleFrameFallback covers task 9 round 4
// IMPORTANT 2: a fully-tracked segment (tracks_built_at set) whose only
// matching detection was a single frame has zero det_tracks rows for it
// (BuildTracks drops tracks shorter than 2 frames as noise) — without a
// fallback to the raw per-row query, searchStructuralHits would render 0
// chips for a segment that legitimately matches.
func TestSearchTrackedSegmentSingleFrameFallback(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	segID, _ := seedSingleFrameTrackedSegment(t, d)

	results, _, err := d.Search(ctx, SearchFilter{Classes: []string{"car"}})
	if err != nil {
		t.Fatalf("Search(class=car): %v", err)
	}
	var got *SearchResult
	for i := range results {
		if results[i].Segment.ID == segID {
			got = &results[i]
		}
	}
	if got == nil {
		t.Fatalf("segment %d not in results: %+v", segID, results)
	}
	if len(got.Hits) == 0 {
		t.Fatalf("expected a fallback class chip for the single-frame-tracked segment, got zero hits")
	}
	if got.Hits[0].Kind != "class" || got.Hits[0].Text != "car" {
		t.Errorf("unexpected hit: %+v", got.Hits[0])
	}
}

// TestSearchQHitsTrackedSegmentSingleFrameFallback is the same case for
// searchQHits' tracked branch (q hits, not structural hits) — task 9
// round 4 IMPORTANT 2 explicitly calls out "also in searchQHits' tracked
// branch".
func TestSearchQHitsTrackedSegmentSingleFrameFallback(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	segID, _ := seedSingleFrameTrackedSegment(t, d)

	// "ZZZ" is a genuine trigram substring of the seeded plate "ZZZ777"
	// but isn't a vocabulary word at all (plates aren't part of the
	// vocabulary — see rewriteQAsStructural), so it stays on the
	// FTS/searchQHits path.
	results, _, err := d.Search(ctx, SearchFilter{Q: "ZZZ"})
	if err != nil {
		t.Fatalf("Search(q=ZZZ): %v", err)
	}
	var got *SearchResult
	for i := range results {
		if results[i].Segment.ID == segID {
			got = &results[i]
		}
	}
	if got == nil {
		t.Fatalf("segment %d not in q=ZZZ results: %+v", segID, results)
	}
	if len(got.Hits) == 0 {
		t.Fatalf("expected a fallback q-hit chip for the single-frame-tracked segment, got zero hits")
	}
}
