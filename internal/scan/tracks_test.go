package scan

import "testing"

// box returns a NormBox at (x,y)-(x+w,y+h), the shorthand used throughout
// this file to build synthetic per-frame boxes.
func box(x, y, w, h float64) NormBox {
	return NormBox{X1: x, Y1: y, X2: x + w, Y2: y + h}
}

// frame builds a DetectionFrame at pts carrying a single "car" object at
// box b with confidence conf.
func frame(pts float64, b NormBox, conf float64) DetectionFrame {
	return DetectionFrame{
		PTSOffset: pts,
		Objects:   []Object{{Class: "car", Confidence: conf, Norm: b}},
	}
}

func TestBuildTracksDriftingBoxIsOneTrack(t *testing.T) {
	// A car drifting slightly frame to frame (small overlap-preserving
	// motion) across 5 frames should be tracked as a single continuous
	// track, not fragmented.
	d := &Detections{DetectionFrames: []DetectionFrame{
		frame(0.0, box(0.10, 0.10, 0.20, 0.20), 0.9),
		frame(0.1, box(0.12, 0.10, 0.20, 0.20), 0.9),
		frame(0.2, box(0.14, 0.10, 0.20, 0.20), 0.85),
		frame(0.3, box(0.16, 0.10, 0.20, 0.20), 0.9),
		frame(0.4, box(0.18, 0.10, 0.20, 0.20), 0.95),
	}}

	tracks, _ := BuildTracks(d)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1: %+v", len(tracks), tracks)
	}
	tr := tracks[0]
	if tr.Kind != "object" || tr.Class != "car" {
		t.Errorf("kind/class = %q/%q, want object/car", tr.Kind, tr.Class)
	}
	if tr.NFrames != 5 {
		t.Errorf("NFrames = %d, want 5", tr.NFrames)
	}
	if tr.FirstPts != 0.0 || tr.LastPts != 0.4 {
		t.Errorf("FirstPts/LastPts = %v/%v, want 0/0.4", tr.FirstPts, tr.LastPts)
	}
	if tr.BestConf != 0.95 {
		t.Errorf("BestConf = %v, want 0.95", tr.BestConf)
	}
}

func TestBuildTracksTwoDisjointBoxesAreTwoTracks(t *testing.T) {
	// Two cars, opposite corners of the frame, never overlapping across
	// 4 frames each: two independent tracks, not one merged or split
	// further.
	d := &Detections{DetectionFrames: []DetectionFrame{
		{PTSOffset: 0.0, Objects: []Object{
			{Class: "car", Confidence: 0.9, Norm: box(0.0, 0.0, 0.1, 0.1)},
			{Class: "car", Confidence: 0.9, Norm: box(0.8, 0.8, 0.1, 0.1)},
		}},
		{PTSOffset: 0.1, Objects: []Object{
			{Class: "car", Confidence: 0.9, Norm: box(0.01, 0.0, 0.1, 0.1)},
			{Class: "car", Confidence: 0.9, Norm: box(0.81, 0.8, 0.1, 0.1)},
		}},
		{PTSOffset: 0.2, Objects: []Object{
			{Class: "car", Confidence: 0.9, Norm: box(0.02, 0.0, 0.1, 0.1)},
			{Class: "car", Confidence: 0.9, Norm: box(0.82, 0.8, 0.1, 0.1)},
		}},
		{PTSOffset: 0.3, Objects: []Object{
			{Class: "car", Confidence: 0.9, Norm: box(0.03, 0.0, 0.1, 0.1)},
			{Class: "car", Confidence: 0.9, Norm: box(0.83, 0.8, 0.1, 0.1)},
		}},
	}}

	tracks, _ := BuildTracks(d)
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2: %+v", len(tracks), tracks)
	}
	for _, tr := range tracks {
		if tr.NFrames != 4 {
			t.Errorf("track %+v: NFrames = %d, want 4", tr, tr.NFrames)
		}
	}
}

func TestBuildTracksDropsSingleFrameNoise(t *testing.T) {
	d := &Detections{DetectionFrames: []DetectionFrame{
		frame(0.0, box(0.1, 0.1, 0.2, 0.2), 0.5),
	}}
	tracks, _ := BuildTracks(d)
	if len(tracks) != 0 {
		t.Fatalf("got %d tracks, want 0 (single-frame noise dropped): %+v", len(tracks), tracks)
	}
}

func TestBuildTracksClosesAfterGapFramesMisses(t *testing.T) {
	// A car present, then gone for more than gapFrames consecutive
	// frames, then a new car appears in the same spot: two separate
	// tracks, not one continuous one spanning the gap.
	frames := []DetectionFrame{
		frame(0.0, box(0.1, 0.1, 0.2, 0.2), 0.9),
		frame(0.1, box(0.1, 0.1, 0.2, 0.2), 0.9),
	}
	// gapFrames consecutive frames elsewhere (a different, non-overlapping
	// class-mate) so the car's track accrues misses past gapFrames.
	for i := 0; i < gapFrames+1; i++ {
		frames = append(frames, DetectionFrame{
			PTSOffset: 0.2 + float64(i)*0.1,
			Objects:   []Object{{Class: "car", Confidence: 0.9, Norm: box(0.9, 0.9, 0.05, 0.05)}},
		})
	}
	frames = append(frames,
		frame(1.0, box(0.1, 0.1, 0.2, 0.2), 0.9),
		frame(1.1, box(0.1, 0.1, 0.2, 0.2), 0.9),
	)
	d := &Detections{DetectionFrames: frames}

	tracks, _ := BuildTracks(d)
	if len(tracks) != 3 {
		t.Fatalf("got %d tracks, want 3 (original + filler + reappearance): %+v", len(tracks), tracks)
	}
}

func TestBuildTracksPlateBestText(t *testing.T) {
	d := &Detections{DetectionFrames: []DetectionFrame{
		{PTSOffset: 0.0, Plates: []Plate{{Text: "ABC123", Confidence: 0.5, Norm: box(0.1, 0.1, 0.1, 0.05)}}},
		{PTSOffset: 0.1, Plates: []Plate{{Text: "ABC123", Confidence: 0.95, Norm: box(0.11, 0.1, 0.1, 0.05)}}},
		{PTSOffset: 0.2, Plates: []Plate{{Text: "ABC128", Confidence: 0.6, Norm: box(0.12, 0.1, 0.1, 0.05)}}},
	}}

	tracks, _ := BuildTracks(d)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1: %+v", len(tracks), tracks)
	}
	tr := tracks[0]
	if tr.Kind != "plate" || tr.Class != "" {
		t.Errorf("kind/class = %q/%q, want plate/\"\"", tr.Kind, tr.Class)
	}
	if tr.BestText != "ABC123" {
		t.Errorf("BestText = %q, want ABC123 (highest-confidence read)", tr.BestText)
	}
	if tr.NFrames != 3 {
		t.Errorf("NFrames = %d, want 3", tr.NFrames)
	}
}

func TestBuildTracksOutOfOrderFramesSortedByPTS(t *testing.T) {
	// Frames arriving out of pts_offset order (shouldn't happen from a
	// real sidecar, but BuildTracks sorts defensively) still produce one
	// continuous track.
	d := &Detections{DetectionFrames: []DetectionFrame{
		frame(0.2, box(0.14, 0.1, 0.2, 0.2), 0.9),
		frame(0.0, box(0.10, 0.1, 0.2, 0.2), 0.9),
		frame(0.1, box(0.12, 0.1, 0.2, 0.2), 0.9),
	}}
	tracks, _ := BuildTracks(d)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1: %+v", len(tracks), tracks)
	}
	if tracks[0].FirstPts != 0.0 || tracks[0].LastPts != 0.2 {
		t.Errorf("FirstPts/LastPts = %v/%v, want 0/0.2", tracks[0].FirstPts, tracks[0].LastPts)
	}
}

func TestBuildTracksAssignmentsMapBoxesBackToOriginalFrameOrder(t *testing.T) {
	// Frames deliberately out of pts_offset order: FrameIdx in the
	// returned Assignments must refer to the box's position in d's
	// original (unsorted) DetectionFrames, not the sorted order BuildTracks
	// processes internally.
	d := &Detections{DetectionFrames: []DetectionFrame{
		frame(0.2, box(0.14, 0.1, 0.2, 0.2), 0.9), // original index 0, pts 0.2 (processed last)
		frame(0.0, box(0.10, 0.1, 0.2, 0.2), 0.9), // original index 1, pts 0.0 (processed first)
		frame(0.1, box(0.12, 0.1, 0.2, 0.2), 0.9), // original index 2, pts 0.1 (processed second)
	}}
	tracks, assignments := BuildTracks(d)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	if len(assignments) != 3 {
		t.Fatalf("got %d assignments, want 3: %+v", len(assignments), assignments)
	}
	seenFrameIdx := map[int]bool{}
	for _, a := range assignments {
		if a.Kind != "object" {
			t.Errorf("assignment kind = %q, want object", a.Kind)
		}
		if a.TrackIdx != 0 {
			t.Errorf("assignment TrackIdx = %d, want 0", a.TrackIdx)
		}
		if a.ObjIdx != 0 {
			t.Errorf("assignment ObjIdx = %d, want 0 (one object per frame)", a.ObjIdx)
		}
		seenFrameIdx[a.FrameIdx] = true
	}
	for _, want := range []int{0, 1, 2} {
		if !seenFrameIdx[want] {
			t.Errorf("no assignment for original FrameIdx %d; got %+v", want, assignments)
		}
	}
}

func TestBuildTracksAssignmentsOmitDroppedNoiseTracks(t *testing.T) {
	// A single-frame track is dropped as noise (see
	// TestBuildTracksDropsSingleFrameNoise); its box must not appear in
	// Assignments either, since there's no surviving TrackIdx for it to
	// point at.
	d := &Detections{DetectionFrames: []DetectionFrame{
		frame(0.0, box(0.1, 0.1, 0.2, 0.2), 0.5),
	}}
	tracks, assignments := BuildTracks(d)
	if len(tracks) != 0 || len(assignments) != 0 {
		t.Fatalf("got %d tracks / %d assignments, want 0/0", len(tracks), len(assignments))
	}
}
