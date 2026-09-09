package db

import (
	"context"
	"reflect"
	"testing"
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

func TestRewriteQAsStructural(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedVocabFixture(t, d)

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
