package db

import (
	"context"
	"testing"
)

func TestSegmentMarkAndClearUnreadable(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	segs, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	id := segs[0].ID

	if err := d.SegmentMarkUnreadable(ctx, id, "Missing moov atom"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	seg, err := d.SegmentGet(ctx, id)
	if err != nil {
		t.Fatalf("SegmentGet: %v", err)
	}
	if seg.UnreadableReason == nil || *seg.UnreadableReason != "Missing moov atom" {
		t.Errorf("UnreadableReason = %v, want \"Missing moov atom\"", seg.UnreadableReason)
	}
	if seg.UnreadableAt == nil {
		t.Errorf("UnreadableAt = nil, want set")
	}

	// A segment marked unreadable is excluded from the thumbs sweep.
	remaining, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	for _, s := range remaining {
		if s.ID == id {
			t.Errorf("segment %d still returned by SegmentsForThumbs after being marked unreadable", id)
		}
	}

	count, err := d.SegmentsUnreadableCount(ctx)
	if err != nil {
		t.Fatalf("SegmentsUnreadableCount: %v", err)
	}
	if count != 1 {
		t.Errorf("SegmentsUnreadableCount = %d, want 1", count)
	}

	if err := d.SegmentClearUnreadable(ctx, id); err != nil {
		t.Fatalf("SegmentClearUnreadable: %v", err)
	}
	seg, err = d.SegmentGet(ctx, id)
	if err != nil {
		t.Fatalf("SegmentGet: %v", err)
	}
	if seg.UnreadableReason != nil || seg.UnreadableAt != nil {
		t.Errorf("segment still unreadable after clear: %+v", seg)
	}

	// SegmentsForThumbs picks it up again once cleared.
	remaining, err = d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	found := false
	for _, s := range remaining {
		if s.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("segment %d not returned by SegmentsForThumbs after clearing unreadable", id)
	}
}

func TestSegmentsUnreadableListAndRecheck(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	segs, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	if len(segs) < 2 {
		t.Fatalf("need at least 2 segments, got %d", len(segs))
	}
	id1, id2 := segs[0].ID, segs[1].ID

	if err := d.SegmentMarkUnreadable(ctx, id1, "Corrupt header"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}
	if err := d.SegmentMarkUnreadable(ctx, id2, "No decodable frames"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	list, cursor, err := d.SegmentsUnreadable(ctx, 50, "")
	if err != nil {
		t.Fatalf("SegmentsUnreadable: %v", err)
	}
	if cursor != "" {
		t.Errorf("cursor = %q, want empty (only 2 rows)", cursor)
	}
	if len(list) != 2 {
		t.Fatalf("got %d unreadable segments, want 2: %+v", len(list), list)
	}

	// Recheck scoped to just id1.
	ids, err := d.SegmentIDsToRecheck(ctx, []int64{id1, 999999})
	if err != nil {
		t.Fatalf("SegmentIDsToRecheck: %v", err)
	}
	if len(ids) != 1 || ids[0] != id1 {
		t.Errorf("SegmentIDsToRecheck(id1, bogus) = %v, want [%d]", ids, id1)
	}
	if err := d.SegmentsClearUnreadableByIDs(ctx, ids); err != nil {
		t.Fatalf("SegmentsClearUnreadableByIDs: %v", err)
	}
	count, err := d.SegmentsUnreadableCount(ctx)
	if err != nil {
		t.Fatalf("SegmentsUnreadableCount: %v", err)
	}
	if count != 1 {
		t.Errorf("SegmentsUnreadableCount after scoped recheck = %d, want 1", count)
	}

	// Recheck all clears the rest.
	allIDs, err := d.SegmentIDsToRecheck(ctx, nil)
	if err != nil {
		t.Fatalf("SegmentIDsToRecheck(nil): %v", err)
	}
	if len(allIDs) != 1 || allIDs[0] != id2 {
		t.Errorf("SegmentIDsToRecheck(nil) = %v, want [%d]", allIDs, id2)
	}
	n, err := d.SegmentClearUnreadableAll(ctx)
	if err != nil {
		t.Fatalf("SegmentClearUnreadableAll: %v", err)
	}
	if n != 1 {
		t.Errorf("SegmentClearUnreadableAll rows affected = %d, want 1", n)
	}
	count, err = d.SegmentsUnreadableCount(ctx)
	if err != nil {
		t.Fatalf("SegmentsUnreadableCount: %v", err)
	}
	if count != 0 {
		t.Errorf("SegmentsUnreadableCount after recheck all = %d, want 0", count)
	}
}

func TestTripGetIncludesUnreadableFields(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	trip, err := d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	if len(trip.Segments) == 0 {
		t.Fatal("no segments returned")
	}
	segID := trip.Segments[0].ID
	if err := d.SegmentMarkUnreadable(ctx, segID, "Missing moov atom"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	trip, err = d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	if trip.UnreadableCount != 1 {
		t.Errorf("trip.UnreadableCount = %d, want 1", trip.UnreadableCount)
	}
	var found bool
	for _, s := range trip.Segments {
		if s.ID == segID {
			found = true
			if s.UnreadableReason == nil || *s.UnreadableReason != "Missing moov atom" {
				t.Errorf("segment UnreadableReason = %v, want \"Missing moov atom\"", s.UnreadableReason)
			}
		}
	}
	if !found {
		t.Fatalf("segment %d not found in trip detail", segID)
	}
}
