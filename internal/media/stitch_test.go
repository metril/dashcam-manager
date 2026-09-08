package media

import (
	"strings"
	"testing"
)

func TestBuildPlanFastPathWholeSegments(t *testing.T) {
	items := []PlanItem{{Whole: true}, {Whole: true}}
	plan := BuildPlan(items, "front", false)
	if !plan.FastPath {
		t.Errorf("BuildPlan(whole segments, single camera, no burn-in) FastPath = false, want true")
	}
}

func TestBuildPlanFastPathKeyframeAligned(t *testing.T) {
	items := []PlanItem{{Whole: false, KeyframeAligned: true}}
	plan := BuildPlan(items, "rear", false)
	if !plan.FastPath {
		t.Errorf("BuildPlan(keyframe-aligned partial, single camera) FastPath = false, want true")
	}
}

func TestBuildPlanReencodeWhenNotKeyframeAligned(t *testing.T) {
	items := []PlanItem{{Whole: false, KeyframeAligned: false}}
	plan := BuildPlan(items, "front", false)
	if plan.FastPath {
		t.Errorf("BuildPlan(non-keyframe-aligned partial) FastPath = true, want false")
	}
}

func TestBuildPlanReencodeWhenBurnIn(t *testing.T) {
	items := []PlanItem{{Whole: true}}
	plan := BuildPlan(items, "front", true)
	if plan.FastPath {
		t.Errorf("BuildPlan(burn-in on) FastPath = true, want false")
	}
}

func TestBuildPlanReencodeWhenBothCameras(t *testing.T) {
	items := []PlanItem{{Whole: true}}
	plan := BuildPlan(items, "both", false)
	if plan.FastPath {
		t.Errorf("BuildPlan(camera=both) FastPath = true, want false")
	}
}

func TestBuildPlanMixedItemsOneUnaligned(t *testing.T) {
	items := []PlanItem{{Whole: true}, {Whole: false, KeyframeAligned: false}}
	plan := BuildPlan(items, "front", false)
	if plan.FastPath {
		t.Errorf("BuildPlan(one whole, one unaligned partial) FastPath = true, want false")
	}
}

func TestResolveRearSegmentFindsCoveringSegment(t *testing.T) {
	rear := []RearSegmentPlacement{
		{SegmentID: 1, OffsetS: 0, DurationS: 30},
		{SegmentID: 2, OffsetS: 30, DurationS: 30},
		{SegmentID: 3, OffsetS: 60, DurationS: 30},
	}
	// front instant at trip-offset 35s, rear clock is 2s ahead of front's
	// (pairOffsetS=2) -> rear-timeline target is 37s, inside segment 2
	// [30,60), at local offset 7s.
	segID, localOffset, ok := ResolveRearSegment(rear, 35, 2)
	if !ok {
		t.Fatalf("ResolveRearSegment: ok = false, want true")
	}
	if segID != 2 {
		t.Errorf("ResolveRearSegment segment = %d, want 2", segID)
	}
	if diff := localOffset - 7; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ResolveRearSegment localOffset = %v, want 7", localOffset)
	}
}

func TestResolveRearSegmentNoCoverage(t *testing.T) {
	rear := []RearSegmentPlacement{{SegmentID: 1, OffsetS: 0, DurationS: 30}}
	_, _, ok := ResolveRearSegment(rear, 100, 0)
	if ok {
		t.Errorf("ResolveRearSegment: ok = true for a gap, want false")
	}
}

func TestResolveRearSegmentNegativeOffset(t *testing.T) {
	rear := []RearSegmentPlacement{{SegmentID: 5, OffsetS: 0, DurationS: 30}}
	// front instant at 3s, rear clock is 1s BEHIND front's (pairOffsetS=-1)
	// -> rear-timeline target is 2s, still inside segment 5.
	segID, localOffset, ok := ResolveRearSegment(rear, 3, -1)
	if !ok || segID != 5 {
		t.Fatalf("ResolveRearSegment negative offset: got seg=%d ok=%v, want seg=5 ok=true", segID, ok)
	}
	if diff := localOffset - 2; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ResolveRearSegment localOffset = %v, want 2", localOffset)
	}
}

func TestWriteConcatListEscapesQuotes(t *testing.T) {
	entries := []ConcatEntry{{Path: "/mnt/dashcam/front/it's a segment.mp4"}}
	got := BuildConcatList(entries)
	want := "file '/mnt/dashcam/front/it'\\''s a segment.mp4'\n"
	if got != want {
		t.Errorf("BuildConcatList escaping = %q, want %q", got, want)
	}
}

func TestWriteConcatListWholeSegmentsNoInOutPoints(t *testing.T) {
	entries := []ConcatEntry{{Path: "a.mp4"}, {Path: "b.mp4"}}
	got := BuildConcatList(entries)
	want := "file 'a.mp4'\nfile 'b.mp4'\n"
	if got != want {
		t.Errorf("BuildConcatList(whole segments) = %q, want %q", got, want)
	}
	if strings.Contains(got, "inpoint") || strings.Contains(got, "outpoint") {
		t.Errorf("BuildConcatList(whole segments) should have no in/outpoint directives: %q", got)
	}
}

func TestWriteConcatListPartialSegmentHasInOutPoints(t *testing.T) {
	entries := []ConcatEntry{{Path: "a.mp4", InS: 5, OutS: 12.5}}
	got := BuildConcatList(entries)
	want := "file 'a.mp4'\ninpoint 5\noutpoint 12.5\n"
	if got != want {
		t.Errorf("BuildConcatList(partial) = %q, want %q", got, want)
	}
}

func TestNearestKeyframeWithinTolerance(t *testing.T) {
	times := []float64{0, 2.02, 4}
	if !NearestKeyframe(times, 2.0, 0.05) {
		t.Errorf("NearestKeyframe(2.0 vs 2.02, tol 0.05) = false, want true")
	}
	if NearestKeyframe(times, 3.0, 0.05) {
		t.Errorf("NearestKeyframe(3.0, no nearby keyframe) = true, want false")
	}
}

func TestBuildFilterComplexSingleCamera(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{
			{Path: "a.mp4", OutS: 10},
			{Path: "b.mp4", InS: 2, OutS: 8},
		},
		OutW: 1920, OutH: 1080,
	}
	script, outLabel, overlayDirs := BuildFilterComplex(spec)
	if outLabel != "vout" {
		t.Errorf("outLabel = %q, want vout", outLabel)
	}
	if len(overlayDirs) != 0 {
		t.Errorf("overlayDirs = %v, want none", overlayDirs)
	}
	for _, want := range []string{"[0:v]", "[1:v]", "scale=1920:1080", "setpts=PTS-STARTPTS", "concat=n=2:v=1:a=0[vout]"} {
		if !strings.Contains(script, want) {
			t.Errorf("BuildFilterComplex script missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "hstack") {
		t.Errorf("single-camera script should not hstack: %s", script)
	}
}

func TestBuildFilterComplexBothCameras(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{{Path: "front.mp4", OutS: 10}},
		Rear:  []ReencodeInput{{Path: "rear.mp4", OutS: 10}},
		OutW:  960, OutH: 540,
	}
	script, outLabel, _ := BuildFilterComplex(spec)
	if outLabel != "vout" {
		t.Errorf("outLabel = %q, want vout", outLabel)
	}
	for _, want := range []string{"[0:v]", "[1:v]", "hstack=inputs=2[h0]", "[h0]concat=n=1:v=1:a=0[vout]"} {
		if !strings.Contains(script, want) {
			t.Errorf("BuildFilterComplex(both) script missing %q: %s", want, script)
		}
	}
}

// TestBuildFilterComplexBothCamerasMultiItemInterleaving is a regression
// test for the M11 review's critical finding: with more than one item,
// BuildFilterComplex's graph indices (front0=0, rear0=1, front1=2,
// rear1=3, ...) must match the interleaved order ReencodeArgs/
// videoInputOrder actually emits -i flags in — NOT "all fronts then all
// rears", which would silently hstack the wrong pairs together.
func TestBuildFilterComplexBothCamerasMultiItemInterleaving(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{{Path: "front0.mp4", OutS: 10}, {Path: "front1.mp4", OutS: 10}},
		Rear:  []ReencodeInput{{Path: "rear0.mp4", OutS: 10}, {Path: "rear1.mp4", OutS: 10}},
		OutW:  960, OutH: 540,
	}
	script, _, _ := BuildFilterComplex(spec)
	// front0 must be graph input 0, rear0 input 1 (paired into h0);
	// front1 input 2, rear1 input 3 (paired into h1).
	for _, want := range []string{
		"[0:v]", "[1:v]", "[2:v]", "[3:v]",
		"[v0][v1]hstack=inputs=2[h0]",
		"[v2][v3]hstack=inputs=2[h1]",
		"[h0][h1]concat=n=2:v=1:a=0[vout]",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("BuildFilterComplex(both, 2 items) missing %q: %s", want, script)
		}
	}

	order := videoInputOrder(spec)
	wantOrder := []string{"front0.mp4", "rear0.mp4", "front1.mp4", "rear1.mp4"}
	if len(order) != len(wantOrder) {
		t.Fatalf("videoInputOrder length = %d, want %d", len(order), len(wantOrder))
	}
	for i, w := range wantOrder {
		if order[i].Path != w {
			t.Errorf("videoInputOrder[%d].Path = %q, want %q (must match BuildFilterComplex's indexing)", i, order[i].Path, w)
		}
	}
}

func TestBuildFilterComplexIncludesOverlay(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{{Path: "a.mp4", OutS: 10, Overlay: "drawbox=x=1:y=1:w=2:h=2"}},
		OutW:  640, OutH: 480,
	}
	script, _, _ := BuildFilterComplex(spec)
	if !strings.Contains(script, "drawbox=x=1:y=1:w=2:h=2") {
		t.Errorf("BuildFilterComplex should splice in the overlay fragment: %s", script)
	}
}

func TestBuildFilterComplexIncludesPNGOverlay(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{
			{Path: "a.mp4", OutS: 10},
			{Path: "b.mp4", OutS: 10, OverlayPNGDir: "/tmp/ov1"},
		},
		OutW: 640, OutH: 480,
	}
	script, outLabel, overlayDirs := BuildFilterComplex(spec)
	if len(overlayDirs) != 1 || overlayDirs[0] != "/tmp/ov1" {
		t.Fatalf("overlayDirs = %v, want [/tmp/ov1]", overlayDirs)
	}
	// The PNG overlay input for item 1 (graph video index 1) must be
	// indexed right after every video input (2 items here -> index 2).
	for _, want := range []string{
		"[v1][2:v]overlay=eof_action=pass[v1p]",
		"[v0][v1p]concat=n=2:v=1:a=0[" + outLabel + "]",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("BuildFilterComplex(png overlay) missing %q: %s", want, script)
		}
	}
}

func TestReencodeInputArgsWholeSegment(t *testing.T) {
	args := ReencodeInputArgs(ReencodeInput{Path: "a.mp4"})
	got := strings.Join(args, " ")
	if got != "-i a.mp4" {
		t.Errorf("ReencodeInputArgs(whole) = %q, want \"-i a.mp4\"", got)
	}
}

func TestReencodeInputArgsPartial(t *testing.T) {
	args := ReencodeInputArgs(ReencodeInput{Path: "a.mp4", InS: 1.5, OutS: 9})
	got := strings.Join(args, " ")
	want := "-ss 1.5 -to 9 -i a.mp4"
	if got != want {
		t.Errorf("ReencodeInputArgs(partial) = %q, want %q", got, want)
	}
}

func TestReencodeArgsQSVIncludesDeviceInit(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{{Path: "a.mp4", OutS: 10}},
		OutW:  640, OutH: 480,
		Mode:   ModeQSV,
		Device: "/dev/dri/renderD128",
	}
	args := ReencodeArgs(spec, nil, "script.txt", "vout", "out.mp4")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"-filter_hw_device hw",
		"-i a.mp4",
		"-c:v h264_qsv",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ReencodeArgs(qsv) missing %q: %s", want, joined)
		}
	}
	// Device init must come before the first -i (a global option, not
	// associated with a specific input stream here).
	if strings.Index(joined, "-init_hw_device") > strings.Index(joined, "-i a.mp4") {
		t.Errorf("-init_hw_device must precede the first -i: %s", joined)
	}
}

func TestReencodeArgsSoftwareOmitsDeviceInit(t *testing.T) {
	spec := ReencodeSpec{Items: []ReencodeInput{{Path: "a.mp4", OutS: 10}}, Mode: ModeSoftware, Device: "/dev/dri/renderD128"}
	args := ReencodeArgs(spec, nil, "script.txt", "vout", "out.mp4")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-init_hw_device") {
		t.Errorf("software mode should not init a hw device: %s", joined)
	}
}

func TestReencodeArgsAppendsOverlayInputsAfterVideoInputs(t *testing.T) {
	spec := ReencodeSpec{Items: []ReencodeInput{{Path: "a.mp4", OutS: 10}, {Path: "b.mp4", OutS: 10}}}
	args := ReencodeArgs(spec, []string{"/tmp/ov"}, "script.txt", "vout", "out.mp4")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-framerate 10 -i /tmp/ov/overlay_%05d.png") {
		t.Errorf("ReencodeArgs missing overlay png input: %s", joined)
	}
	if strings.Index(joined, "-i a.mp4") > strings.Index(joined, "/tmp/ov") {
		t.Errorf("overlay inputs must come after every video input: %s", joined)
	}
}

func TestResolveOutputDims1080p(t *testing.T) {
	w, h := ResolveOutputDims("1080p", 3840, 2160)
	if w != 1920 || h != 1080 {
		t.Errorf("ResolveOutputDims(1080p, 3840x2160) = %dx%d, want 1920x1080", w, h)
	}
}

func TestResolveOutputDimsSource(t *testing.T) {
	w, h := ResolveOutputDims("source", 3840, 2160)
	if w != 3840 || h != 2160 {
		t.Errorf("ResolveOutputDims(source) = %dx%d, want 3840x2160", w, h)
	}
}

func TestConcatCopyArgsHasCopyAndFaststart(t *testing.T) {
	args := ConcatCopyArgs("list.txt", "out.mp4")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-f concat", "-safe 0", "-i list.txt", "-c copy", "+faststart", "out.mp4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ConcatCopyArgs missing %q: %s", want, joined)
		}
	}
}
