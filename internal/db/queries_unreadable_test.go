package db

import (
	"context"
	"testing"
	"time"
)

func TestSegmentMarkAndClearUnreadable(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	segs, err := d.SegmentsForThumbs(ctx, sourceID, nil, false)
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
	remaining, err := d.SegmentsForThumbs(ctx, sourceID, nil, false)
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
	remaining, err = d.SegmentsForThumbs(ctx, sourceID, nil, false)
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

	segs, err := d.SegmentsForThumbs(ctx, sourceID, nil, false)
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

// TestSegmentsUnreadablePagesByKeysetNotOffset covers final-findings item
// 5: SegmentsUnreadable must page by keyset on (unreadable_at DESC, id
// DESC) rather than OFFSET, so a row cleared between page fetches can't
// shift a later page's contents (the classic "offset pagination skips or
// repeats a row when the underlying set changes mid-pagination" bug).
func TestSegmentsUnreadablePagesByKeysetNotOffset(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	segs, err := d.SegmentsForThumbs(ctx, sourceID, nil, false)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	if len(segs) < 3 {
		t.Fatalf("need at least 3 segments, got %d", len(segs))
	}
	id1, id2, id3 := segs[0].ID, segs[1].ID, segs[2].ID
	// Mark unreadable in order so unreadable_at strictly increases —
	// SegmentsUnreadable orders most-recently-marked first, so the page
	// order should come back id3, id2, id1.
	for _, id := range []int64{id1, id2, id3} {
		if err := d.SegmentMarkUnreadable(ctx, id, "Missing moov atom"); err != nil {
			t.Fatalf("SegmentMarkUnreadable %d: %v", id, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	// Page 1: id3 only.
	page1, cursor1, err := d.SegmentsUnreadable(ctx, 1, "")
	if err != nil {
		t.Fatalf("SegmentsUnreadable page 1: %v", err)
	}
	if len(page1) != 1 || page1[0].ID != id3 {
		t.Fatalf("page1 = %+v, want [id3=%d]", page1, id3)
	}
	if cursor1 == "" {
		t.Fatal("cursor1 empty, want a next-page cursor (2 rows remain)")
	}

	// Row ahead of the cursor (id3) is cleared between page fetches —
	// with keyset pagination this has no effect on page 2's contents,
	// unlike an offset-based scheme which would now skip id2.
	if err := d.SegmentClearUnreadable(ctx, id3); err != nil {
		t.Fatalf("SegmentClearUnreadable: %v", err)
	}

	page2, cursor2, err := d.SegmentsUnreadable(ctx, 1, cursor1)
	if err != nil {
		t.Fatalf("SegmentsUnreadable page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != id2 {
		t.Fatalf("page2 = %+v, want [id2=%d] (clearing id3 must not skip id2)", page2, id2)
	}
	if cursor2 == "" {
		t.Fatal("cursor2 empty, want a next-page cursor (1 row remains)")
	}

	page3, cursor3, err := d.SegmentsUnreadable(ctx, 1, cursor2)
	if err != nil {
		t.Fatalf("SegmentsUnreadable page 3: %v", err)
	}
	if len(page3) != 1 || page3[0].ID != id1 {
		t.Fatalf("page3 = %+v, want [id1=%d]", page3, id1)
	}
	if cursor3 != "" {
		t.Errorf("cursor3 = %q, want empty (no more rows)", cursor3)
	}
}

// TestSegmentsUnreadableRejectsMalformedCursor covers the opaque-cursor
// decode path: a cursor that isn't validly-encoded must error rather than
// panic or silently reinterpret as an offset of 0.
func TestSegmentsUnreadableRejectsMalformedCursor(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, _, err := d.SegmentsUnreadable(ctx, 10, "not-a-valid-cursor!!"); err == nil {
		t.Fatal("SegmentsUnreadable with garbage cursor: want error, got nil")
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
