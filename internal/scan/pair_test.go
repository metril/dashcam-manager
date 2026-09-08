package scan

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

// TestPairRecordingsMedianOffsetRobustToOutlier reproduces the m6-report.md
// scenario: one front segment's start comes from an mtime fallback and
// happens to coincide with the rear recording's Start, which would zero (or
// badly skew) the naive rear.Start-front.Start offset. The other segments
// have a consistent ~300ms offset. PairOffsetS should reflect the median of
// per-segment overlapping deltas (~0.3s), not the outlier.
func TestPairRecordingsMedianOffsetRobustToOutlier(t *testing.T) {
	base := mustParse(t, "2026-03-16T05:51:07Z")
	seg := func(startOffset time.Duration, dur time.Duration, id int64) segRow {
		start := base.Add(startOffset)
		return segRow{id: id, start: start, end: start.Add(dur)}
	}

	// Front: seg0 is a normal 30s segment; seg1..seg3 are consistently
	// offset ~0.3s ahead of their rear counterparts.
	front := []segRow{
		seg(0, 30*time.Second, 1),
		seg(30*time.Second, 30*time.Second, 2),
		seg(60*time.Second, 30*time.Second, 3),
		seg(90*time.Second, 30*time.Second, 4),
	}
	// Rear: seg0's sidecar timestamp is missing; the mtime-heuristic
	// fallback happens to coincide almost exactly with front's Start,
	// dragging a naive min(rear.start) down to match front's Start (the
	// bug from m6-report.md). The remaining rear segments are ~300ms ahead
	// of their front counterparts, the real steady-state offset.
	rear := []segRow{
		seg(10*time.Millisecond, 30*time.Second, 101), // outlier: overlaps front seg0
		seg(30*time.Second+300*time.Millisecond, 30*time.Second, 102),
		seg(60*time.Second+300*time.Millisecond, 30*time.Second, 103),
		seg(90*time.Second+300*time.Millisecond, 30*time.Second, 104),
	}

	recStart := func(segs []segRow) (time.Time, time.Time) {
		start, end := segs[0].start, segs[0].end
		for _, s := range segs[1:] {
			if s.start.Before(start) {
				start = s.start
			}
			if s.end.After(end) {
				end = s.end
			}
		}
		return start, end
	}

	fStart, fEnd := recStart(front)
	rStart, rEnd := recStart(rear)

	fronts := []Recording{{
		Camera: "front", Key: "recording_20260316_055107",
		Start: fStart, End: fEnd, Segments: front,
	}}
	rears := []Recording{{
		Camera: "rear", Key: "recording_20260316_055107",
		Start: rStart, End: rEnd, Segments: rear,
	}}

	pairs := PairRecordings(fronts, rears)
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(pairs))
	}
	p := pairs[0]
	if p.PairOffsetS == nil {
		t.Fatal("expected non-nil PairOffsetS")
	}

	// The naive formula (rear.Start - front.Start) would be ~0.01s here,
	// masking the real ~0.3s offset. Assert we're close to the median of
	// the non-outlier deltas instead.
	const want = 0.3
	if got := *p.PairOffsetS; got < want-0.01 || got > want+0.01 {
		t.Errorf("PairOffsetS = %v, want ~%v (median of overlapping-segment deltas, robust to the seg0 outlier)", got, want)
	}

	naive := rStart.Sub(fStart).Seconds()
	if naive > 0.05 {
		t.Fatalf("test setup invalid: naive offset %v should be near zero to demonstrate the bug", naive)
	}
}

// TestPairRecordingsFallsBackWithoutSegmentTiming checks that pairing still
// works (using the old Start-Start formula) when a Recording carries no
// per-segment Segments data, so callers that don't populate it don't break.
func TestPairRecordingsFallsBackWithoutSegmentTiming(t *testing.T) {
	fStart := mustParse(t, "2026-01-01T00:00:00Z")
	fEnd := fStart.Add(30 * time.Second)
	rStart := fStart.Add(2 * time.Second)
	rEnd := rStart.Add(30 * time.Second)

	fronts := []Recording{{Camera: "front", Key: "recording_x", Start: fStart, End: fEnd, SegmentIDs: []int64{1}}}
	rears := []Recording{{Camera: "rear", Key: "recording_x", Start: rStart, End: rEnd, SegmentIDs: []int64{2}}}

	pairs := PairRecordings(fronts, rears)
	if len(pairs) != 1 || pairs[0].PairOffsetS == nil {
		t.Fatalf("expected 1 paired trip with a PairOffsetS, got %+v", pairs)
	}
	want := rStart.Sub(fStart).Seconds()
	if got := *pairs[0].PairOffsetS; got != want {
		t.Errorf("PairOffsetS = %v, want fallback value %v", got, want)
	}
}

// TestMedianPairOffsetMatchesByMaxOverlap directly exercises
// medianPairOffset with segments of differing lengths, to check pairing by
// max overlap (not by index/seq) and the even-count median.
func TestMedianPairOffsetMatchesByMaxOverlap(t *testing.T) {
	base := mustParse(t, "2026-01-01T00:00:00Z")
	seg := func(startOffset, dur time.Duration) segRow {
		start := base.Add(startOffset)
		return segRow{start: start, end: start.Add(dur)}
	}

	// One long front segment spans two shorter rear segments.
	front := []segRow{seg(0, 60*time.Second)}
	rear := []segRow{
		seg(1*time.Second, 30*time.Second),  // overlaps front[0] by 29s -> delta 1s
		seg(31*time.Second, 30*time.Second), // overlaps front[0] by 29s -> delta 31s
	}

	offset, ok := medianPairOffset(front, rear)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// median of {1, 31} = 16
	if want := 16.0; offset != want {
		t.Errorf("offset = %v, want %v", offset, want)
	}
}

// TestMedianPairOffsetNoOverlapReturnsFalse checks the ok=false path used to
// trigger the caller's fallback.
func TestMedianPairOffsetNoOverlapReturnsFalse(t *testing.T) {
	base := mustParse(t, "2026-01-01T00:00:00Z")
	front := []segRow{{start: base, end: base.Add(10 * time.Second)}}
	rear := []segRow{{start: base.Add(time.Hour), end: base.Add(time.Hour + 10*time.Second)}}

	if _, ok := medianPairOffset(front, rear); ok {
		t.Error("expected ok=false for non-overlapping segments")
	}
	if _, ok := medianPairOffset(nil, rear); ok {
		t.Error("expected ok=false for empty front segments")
	}
}
