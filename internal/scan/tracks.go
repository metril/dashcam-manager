package scan

import (
	"sort"
)

// Tracker constants, kept together for tuning. Measured against real
// sidecars from the production SSD (2026-09-08, ~9k detections_raw rows):
// analyzed-frame spacing is bimodal — a dense run of frames while an
// object stays in view (median gap ~0.03-0.15s, i.e. roughly the
// recorder's own frame rate) separated by long silent stretches (seconds,
// occasionally tens of seconds) when nothing at all was detected and the
// sidecar simply omits those frames. gapFrames therefore counts
// consecutive *analyzed* frames a class was absent from, not wall-clock
// time — a handful of frames' occlusion/miss tolerance stays meaningful
// even though the underlying frame cadence is irregular. iouMin=0.3 and
// gapFrames=3 are the standard IoU-tracker defaults and held up fine on
// spot checks of dense car/person runs in real segments; revisit if
// spot-checks on more segments show excessive track fragmentation or
// swapping.
const (
	// iouMin is the minimum IoU for a box to extend an existing track.
	iouMin = 0.3
	// gapFrames is how many consecutive analyzed frames a track may go
	// unmatched before it's closed.
	gapFrames = 3
	// minTrackFrames drops tracks shorter than this as noise (a single
	// stray detection, not a followed object).
	minTrackFrames = 2
)

// Track is one tracked object/plate/vehicle across a segment's analyzed
// frames, built by BuildTracks. Kind is "object", "plate", or "vehicle";
// Class is the object/vehicle class ("car", "person", ...) or "" for
// plates (plates aren't classified, so all plate detections in a segment
// share one class-less group). Last* fields describe the box at the last
// matched frame.
type Track struct {
	Kind     string
	Class    string
	FirstPts float64
	LastPts  float64
	NFrames  int
	BestConf float64
	// BestText is the highest-confidence plate text seen in the track;
	// always "" for kind != "plate".
	BestText string
	Last     NormBox
}

// Assignment ties one raw detection box (identified by its position in
// the original, unsorted d.DetectionFrames passed to BuildTracks) to the
// track it ended up part of. Kind is "object", "plate", or "vehicle";
// ObjIdx indexes that frame's Objects/Plates/Vehicles slice respectively.
// TrackIdx indexes the []Track BuildTracks returns alongside these.
// Boxes belonging to a track dropped as noise (fewer than minTrackFrames)
// have no Assignment entry.
type Assignment struct {
	Kind     string
	FrameIdx int
	ObjIdx   int
	TrackIdx int
}

// trackState is a Track under construction, plus tracker bookkeeping not
// exposed on the finished Track.
type trackState struct {
	t    Track
	miss int
	// members lists every candidate matched into this track so far, as
	// (frameIdx, objIdx) pairs — resolved to Assignment entries once we
	// know whether the track survives (>= minTrackFrames) and, if so,
	// its final index in the returned Track slice.
	members [][2]int
}

// candidate is one frame's box for a given (kind, class) group, carried
// through matching.
type candidate struct {
	box    NormBox
	conf   float64
	text   string
	objIdx int
}

func iou(a, b NormBox) float64 {
	ix1, iy1 := max(a.X1, b.X1), max(a.Y1, b.Y1)
	ix2, iy2 := min(a.X2, b.X2), min(a.Y2, b.Y2)
	iw, ih := ix2-ix1, iy2-iy1
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	areaA := (a.X2 - a.X1) * (a.Y2 - a.Y1)
	areaB := (b.X2 - b.X1) * (b.Y2 - b.Y1)
	union := areaA + areaB - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// classKey groups tracker state by (kind, class).
type classKey struct {
	kind, class string
}

// BuildTracks runs a greedy IoU tracker (see the package's tracks.go
// constants) over d's analyzed frames, grouping boxes by (kind, class) —
// "object" keyed by Object.Class, "vehicle" keyed by Vehicle.Type, "plate"
// with an empty class since plates aren't classified — and returns every
// track with at least minTrackFrames frames, plus an Assignment per
// surviving box tying it back to its track by index.
//
// Algorithm, per (kind, class) group, over frames ordered by PTSOffset:
//  1. Compute IoU between every candidate box and the last box of every
//     open track in the group.
//  2. Greedily assign the highest-IoU pair, then repeat among the
//     remaining unassigned pairs, stopping once the best remaining IoU is
//     below iouMin. A matched candidate extends its track (last box/pts,
//     best confidence, and for plates the highest-confidence text).
//  3. Unmatched candidates open new tracks.
//  4. Every open track not matched this frame (including groups absent
//     from the frame entirely) increments a miss counter; a track closes
//     once its miss counter exceeds gapFrames.
//  5. At the end of the segment, all remaining open tracks are closed.
//     Tracks with fewer than minTrackFrames frames are dropped as noise,
//     along with their Assignment entries.
func BuildTracks(d *Detections) ([]Track, []Assignment) {
	order := make([]int, len(d.DetectionFrames))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return d.DetectionFrames[order[i]].PTSOffset < d.DetectionFrames[order[j]].PTSOffset
	})

	open := map[classKey][]*trackState{}
	var done []Track
	var assignments []Assignment

	closeTrack := func(kind string, ts *trackState) {
		if ts.t.NFrames < minTrackFrames {
			return
		}
		trackIdx := len(done)
		done = append(done, ts.t)
		for _, m := range ts.members {
			assignments = append(assignments, Assignment{
				Kind: kind, FrameIdx: m[0], ObjIdx: m[1], TrackIdx: trackIdx,
			})
		}
	}

	for _, fi := range order {
		frame := d.DetectionFrames[fi]
		// Group this frame's candidates by (kind, class).
		byKey := map[classKey][]candidate{}
		for oi, o := range frame.Objects {
			k := classKey{"object", o.Class}
			byKey[k] = append(byKey[k], candidate{box: o.Norm, conf: o.Confidence, objIdx: oi})
		}
		for oi, pl := range frame.Plates {
			k := classKey{"plate", ""}
			byKey[k] = append(byKey[k], candidate{box: pl.Norm, conf: pl.Confidence, text: pl.Text, objIdx: oi})
		}
		for oi, v := range frame.Vehicles {
			k := classKey{"vehicle", v.Type}
			byKey[k] = append(byKey[k], candidate{box: v.Norm, conf: v.TypeConfidence, objIdx: oi})
		}

		// Every key with either open tracks or new candidates gets
		// processed this tick, so a group with open tracks but no
		// candidates this frame still gets its miss counters bumped.
		// Collected into a sorted slice (rather than ranged over
		// directly as a map) so processing order — and therefore which
		// track ends up matched first when scores tie in the pair sort
		// below — is deterministic run to run.
		keySet := map[classKey]bool{}
		for k := range open {
			keySet[k] = true
		}
		for k := range byKey {
			keySet[k] = true
		}
		keys := make([]classKey, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].kind != keys[j].kind {
				return keys[i].kind < keys[j].kind
			}
			return keys[i].class < keys[j].class
		})

		for _, k := range keys {
			tracks := open[k]
			cands := byKey[k]
			matchedTrack := make([]bool, len(tracks))
			matchedCand := make([]bool, len(cands))

			type pair struct {
				ti, ci int
				iou    float64
			}
			var pairs []pair
			for ti, ts := range tracks {
				for ci, c := range cands {
					if v := iou(ts.t.Last, c.box); v >= iouMin {
						pairs = append(pairs, pair{ti, ci, v})
					}
				}
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].iou > pairs[j].iou })
			for _, p := range pairs {
				if matchedTrack[p.ti] || matchedCand[p.ci] {
					continue
				}
				matchedTrack[p.ti] = true
				matchedCand[p.ci] = true
				ts := tracks[p.ti]
				c := cands[p.ci]
				ts.t.Last = c.box
				ts.t.LastPts = frame.PTSOffset
				ts.t.NFrames++
				ts.miss = 0
				ts.members = append(ts.members, [2]int{fi, c.objIdx})
				if c.conf > ts.t.BestConf {
					ts.t.BestConf = c.conf
					if k.kind == "plate" {
						ts.t.BestText = c.text
					}
				}
			}

			var stillOpen []*trackState
			for ti, ts := range tracks {
				if matchedTrack[ti] {
					stillOpen = append(stillOpen, ts)
					continue
				}
				ts.miss++
				if ts.miss > gapFrames {
					closeTrack(k.kind, ts)
					continue
				}
				stillOpen = append(stillOpen, ts)
			}
			for ci, c := range cands {
				if matchedCand[ci] {
					continue
				}
				nt := &trackState{t: Track{
					Kind: k.kind, Class: k.class,
					FirstPts: frame.PTSOffset, LastPts: frame.PTSOffset,
					NFrames: 1, BestConf: c.conf, Last: c.box,
				}, members: [][2]int{{fi, c.objIdx}}}
				if k.kind == "plate" {
					nt.t.BestText = c.text
				}
				stillOpen = append(stillOpen, nt)
			}

			if len(stillOpen) == 0 {
				delete(open, k)
			} else {
				open[k] = stillOpen
			}
		}
	}

	remainingKeys := make([]classKey, 0, len(open))
	for k := range open {
		remainingKeys = append(remainingKeys, k)
	}
	sort.Slice(remainingKeys, func(i, j int) bool {
		if remainingKeys[i].kind != remainingKeys[j].kind {
			return remainingKeys[i].kind < remainingKeys[j].kind
		}
		return remainingKeys[i].class < remainingKeys[j].class
	})
	for _, k := range remainingKeys {
		for _, ts := range open[k] {
			closeTrack(k.kind, ts)
		}
	}

	return done, assignments
}
