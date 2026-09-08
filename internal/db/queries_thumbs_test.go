package db

import (
	"context"
	"testing"
)

func TestSegmentsForThumbsDefaultsToPending(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	segs, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	// seedBrowseFixture inserts 3 segments, all default thumb_status='pending'.
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3: %+v", len(segs), segs)
	}
	for _, s := range segs {
		if s.RootPath != "/mnt/dashcam" {
			t.Errorf("segment %d RootPath = %q, want /mnt/dashcam", s.ID, s.RootPath)
		}
	}
}

func TestSegmentsForThumbsExcludesDone(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	all, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	if err := d.SegmentSetThumbStatus(ctx, all[0].ID, "done"); err != nil {
		t.Fatalf("SegmentSetThumbStatus: %v", err)
	}

	remaining, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	if len(remaining) != len(all)-1 {
		t.Fatalf("got %d remaining, want %d", len(remaining), len(all)-1)
	}
	for _, s := range remaining {
		if s.ID == all[0].ID {
			t.Errorf("done segment %d still returned", s.ID)
		}
	}
}

func TestSegmentsForThumbsByExplicitIDs(t *testing.T) {
	d := openTest(t)
	sourceID, _, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	all, err := d.SegmentsForThumbs(ctx, sourceID, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	want := all[0].ID

	got, err := d.SegmentsForThumbs(ctx, 0, []int64{want})
	if err != nil {
		t.Fatalf("SegmentsForThumbs by id: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("got %+v, want single segment %d", got, want)
	}
}

func TestSegmentThumbInfoGetNotFound(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, err := d.SegmentThumbInfoGet(ctx, 999); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTripCoverCandidateAndSetCoverThumb(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	segs, err := d.SegmentsForThumbs(ctx, 0, nil)
	if err != nil {
		t.Fatalf("SegmentsForThumbs: %v", err)
	}
	var frontSeg int64
	for _, s := range segs {
		if s.TripID != nil && *s.TripID == trip1 && s.Camera == "front" {
			frontSeg = s.ID
		}
	}
	if frontSeg == 0 {
		t.Fatalf("no front segment found for trip1")
	}

	// No candidate until the front segment's thumb is done.
	if _, ok, err := d.TripCoverCandidate(ctx, trip1); err != nil {
		t.Fatalf("TripCoverCandidate: %v", err)
	} else if ok {
		t.Fatalf("expected no candidate before thumb done")
	}

	if err := d.SegmentSetThumbStatus(ctx, frontSeg, "done"); err != nil {
		t.Fatalf("SegmentSetThumbStatus: %v", err)
	}

	segID, ok, err := d.TripCoverCandidate(ctx, trip1)
	if err != nil {
		t.Fatalf("TripCoverCandidate: %v", err)
	}
	if !ok || segID != frontSeg {
		t.Fatalf("TripCoverCandidate = (%d, %v), want (%d, true)", segID, ok, frontSeg)
	}

	url := "/api/segments/1/thumb.jpg"
	if err := d.TripSetCoverThumb(ctx, trip1, url); err != nil {
		t.Fatalf("TripSetCoverThumb: %v", err)
	}
	trip, err := d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	if trip.CoverThumb == nil || *trip.CoverThumb != url {
		t.Fatalf("CoverThumb = %v, want %q", trip.CoverThumb, url)
	}

	// Setting again with a different url must not clobber the first.
	if err := d.TripSetCoverThumb(ctx, trip1, "/api/segments/2/thumb.jpg"); err != nil {
		t.Fatalf("TripSetCoverThumb (second): %v", err)
	}
	trip, err = d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	if *trip.CoverThumb != url {
		t.Fatalf("CoverThumb changed on second set: got %q, want %q", *trip.CoverThumb, url)
	}
}
