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

func TestReencodeArgsNVENCIncludesDeviceInit(t *testing.T) {
	spec := ReencodeSpec{
		Items: []ReencodeInput{{Path: "a.mp4", OutS: 10}},
		OutW:  640, OutH: 480,
		Mode:   ModeNVENC,
		Device: "0",
	}
	args := ReencodeArgs(spec, nil, "script.txt", "vout", "out.mp4")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device cuda=hw:0",
		"-filter_hw_device hw",
		"-i a.mp4",
		"-c:v h264_nvenc",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ReencodeArgs(nvenc) missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "-hwaccel ") {
		t.Errorf("ReencodeArgs(nvenc) filter graph stays software (nvenc uploads sw frames itself), should not have a -hwaccel decode flag: %s", joined)
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
	args := ConcatCopyArgs("list.txt", "out.mp4", ContainerMP4)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-f concat", "-safe 0", "-i list.txt", "-c copy", "+faststart", "out.mp4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ConcatCopyArgs missing %q: %s", want, joined)
		}
	}
}

func TestConcatCopyArgsWebMOmitsFaststart(t *testing.T) {
	args := ConcatCopyArgs("list.txt", "out.webm", ContainerWebM)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "faststart") {
		t.Errorf("ConcatCopyArgs(webm) should not include faststart: %s", joined)
	}
}

func TestMuxArgsMatrix(t *testing.T) {
	cases := []struct {
		container     Container
		wantFaststart bool
	}{
		{ContainerMP4, true},
		{"", true}, // empty defaults to mp4
		{ContainerMKV, false},
		{ContainerWebM, false},
	}
	for _, tc := range cases {
		got := strings.Join(muxArgs(tc.container), " ")
		hasFaststart := strings.Contains(got, "faststart")
		if hasFaststart != tc.wantFaststart {
			t.Errorf("muxArgs(%q) faststart = %v, want %v (args: %q)", tc.container, hasFaststart, tc.wantFaststart, got)
		}
	}
}

func TestConcatCopyArgsMKVOmitsFaststart(t *testing.T) {
	args := ConcatCopyArgs("list.txt", "out.mkv", ContainerMKV)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "faststart") {
		t.Errorf("ConcatCopyArgs(mkv) should not include faststart: %s", joined)
	}
	if !strings.Contains(joined, "-c copy") {
		t.Errorf("ConcatCopyArgs(mkv) missing -c copy: %s", joined)
	}
}

// ---- Task 4: codec/container/quality matrix ----

func TestReencodeCodecArgsMatrix(t *testing.T) {
	full := Capabilities{HEVCQSV: true, AV1QSV: true, HEVCNVENC: true, AV1NVENC: true, Libx265: true, Libsvtav1: true, Libaomav1: true}
	noneAvailable := Capabilities{}
	svtOnly := Capabilities{Libsvtav1: true}
	aomOnly := Capabilities{Libaomav1: true}

	cases := []struct {
		name    string
		codec   Codec
		mode    Mode
		caps    Capabilities
		wantEnc string
	}{
		{"h264 software default", CodecH264, ModeSoftware, noneAvailable, "libx264"},
		{"h264 qsv when hw capable", CodecH264, ModeQSV, full, "h264_qsv"},
		{"empty codec behaves like h264", "", ModeSoftware, noneAvailable, "libx264"},
		{"h265 software when no hevc_qsv", CodecH265, ModeSoftware, noneAvailable, "libx265"},
		{"h265 qsv when hw capable", CodecH265, ModeQSV, full, "hevc_qsv"},
		{"h265 falls back to libx265 when qsv requested but unavailable", CodecH265, ModeQSV, noneAvailable, "libx265"},
		{"av1 qsv when hw capable", CodecAV1, ModeQSV, full, "av1_qsv"},
		{"av1 prefers libsvtav1 over libaom-av1", CodecAV1, ModeSoftware, full, "libsvtav1"},
		{"av1 falls back to libsvtav1 when only that's available", CodecAV1, ModeSoftware, svtOnly, "libsvtav1"},
		{"av1 falls back to libaom-av1 last", CodecAV1, ModeSoftware, aomOnly, "libaom-av1"},
		{"h264 nvenc when hw capable", CodecH264, ModeNVENC, full, "h264_nvenc"},
		{"h265 nvenc when hw capable", CodecH265, ModeNVENC, full, "hevc_nvenc"},
		{"h265 falls back to libx265 when nvenc requested but unavailable", CodecH265, ModeNVENC, noneAvailable, "libx265"},
		{"av1 nvenc when hw capable", CodecAV1, ModeNVENC, full, "av1_nvenc"},
		{"av1 falls back to software when nvenc requested but unavailable", CodecAV1, ModeNVENC, svtOnly, "libsvtav1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := reencodeCodecArgs(tc.codec, tc.mode, "source", ContainerMP4, tc.caps)
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-c:v "+tc.wantEnc) {
				t.Errorf("reencodeCodecArgs(%q, %q) = %q, want encoder %q", tc.codec, tc.mode, joined, tc.wantEnc)
			}
		})
	}
}

func TestReencodeCodecArgsQualityCRF(t *testing.T) {
	cases := []struct {
		quality string
		wantCRF string
	}{
		{"source", "18"},
		{"", "18"},
		{"high", "20"},
		{"balanced", "22"},
		{"1080p", "22"}, // legacy alias maps to balanced, per the Task 4 brief
		{"small", "28"},
	}
	for _, tc := range cases {
		t.Run(tc.quality, func(t *testing.T) {
			args := reencodeCodecArgs(CodecH264, ModeSoftware, tc.quality, ContainerMP4, Capabilities{})
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-crf "+tc.wantCRF) {
				t.Errorf("reencodeCodecArgs(quality=%q) = %q, want crf %q", tc.quality, joined, tc.wantCRF)
			}
		})
	}
}

// nvenc's -cq is coarser than x264/x265 CRF at the same number, so the
// NVENC branches use crf-2; this pins the full flag set so a stray
// -global_quality or a lost offset can't slip through the matrix test.
func TestReencodeCodecArgsNVENCQualityFlags(t *testing.T) {
	caps := Capabilities{H264NVENC: true, HEVCNVENC: true, AV1NVENC: true}
	cases := []struct {
		codec   Codec
		quality string
		want    string
	}{
		{CodecH264, "source", "-c:v h264_nvenc -preset p4 -rc vbr -cq 16 -b:v 0"},
		{CodecH264, "balanced", "-c:v h264_nvenc -preset p4 -rc vbr -cq 20 -b:v 0"},
		{CodecH265, "small", "-c:v hevc_nvenc -preset p4 -rc vbr -cq 28 -b:v 0 -tag:v hvc1"},
		{CodecAV1, "high", "-c:v av1_nvenc -preset p4 -rc vbr -cq 26 -b:v 0"},
	}
	for _, tc := range cases {
		got := strings.Join(reencodeCodecArgs(tc.codec, ModeNVENC, tc.quality, ContainerMP4, caps), " ")
		if got != tc.want {
			t.Errorf("reencodeCodecArgs(%s, nvenc, %s) = %q, want %q", tc.codec, tc.quality, got, tc.want)
		}
		if strings.Contains(got, "-global_quality") || strings.Contains(got, "-crf") {
			t.Errorf("nvenc args must not carry QSV/software quality flags: %q", got)
		}
	}
}

func TestReencodeArgsUsesSpecCodecContainerQuality(t *testing.T) {
	spec := ReencodeSpec{
		Items:     []ReencodeInput{{Path: "a.mp4", OutS: 10}},
		Mode:      ModeSoftware,
		Codec:     CodecH265,
		Quality:   "small",
		Container: ContainerMKV,
	}
	args := ReencodeArgs(spec, nil, "script.txt", "vout", "out.mkv")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v libx265") {
		t.Errorf("ReencodeArgs should use libx265: %s", joined)
	}
	if !strings.Contains(joined, "-crf 30") {
		t.Errorf("ReencodeArgs should use small-quality crf 30: %s", joined)
	}
	if strings.Contains(joined, "faststart") {
		t.Errorf("ReencodeArgs(mkv) should not include faststart: %s", joined)
	}
	if strings.Contains(joined, "hvc1") {
		t.Errorf("ReencodeArgs(h265, mkv) should not tag hvc1 (mp4-only quirk): %s", joined)
	}
}

// TestReencodeCodecArgsH265MP4TagsHVC1 is a regression test for Fix round
// 1: an mp4-muxed HEVC stream defaults to the "hev1" sample-entry tag,
// which Safari/QuickTime refuse to play — they require "hvc1". mkv/webm
// don't need this (mkv doesn't use the tag; webm can't carry h265 at all).
func TestReencodeCodecArgsH265MP4TagsHVC1(t *testing.T) {
	cases := []struct {
		name      string
		container Container
		wantTag   bool
	}{
		{"mp4 explicit", ContainerMP4, true},
		{"mp4 default (empty container)", "", true},
		{"mkv does not need the tag", ContainerMKV, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := reencodeCodecArgs(CodecH265, ModeSoftware, "source", tc.container, Capabilities{})
			joined := strings.Join(args, " ")
			got := strings.Contains(joined, "-tag:v hvc1")
			if got != tc.wantTag {
				t.Errorf("reencodeCodecArgs(h265, container=%q) hvc1 tag = %v, want %v: %s", tc.container, got, tc.wantTag, joined)
			}
		})
	}

	// Also confirmed through the full ReencodeArgs path (hw QSV branch),
	// not just the software one above.
	spec := ReencodeSpec{
		Items:     []ReencodeInput{{Path: "a.mp4", OutS: 10}},
		Mode:      ModeQSV,
		Codec:     CodecH265,
		Container: ContainerMP4,
		Caps:      Capabilities{HEVCQSV: true},
	}
	args := ReencodeArgs(spec, nil, "script.txt", "vout", "out.mp4")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v hevc_qsv") || !strings.Contains(joined, "-tag:v hvc1") {
		t.Errorf("ReencodeArgs(h265 qsv, mp4) missing hevc_qsv or hvc1 tag: %s", joined)
	}
}

func TestContainerExtAndContentType(t *testing.T) {
	cases := []struct {
		c        Container
		wantExt  string
		wantType string
	}{
		{ContainerMP4, "mp4", "video/mp4"},
		{ContainerMKV, "mkv", "video/x-matroska"},
		{ContainerWebM, "webm", "video/webm"},
		{"", "mp4", "video/mp4"},
	}
	for _, tc := range cases {
		if got := ContainerExt(tc.c); got != tc.wantExt {
			t.Errorf("ContainerExt(%q) = %q, want %q", tc.c, got, tc.wantExt)
		}
		if got := ContainerContentType(tc.c); got != tc.wantType {
			t.Errorf("ContainerContentType(%q) = %q, want %q", tc.c, got, tc.wantType)
		}
	}
}

func TestExportOutputPathUsesContainerExt(t *testing.T) {
	cases := []struct {
		c    Container
		want string
	}{
		{ContainerMP4, "/cache/exports/5.mp4"},
		{ContainerMKV, "/cache/exports/5.mkv"},
		{ContainerWebM, "/cache/exports/5.webm"},
		{"", "/cache/exports/5.mp4"},
	}
	for _, tc := range cases {
		if got := ExportOutputPath("/cache", 5, tc.c); got != tc.want {
			t.Errorf("ExportOutputPath(%q) = %q, want %q", tc.c, got, tc.want)
		}
	}
}
