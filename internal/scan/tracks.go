package scan

import (
	"sort"
	"strings"
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

	// nmsIoUVehicle and nmsIoUOther are the same-frame NMS thresholds run
	// per (frame, kind) across all classes/Types before per-class-key
	// tracking (see nmsSuppress) — a safety net against the recorder's
	// duplicate-vehicle-classification bug
	// (docs/upstream/recorder-duplicate-vehicles.md), which the sidecar
	// parser's frame-hygiene pass (sidecar.go's dropEchoedEntries) already
	// handles deterministically for the exact-echo case; this catches
	// near-duplicates that don't survive as byte-identical echoes (e.g. a
	// dup pair whose two entries carry different Types, so they'd
	// otherwise land in different classKey groups and become two tracks).
	// Vehicle boxes get a looser (lower) threshold than plates/objects
	// since the observed duplicate pairs commonly sit at IoU ~0.75-1.0
	// with slightly different bboxes.
	nmsIoUVehicle = 0.75
	nmsIoUOther   = 0.90
	// makeVoteFloor is the minimum weight a track's make vote can carry,
	// so an absorbed or member box with MakeConfidence == 0 (or no make at
	// all — filtered out before voting) still contributes rather than
	// vanishing from the tally.
	makeVoteFloor = 1e-3
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
	// Make is the canonical per-track make vote (see trackState.makeVotes):
	// the lower(trim(make)) key with the highest accumulated weight across
	// every member and NMS-absorbed box, tie-broken by vote count then
	// lexicographically. "" for kind != "vehicle" or when no member ever
	// reported a non-empty make.
	Make string
	Last NormBox
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
	// makeVotes accumulates, for kind=="vehicle" tracks only, weight per
	// lower(trim(make)) key from every member candidate (and, in
	// resolveAbsorbed, every NMS-absorbed box sharing this track) that
	// reported a non-empty make. makeSample keeps one original-cased make
	// string per key (first seen) so the winning key can be rendered back
	// as a real make name rather than its lowercased vote key.
	makeVotes  map[string]float64
	makeSample map[string]string
	// makeVoteCount tracks how many boxes voted for each key, used only to
	// break a weight tie in resolveMake.
	makeVoteCount map[string]int
}

// addMakeVote records one box's make vote into ts, weighted by
// max(makeConf, makeVoteFloor) so a MakeConfidence of exactly 0 (or a
// track with no confidence signal at all) still counts once rather than
// vanishing from the tally. No-op for an empty/whitespace-only make.
func (ts *trackState) addMakeVote(make_ string, makeConf float64) {
	key := strings.ToLower(strings.TrimSpace(make_))
	if key == "" {
		return
	}
	if ts.makeVotes == nil {
		ts.makeVotes = map[string]float64{}
		ts.makeSample = map[string]string{}
		ts.makeVoteCount = map[string]int{}
	}
	weight := makeConf
	if weight < makeVoteFloor {
		weight = makeVoteFloor
	}
	ts.makeVotes[key] += weight
	ts.makeVoteCount[key]++
	if _, ok := ts.makeSample[key]; !ok {
		ts.makeSample[key] = strings.TrimSpace(make_)
	}
}

// resolveMake picks ts's winning make: highest accumulated weight, tie-
// broken by vote count then lexicographically by key (for determinism),
// rendered back via makeSample. Returns "" if no vote was ever cast.
func (ts *trackState) resolveMake() string {
	var bestKey string
	var bestWeight float64
	var bestCount int
	first := true
	for key, w := range ts.makeVotes {
		c := ts.makeVoteCount[key]
		better := first
		if !first {
			switch {
			case w != bestWeight:
				better = w > bestWeight
			case c != bestCount:
				better = c > bestCount
			default:
				better = key < bestKey
			}
		}
		if better {
			bestKey, bestWeight, bestCount, first = key, w, c, false
		}
	}
	if first {
		return ""
	}
	return ts.makeSample[bestKey]
}

// candidate is one frame's box for a given (kind, class) group, carried
// through matching.
type candidate struct {
	box    NormBox
	conf   float64
	text   string
	objIdx int
	// make/makeConf are set only for kind=="vehicle" candidates, carried
	// through so the matching track's makeVotes can be updated.
	make     string
	makeConf float64
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

// sortedIntKeys returns m's keys in ascending order, so callers that fold
// map-derived values into shared per-track state (e.g. make votes) do so
// in a deterministic order rather than Go's randomized map iteration.
func sortedIntKeys(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
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
	// pendingAbsorbed records boxes suppressed by same-frame NMS
	// (nmsSuppress below), keyed to the winner box (same kind, same frame)
	// they were absorbed into. Resolved into extra Assignment entries once
	// every winner's final TrackIdx is known (after the main loop below),
	// so an absorbed box's det_objects/det_plates/det_vehicles row gets
	// the same track_id as its winner without itself extending NFrames or
	// participating in IoU matching.
	var pendingAbsorbed []absorbedRef

	closeTrack := func(kind string, ts *trackState) {
		if ts.t.NFrames < minTrackFrames {
			return
		}
		if kind == "vehicle" {
			ts.t.Make = ts.resolveMake()
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

		// Same-frame NMS across all classes/Types, per kind, as a safety
		// net for the recorder's duplicate-vehicle bug (see nmsSuppress
		// and the nmsIoU* constants): only the winners of each kind go on
		// to per-class-key tracking below. Absorbed boxes are recorded in
		// pendingAbsorbed (track_id, resolved after the main loop) and,
		// for vehicles, immediately fold their make vote into whichever
		// trackState the winner joins this frame (frameVehicleExtraVotes).
		objWinners, objAbsorbedBy := nmsSuppress(objectNMSItems(frame.Objects), nmsIoUOther)
		plateWinners, plateAbsorbedBy := nmsSuppress(plateNMSItems(frame.Plates), nmsIoUOther)
		vehWinners, vehAbsorbedBy := nmsSuppress(vehicleNMSItems(frame.Vehicles), nmsIoUVehicle)
		for _, absorbedIdx := range sortedIntKeys(objAbsorbedBy) {
			pendingAbsorbed = append(pendingAbsorbed, absorbedRef{kind: "object", frameIdx: fi, absorbedObjIdx: absorbedIdx, winnerObjIdx: objAbsorbedBy[absorbedIdx]})
		}
		for _, absorbedIdx := range sortedIntKeys(plateAbsorbedBy) {
			pendingAbsorbed = append(pendingAbsorbed, absorbedRef{kind: "plate", frameIdx: fi, absorbedObjIdx: absorbedIdx, winnerObjIdx: plateAbsorbedBy[absorbedIdx]})
		}
		for _, absorbedIdx := range sortedIntKeys(vehAbsorbedBy) {
			pendingAbsorbed = append(pendingAbsorbed, absorbedRef{kind: "vehicle", frameIdx: fi, absorbedObjIdx: absorbedIdx, winnerObjIdx: vehAbsorbedBy[absorbedIdx]})
		}
		// frameVehicleExtraVotes' per-winner vote lists are built by
		// iterating vehAbsorbedBy in sorted (absorbedIdx) key order, not
		// map range order: when two absorbed boxes vote for the same
		// winner with the same key but different original casing (e.g.
		// "Toyota" vs "TOYOTA"), addMakeVote's makeSample keeps whichever
		// arrives first, so an arbitrary map iteration order would make
		// the rendered make casing nondeterministic run to run.
		frameVehicleExtraVotes := map[int][]makeVote{}
		for _, absorbedIdx := range sortedIntKeys(vehAbsorbedBy) {
			winnerIdx := vehAbsorbedBy[absorbedIdx]
			av := frame.Vehicles[absorbedIdx]
			frameVehicleExtraVotes[winnerIdx] = append(frameVehicleExtraVotes[winnerIdx], makeVote{make: av.Make, conf: av.MakeConfidence})
		}
		objWinnerSet := intSet(objWinners)
		plateWinnerSet := intSet(plateWinners)
		vehWinnerSet := intSet(vehWinners)

		// Group this frame's (NMS-surviving) candidates by (kind, class).
		byKey := map[classKey][]candidate{}
		for oi, o := range frame.Objects {
			if !objWinnerSet[oi] {
				continue
			}
			k := classKey{"object", o.Class}
			byKey[k] = append(byKey[k], candidate{box: o.Norm, conf: o.Confidence, objIdx: oi})
		}
		for oi, pl := range frame.Plates {
			if !plateWinnerSet[oi] {
				continue
			}
			k := classKey{"plate", ""}
			byKey[k] = append(byKey[k], candidate{box: pl.Norm, conf: pl.Confidence, text: pl.Text, objIdx: oi})
		}
		for oi, v := range frame.Vehicles {
			if !vehWinnerSet[oi] {
				continue
			}
			k := classKey{"vehicle", v.Type}
			byKey[k] = append(byKey[k], candidate{box: v.Norm, conf: v.TypeConfidence, objIdx: oi, make: v.Make, makeConf: v.MakeConfidence})
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
				if k.kind == "vehicle" {
					ts.addMakeVote(c.make, c.makeConf)
					for _, av := range frameVehicleExtraVotes[c.objIdx] {
						ts.addMakeVote(av.make, av.conf)
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
				if k.kind == "vehicle" {
					nt.addMakeVote(c.make, c.makeConf)
					for _, av := range frameVehicleExtraVotes[c.objIdx] {
						nt.addMakeVote(av.make, av.conf)
					}
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

	// Resolve pendingAbsorbed now that every surviving track has its final
	// TrackIdx: an absorbed box gets an Assignment to the same track as
	// its same-frame winner (see nmsSuppress above), without ever having
	// been through IoU matching or counting toward the track's NFrames.
	// If the winner's own track was itself dropped as noise (no
	// Assignment resolved for it), the absorbed box also gets none — a box
	// that was never part of a real track shouldn't manufacture one.
	winnerTrackIdx := map[string]map[int]map[int]int{}
	for _, a := range assignments {
		if winnerTrackIdx[a.Kind] == nil {
			winnerTrackIdx[a.Kind] = map[int]map[int]int{}
		}
		if winnerTrackIdx[a.Kind][a.FrameIdx] == nil {
			winnerTrackIdx[a.Kind][a.FrameIdx] = map[int]int{}
		}
		winnerTrackIdx[a.Kind][a.FrameIdx][a.ObjIdx] = a.TrackIdx
	}
	for _, p := range pendingAbsorbed {
		if trackIdx, ok := winnerTrackIdx[p.kind][p.frameIdx][p.winnerObjIdx]; ok {
			assignments = append(assignments, Assignment{
				Kind: p.kind, FrameIdx: p.frameIdx, ObjIdx: p.absorbedObjIdx, TrackIdx: trackIdx,
			})
		}
	}

	return done, assignments
}

// absorbedRef records one box suppressed by same-frame NMS, pending
// resolution (in BuildTracks, after the main tracking loop) to an
// Assignment sharing its winner's final TrackIdx.
type absorbedRef struct {
	kind                         string
	frameIdx                     int
	absorbedObjIdx, winnerObjIdx int
}

// makeVote is one box's (make, make_confidence) pair, carried from an
// NMS-absorbed vehicle duplicate to whichever trackState its winner joins
// (see BuildTracks' frameVehicleExtraVotes).
type makeVote struct {
	make string
	conf float64
}

// intSet builds a membership set from a slice of ints, for the O(1)
// "is this objIdx an NMS winner" checks in BuildTracks.
func intSet(vals []int) map[int]bool {
	s := make(map[int]bool, len(vals))
	for _, v := range vals {
		s[v] = true
	}
	return s
}

// nmsItem is one frame's box, prepared for nmsSuppress: objIdx is its
// position in the frame's Objects/Plates/Vehicles slice (whichever kind
// nmsSuppress is being run for).
type nmsItem struct {
	objIdx int
	conf   float64
	box    NormBox
}

func objectNMSItems(objs []Object) []nmsItem {
	items := make([]nmsItem, len(objs))
	for oi, o := range objs {
		items[oi] = nmsItem{objIdx: oi, conf: o.Confidence, box: o.Norm}
	}
	return items
}

func plateNMSItems(plates []Plate) []nmsItem {
	items := make([]nmsItem, len(plates))
	for oi, p := range plates {
		items[oi] = nmsItem{objIdx: oi, conf: p.Confidence, box: p.Norm}
	}
	return items
}

func vehicleNMSItems(vehicles []Vehicle) []nmsItem {
	items := make([]nmsItem, len(vehicles))
	for oi, v := range vehicles {
		items[oi] = nmsItem{objIdx: oi, conf: v.TypeConfidence, box: v.Norm}
	}
	return items
}

func nmsBoxArea(b NormBox) float64 {
	return (b.X2 - b.X1) * (b.Y2 - b.Y1)
}

// nmsSuppress runs greedy same-frame NMS over items (all of one kind,
// across every class/Type — the caller groups by kind, not by class,
// since the bug this defends against can echo a duplicate box under a
// *different* Type than the original, see the nmsIoU* constants' comment)
// at iouThresh, returning the surviving winners' objIdx values and, for
// every suppressed item, the objIdx of the winner it was absorbed into.
//
// Items are considered in order of conf descending, ties broken by larger
// box area then smaller objIdx (earlier in the frame). Each item in turn
// either becomes a new winner (no already-kept winner overlaps it above
// iouThresh) or is absorbed into the first-considered (i.e. highest
// priority by the same ordering) kept winner it overlaps.
func nmsSuppress(items []nmsItem, iouThresh float64) (winners []int, absorbedBy map[int]int) {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := items[order[i]], items[order[j]]
		if a.conf != b.conf {
			return a.conf > b.conf
		}
		aa, ba := nmsBoxArea(a.box), nmsBoxArea(b.box)
		if aa != ba {
			return aa > ba
		}
		return a.objIdx < b.objIdx
	})

	absorbedBy = map[int]int{}
	var kept []int // indices into items, in the order they were kept
	for _, oi := range order {
		it := items[oi]
		winnerItemIdx := -1
		for _, ki := range kept {
			if iou(items[ki].box, it.box) > iouThresh {
				winnerItemIdx = ki
				break
			}
		}
		if winnerItemIdx == -1 {
			kept = append(kept, oi)
			winners = append(winners, it.objIdx)
		} else {
			absorbedBy[it.objIdx] = items[winnerItemIdx].objIdx
		}
	}
	return winners, absorbedBy
}
