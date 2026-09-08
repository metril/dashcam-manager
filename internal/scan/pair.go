package scan

import (
	"sort"
	"time"
)

// TripGapSplit is the maximum gap, between the end of one segment and the
// start of the next within the same recording directory, before
// rebuildTripsForDate splits that recording into separate sub-trips. A
// single "recording_..." directory can in practice span many hours (the
// recorder doesn't always rotate directories), so pairing whole recordings
// as one trip can both produce absurdly long "trips" and, worse, force a
// long front recording to pick just one overlapping rear recording out of
// several, orphaning the others as spurious rear-only trips. Splitting on a
// gap this size groups segments back into the natural drive-length chunks
// a human would call one trip.
const TripGapSplit = 5 * time.Minute

// Recording is one camera's grouped segments within a single recording
// directory (e.g. "recording_20260227_001654"), the unit that front/rear
// trip pairing operates on.
type Recording struct {
	Camera     string // "front" | "rear"
	Key        string // recording dir name
	Start      time.Time
	End        time.Time
	Bytes      int64
	SegmentIDs []int64
}

// Pair is a matched (or unmatched) front/rear recording pair — the basis of
// one trips row. FrontDir/RearDir is empty when that side has no recording
// (a rear-only or front-only trip).
type Pair struct {
	TripKey         string
	FrontDir        string
	RearDir         string
	Started         time.Time
	Ended           time.Time
	DurationS       float64
	PairOffsetS     *float64 // rear.Start - front.Start, seconds; nil if unpaired
	Bytes           int64
	FrontSegmentIDs []int64
	RearSegmentIDs  []int64
}

// SegmentCount is the total number of segments (front + rear) in the pair.
func (p Pair) SegmentCount() int {
	return len(p.FrontSegmentIDs) + len(p.RearSegmentIDs)
}

// AllSegmentIDs returns every segment id (front + rear) in the pair.
func (p Pair) AllSegmentIDs() []int64 {
	out := make([]int64, 0, p.SegmentCount())
	out = append(out, p.FrontSegmentIDs...)
	out = append(out, p.RearSegmentIDs...)
	return out
}

// segRow is one segment's own start/end/bytes, as loaded from the segments
// table for trip (re)building — kept per-segment (rather than folded
// straight into a running min/max) because splitTripGaps needs each
// segment's individual timing to find gaps within a recording directory.
type segRow struct {
	id         int64
	start, end time.Time
	bytes      int64
}

// splitTripGaps sorts segs by start time and splits them into consecutive
// runs wherever the gap between one segment's end and the next segment's
// start exceeds TripGapSplit, returning each run in start order. A
// recording directory with no such gap yields a single run containing every
// segment (splitTripGaps never drops a segment).
func splitTripGaps(segs []segRow) [][]segRow {
	if len(segs) == 0 {
		return nil
	}
	sorted := make([]segRow, len(segs))
	copy(sorted, segs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })

	var runs [][]segRow
	run := []segRow{sorted[0]}
	for i := 1; i < len(sorted); i++ {
		if sorted[i].start.Sub(run[len(run)-1].end) > TripGapSplit {
			runs = append(runs, run)
			run = nil
		}
		run = append(run, sorted[i])
	}
	runs = append(runs, run)
	return runs
}

// overlapSeconds returns the overlap, in seconds, between two [start,end)
// intervals — 0 if they don't overlap (or either interval is degenerate).
func overlapSeconds(aStart, aEnd, bStart, bEnd time.Time) float64 {
	lo := aStart
	if bStart.After(lo) {
		lo = bStart
	}
	hi := aEnd
	if bEnd.Before(hi) {
		hi = bEnd
	}
	if !hi.After(lo) {
		return 0
	}
	return hi.Sub(lo).Seconds()
}

// PairRecordings pairs front and rear recordings by maximum time overlap —
// a greedy match over all overlapping (front,rear) candidates, highest
// overlap first, each recording used at most once. trip_key is the front
// recording's dir name, or the rear recording's when there's no front
// (per the M4 brief). Every input recording appears in exactly one output
// Pair, sorted by Started.
func PairRecordings(fronts, rears []Recording) []Pair {
	type candidate struct {
		fi, ri  int
		overlap float64
	}
	var candidates []candidate
	for fi, f := range fronts {
		for ri, r := range rears {
			if ov := overlapSeconds(f.Start, f.End, r.Start, r.End); ov > 0 {
				candidates = append(candidates, candidate{fi, ri, ov})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].overlap > candidates[j].overlap })

	frontUsed := make([]bool, len(fronts))
	rearUsed := make([]bool, len(rears))
	matchedRear := make(map[int]int, len(candidates)) // front index -> rear index
	for _, c := range candidates {
		if frontUsed[c.fi] || rearUsed[c.ri] {
			continue
		}
		frontUsed[c.fi] = true
		rearUsed[c.ri] = true
		matchedRear[c.fi] = c.ri
	}

	pairs := make([]Pair, 0, len(fronts)+len(rears))
	for fi, f := range fronts {
		p := Pair{
			TripKey:         f.Key,
			FrontDir:        f.Key,
			Started:         f.Start,
			Ended:           f.End,
			Bytes:           f.Bytes,
			FrontSegmentIDs: append([]int64(nil), f.SegmentIDs...),
		}
		if ri, ok := matchedRear[fi]; ok {
			r := rears[ri]
			p.RearDir = r.Key
			if r.Start.Before(p.Started) {
				p.Started = r.Start
			}
			if r.End.After(p.Ended) {
				p.Ended = r.End
			}
			p.Bytes += r.Bytes
			p.RearSegmentIDs = append([]int64(nil), r.SegmentIDs...)
			offset := r.Start.Sub(f.Start).Seconds()
			p.PairOffsetS = &offset
		}
		p.DurationS = p.Ended.Sub(p.Started).Seconds()
		pairs = append(pairs, p)
	}
	for ri, r := range rears {
		if rearUsed[ri] {
			continue
		}
		pairs = append(pairs, Pair{
			TripKey:        r.Key,
			RearDir:        r.Key,
			Started:        r.Start,
			Ended:          r.End,
			DurationS:      r.End.Sub(r.Start).Seconds(),
			Bytes:          r.Bytes,
			RearSegmentIDs: append([]int64(nil), r.SegmentIDs...),
		})
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Started.Before(pairs[j].Started) })
	return pairs
}
