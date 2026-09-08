package scan

import (
	"sort"
	"time"
)

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
