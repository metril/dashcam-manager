// stitch.go implements M11's clip-export pipeline: planning whether an
// export can use ffmpeg's fast concat-demuxer `-c copy` path or needs a
// full re-encode, building the concat list / filter_complex graph, and
// running ffmpeg to produce the final output file. Detection burn-in
// (overlay.go) plugs into the re-encode path as extra per-input filters.
package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---- plan builder ----

// PlanItem is the subset of one export item's shape needed to decide
// between the fast concat-copy path and a full re-encode: whether it
// covers the entire source segment (Whole) and, if not, whether its
// trim points land on keyframes (KeyframeAligned, only consulted when
// !Whole). Keyframe detection itself requires an ffprobe round trip
// (see ProbeKeyframeTimes/NearestKeyframe) and is deliberately kept out
// of this type so BuildPlan stays a pure function callers can unit test
// without touching ffmpeg.
type PlanItem struct {
	Whole           bool
	KeyframeAligned bool
}

// Plan is the outcome of BuildPlan.
type Plan struct {
	// FastPath is true when every item can be stitched with the concat
	// demuxer's `-c copy` (no re-encode): burn-in is off, only one
	// camera is requested, and every item is either a whole segment or
	// trimmed exactly on a keyframe.
	FastPath bool
}

// BuildPlan decides the export strategy. Per the M11 brief, only two
// strategies are implemented (the hybrid "re-encode just the trimmed
// head/tail, copy the rest" path is explicitly skipped): fast concat-copy
// when possible, otherwise a full re-encode of everything.
func BuildPlan(items []PlanItem, camera string, burnIn bool) Plan {
	if burnIn || camera == "both" {
		return Plan{FastPath: false}
	}
	for _, it := range items {
		if !it.Whole && !it.KeyframeAligned {
			return Plan{FastPath: false}
		}
	}
	return Plan{FastPath: true}
}

// ---- rear-camera offset math ----

// RearSegmentPlacement is one rear-camera segment's placement on a trip's
// shared timeline: SegmentID plus [OffsetS, OffsetS+DurationS) in seconds
// from the trip's started_at.
type RearSegmentPlacement struct {
	SegmentID int64
	OffsetS   float64
	DurationS float64
}

// ResolveRearSegment finds which rear segment corresponds to a front-camera
// instant frontTripOffsetS (trip-relative seconds), given the trip's
// pair_offset_s (rear.start - front.start, a median clock-offset computed
// by internal/scan/pair.go). The same physical moment lands
// pairOffsetS seconds later (or earlier, if negative) on the rear
// recorder's clock, so the rear-timeline instant to look up is
// frontTripOffsetS + pairOffsetS. rearSegs need not be pre-sorted.
// Returns ok=false if no rear segment's [OffsetS, OffsetS+DurationS) range
// contains that instant (a gap, or the trip has no rear footage there).
func ResolveRearSegment(rearSegs []RearSegmentPlacement, frontTripOffsetS, pairOffsetS float64) (segmentID int64, localOffsetS float64, ok bool) {
	target := frontTripOffsetS + pairOffsetS
	for _, s := range rearSegs {
		if target >= s.OffsetS && target < s.OffsetS+s.DurationS {
			return s.SegmentID, target - s.OffsetS, true
		}
	}
	return 0, 0, false
}

// ---- concat demuxer list ----

// ConcatEntry is one file (or file range) in an ffmpeg concat-demuxer list.
// InS/OutS are input-relative seconds; OutS <= InS means "to end of file"
// (no outpoint directive is written).
type ConcatEntry struct {
	Path string
	InS  float64
	OutS float64
}

// escapeConcatPath escapes a path for the concat demuxer's single-quoted
// `file '...'` directive: per ffmpeg's documentation, a literal `'` inside
// the quoted value is written as `'\”` (close the quote, an escaped
// literal quote, reopen the quote).
func escapeConcatPath(path string) string {
	return strings.ReplaceAll(path, `'`, `'\''`)
}

// formatSeconds renders a duration with millisecond precision, trimming
// trailing zeros so e.g. 5 renders as "5" and 5.5 as "5.5" rather than
// "5.000000" — cosmetic, but keeps generated concat lists/filter scripts
// easy to eyeball in logs.
func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', -1, 64)
}

// WriteConcatList writes an ffmpeg concat-demuxer list (the `-f concat
// -safe 0 -i <listfile>` input) for entries, in order, to w.
func WriteConcatList(w io.Writer, entries []ConcatEntry) error {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "file '%s'\n", escapeConcatPath(e.Path))
		if e.InS > 0 {
			fmt.Fprintf(&b, "inpoint %s\n", formatSeconds(e.InS))
		}
		if e.OutS > e.InS {
			fmt.Fprintf(&b, "outpoint %s\n", formatSeconds(e.OutS))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// BuildConcatList is WriteConcatList rendered to a string, for callers
// (and tests) that want the text directly rather than writing to an
// io.Writer.
func BuildConcatList(entries []ConcatEntry) string {
	var b strings.Builder
	_ = WriteConcatList(&b, entries) // strings.Builder.Write never errors
	return b.String()
}

// ---- ffmpeg invocations ----

// ConcatCopyArgs builds the fast-path ffmpeg args: concat demuxer, stream
// copy, plus faststart moov atom placement for progressive download/
// seeking when the output is an mp4 (faststart is an mp4/mov-muxer-only
// option; mkv doesn't need it and ffmpeg would warn on a muxer that
// doesn't understand it, so it's only added for ContainerMP4/"").
func ConcatCopyArgs(listPath, out string, container Container) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy",
	}
	args = append(args, muxArgs(container)...)
	return append(args, out)
}

// muxArgs returns extra muxer flags for container, or nil if none apply.
func muxArgs(container Container) []string {
	if container == ContainerMKV || container == ContainerWebM {
		return nil
	}
	return []string{"-movflags", "+faststart"}
}

// ---- codec/container/quality (Task 4: export formats) ----

// Codec is the requested output video codec for a re-encode export.
// CodecCopy means "stream-copy the source codec" and never reaches
// reencodeCodecArgs — see BuildPlan/planExport, which is the only path
// that can produce a copy export. The empty string is a synonym for
// CodecCopy, preserving pre-Task-4 behavior for API callers that don't
// send a codec field at all: try the fast concat-copy path when the
// existing camera/burn-in/keyframe-alignment rules allow it, otherwise
// re-encode to h264 (see reencodeCodecArgs' default case).
type Codec string

const (
	CodecCopy Codec = "copy"
	CodecH264 Codec = "h264"
	CodecH265 Codec = "h265"
	CodecAV1  Codec = "av1"
)

// Container is the requested output container/muxer.
type Container string

const (
	ContainerMP4  Container = "mp4"
	ContainerMKV  Container = "mkv"
	ContainerWebM Container = "webm"
)

// ContainerExt returns the file extension (no leading dot) for c, defaulting
// to mp4 for "" or an unrecognized value.
func ContainerExt(c Container) string {
	switch c {
	case ContainerMKV:
		return "mkv"
	case ContainerWebM:
		return "webm"
	default:
		return "mp4"
	}
}

// ContainerContentType returns the HTTP Content-Type for c, defaulting to
// video/mp4.
func ContainerContentType(c Container) string {
	switch c {
	case ContainerMKV:
		return "video/x-matroska"
	case ContainerWebM:
		return "video/webm"
	default:
		return "video/mp4"
	}
}

// qualityCRF is the per-codec CRF/global_quality value one "quality" preset
// maps to. Values are chosen so that x264/qsv-h264's "balanced" preset
// exactly matches the CRF/global_quality this package always used before
// Task 4 introduced per-quality tuning (22) — old callers passing
// quality="1080p" (which this package has always mapped to "a scale +
// balanced preset", per the Task 4 brief) see identical encode output to
// before, only now expressed through this table. "source"/"" (the other
// pre-Task-4 default) gets a near-lossless preset instead of reusing 22 —
// a deliberate quality improvement for "source quality" exports, since it
// now means something distinct from "balanced": see docs/superpowers or
// the Task 4 report for the rationale.
type qualityCRF struct{ x264, x265, av1 int }

var qualityCRFTable = map[string]qualityCRF{
	"source":   {x264: 18, x265: 20, av1: 24},
	"high":     {x264: 20, x265: 22, av1: 28},
	"balanced": {x264: 22, x265: 24, av1: 30},
	"small":    {x264: 28, x265: 30, av1: 38},
}

// crfForQuality resolves a request's quality string to its CRF/
// global_quality preset. "1080p" (legacy) maps to "balanced" per the Task
// 4 brief; "" and any unrecognized value map to "source" (matching
// ResolveOutputDims' existing "unknown means keep source resolution"
// fallback).
func crfForQuality(quality string) qualityCRF {
	switch quality {
	case "high":
		return qualityCRFTable["high"]
	case "balanced", "1080p":
		return qualityCRFTable["balanced"]
	case "small":
		return qualityCRFTable["small"]
	default: // "", "source"
		return qualityCRFTable["source"]
	}
}

// ResolveOutputDims returns the concrete output pixel width/height for a
// quality preset: "1080p" scales to 1920 wide (preserving srcW/srcH's
// aspect ratio), anything else (including "source" or unknown) keeps the
// source resolution exactly. Unlike an older "0 means no scale filter"
// design, this always returns real numbers — burn-in box coordinates need
// actual output pixel dimensions to draw correctly even when no scaling is
// applied, so ReencodeSpec.OutW/OutH are always set to this function's
// result (BuildFilterComplex's scale filter is then a same-size no-op for
// "source" quality, which ffmpeg handles cheaply).
func ResolveOutputDims(quality string, srcW, srcH int) (w, h int) {
	if quality != "1080p" || srcW <= 0 || srcH <= 0 {
		return srcW, srcH
	}
	w = 1920
	h = int(float64(srcH) * float64(w) / float64(srcW))
	h -= h % 2 // even height: required by most H.264/HEVC encoders
	return w, h
}

// reencodeCodecArgs returns the video-codec args for the full re-encode
// path, choosing hardware QSV when mode is qsv AND caps says the specific
// QSV encoder actually works, otherwise the matching software encoder:
// libx264 (h264), libx265 (h265), libsvtav1 then libaom-av1 (av1, per the
// Task 4 brief's "libaom-av1 last" ordering — libsvtav1 is both faster and
// generally regarded as higher quality per encode second, so it's
// preferred whenever present). VAAPI re-encode isn't wired up here — same
// as before Task 4, the qsv/software split matches what internal/media
// already supports elsewhere.
//
// codec is assumed already validated against caps by the HTTP layer
// (cmd/api's export capabilities check) — this function degrades to
// software rather than erroring so a job already queued before a
// capability changed (or a caller that skipped validation, e.g. a test)
// still produces *something* rather than emitting an encoder ffmpeg can't
// run. CodecCopy/"" fall through to the h264 case, but BuildPlan/
// planExport never route a copy-eligible export through this function in
// practice — see ReencodeArgs' caller.
//
// container matters only for CodecH265: an mp4-muxed HEVC stream defaults
// to the "hev1" sample-entry tag, which Safari/QuickTime refuse to play
// (they only recognize "hvc1"); mkv/webm have no such quirk (mkv doesn't
// use these tags at all, and webm can't carry h265 in the first place —
// see exportRequest.validate()'s container/codec matrix), so `-tag:v
// hvc1` is only added for CodecH265+mp4 (Fix round 1).
// nvencArgs builds the common *_nvenc encoder flags: nvenc's -cq is a
// coarser quality scale than x264/x265/CRF at the same number, so callers
// pass crf-2 as q to land at a comparable visual quality; never combine
// with -global_quality (that's the QSV knob).
func nvencArgs(encoder string, q int) []string {
	return []string{"-c:v", encoder, "-preset", "p4", "-rc", "vbr", "-cq", strconv.Itoa(q), "-b:v", "0"}
}

func reencodeCodecArgs(codec Codec, mode Mode, quality string, container Container, caps Capabilities) []string {
	crf := crfForQuality(quality)
	switch codec {
	case CodecH265:
		var args []string
		switch {
		case mode == ModeNVENC && caps.HEVCNVENC:
			args = nvencArgs("hevc_nvenc", crf.x265-2)
		case mode == ModeQSV && caps.HEVCQSV:
			args = []string{"-c:v", "hevc_qsv", "-global_quality", strconv.Itoa(crf.x265)}
		default:
			args = []string{"-c:v", "libx265", "-crf", strconv.Itoa(crf.x265), "-preset", "veryfast"}
		}
		if container == ContainerMP4 || container == "" {
			args = append(args, "-tag:v", "hvc1")
		}
		return args
	case CodecAV1:
		if mode == ModeNVENC && caps.AV1NVENC {
			return nvencArgs("av1_nvenc", crf.av1-2)
		}
		if mode == ModeQSV && caps.AV1QSV {
			return []string{"-c:v", "av1_qsv", "-global_quality", strconv.Itoa(crf.av1)}
		}
		if caps.Libsvtav1 {
			return []string{"-c:v", "libsvtav1", "-crf", strconv.Itoa(crf.av1), "-preset", "8"}
		}
		return []string{"-c:v", "libaom-av1", "-crf", strconv.Itoa(crf.av1), "-cpu-used", "6"}
	default: // CodecH264, CodecCopy, ""
		// Unlike hevc_qsv/av1_qsv below, h264_qsv isn't gated on
		// caps.H264QSV here: Mode itself (media.Detect, resolved once at
		// boot) already only ever reports ModeQSV after confirming
		// h264_qsv's pipeline initializes — see hwaccel.go's probeQSV,
		// which Detect calls before returning ModeQSV. Gating on a second
		// capability flag here would be redundant and (pre-Task-4
		// behavior) was never done. Same reasoning extends to h264_nvenc/
		// ModeNVENC below (see probeNVENCEncoder, which Detect also
		// confirms before returning ModeNVENC).
		if mode == ModeNVENC {
			return nvencArgs("h264_nvenc", crf.x264-2)
		}
		if mode == ModeQSV {
			return []string{"-c:v", "h264_qsv", "-global_quality", strconv.Itoa(crf.x264)}
		}
		return []string{"-c:v", "libx264", "-crf", strconv.Itoa(crf.x264), "-preset", "veryfast"}
	}
}

// ReencodeInput is one trimmed clip contributing to a full-re-encode
// export: Path plus its [InS,OutS) trim window (input-relative seconds;
// OutS<=InS means "to end of file"). Overlay, if non-empty, is this
// input's burn-in filter chain fragment for the small-box-count script
// method (see overlay.go's BuildDrawFilters), already time-shifted so t=0
// is the trimmed clip's start. OverlayPNGDir, if non-empty, is a
// directory of overlay_%05d.png frames (see overlay.go's
// RenderOverlayFrames) for the large-box-count method — mutually
// exclusive with Overlay; callers pick exactly one method per export (see
// cmd/api's burn-in box-count decision).
type ReencodeInput struct {
	Path          string
	InS           float64
	OutS          float64
	Overlay       string
	OverlayPNGDir string
}

// ReencodeSpec describes a full ffmpeg re-encode: Items are the primary
// (front, or the only camera's) trimmed inputs in output order. Rear, if
// non-empty, must be the same length as Items — Rear[i] is the paired
// rear-camera input hstacked alongside Items[i] for a "both" export.
type ReencodeSpec struct {
	Items      []ReencodeInput
	Rear       []ReencodeInput
	OutW, OutH int // 0,0 = keep source resolution (no scale filter)
	Mode       Mode
	Device     string
	// Codec, Quality, Container and Caps are Task 4's export-format
	// controls: Codec/Quality select reencodeCodecArgs' encoder+CRF,
	// Container selects ReencodeArgs' muxer flags, and Caps (from
	// media.DetectCapabilities) gates which QSV encoders
	// reencodeCodecArgs is allowed to pick. All four zero-value to the
	// pre-Task-4 behavior (h264, source-quality CRF, mp4, no QSV
	// hardware beyond h264 which Mode/Device alone already covered).
	Codec     Codec
	Quality   string
	Container Container
	Caps      Capabilities
}

// videoInputOrder returns spec's video inputs in the exact order both
// BuildFilterComplex and ReencodeArgs emit `-i` flags in — front items in
// order for a single-camera export, or front/rear pairs interleaved
// (front0, rear0, front1, rear1, ...) for "both". BuildFilterComplex's
// filter-graph indices are positions in this list, so ReencodeArgs MUST
// emit inputs in this same order or the graph would reference the wrong
// input for a "both" export with more than one item.
func videoInputOrder(spec ReencodeSpec) []ReencodeInput {
	if len(spec.Rear) == 0 {
		return spec.Items
	}
	order := make([]ReencodeInput, 0, len(spec.Items)*2)
	for i, it := range spec.Items {
		order = append(order, it, spec.Rear[i])
	}
	return order
}

// scaleFilter returns a `scale=W:H` filter fragment, or "" if outW/outH
// are 0 (source resolution — no filter needed).
func scaleFilter(outW, outH int) string {
	if outW <= 0 || outH <= 0 {
		return ""
	}
	return fmt.Sprintf("scale=%d:%d", outW, outH)
}

// buildTrimmedChain returns the filter_complex fragment for one input at
// graph index idx: scale (if requested) + setpts (reset to 0 at the trim
// point) + the input's own script-method burn-in overlay (if any), ending
// in a labeled output pad `[vN]`.
func buildTrimmedChain(idx int, outW, outH int, overlay string) (frag, label string) {
	label = fmt.Sprintf("v%d", idx)
	steps := []string{fmt.Sprintf("[%d:v]", idx)}
	var ops []string
	if sf := scaleFilter(outW, outH); sf != "" {
		ops = append(ops, sf)
	}
	ops = append(ops, "setpts=PTS-STARTPTS")
	if overlay != "" {
		ops = append(ops, overlay)
	}
	steps = append(steps, strings.Join(ops, ","))
	return strings.Join(steps, "") + fmt.Sprintf("[%s]", label), label
}

// BuildFilterComplex builds the full filter_complex graph for a
// re-encode export: per-input scale+trim+script-burn-in, then (for any
// input with OverlayPNGDir set) an `overlay=` compositing its PNG
// sequence input on top, then either a straight concat (single camera) or
// a per-pair hstack (front|rear side by side) followed by concat ("both"
// camera). Returns the graph text (suitable for -filter_complex_script),
// the output pad name to -map (without brackets), and overlayDirs — the
// PNG overlay directories in the order their corresponding extra `-i`
// flags must be appended after the video inputs (see ReencodeArgs).
func BuildFilterComplex(spec ReencodeSpec) (script, outLabel string, overlayDirs []string) {
	order := videoInputOrder(spec)
	var parts []string
	pngIdx := len(order) // PNG overlay inputs are appended after every video input

	chainLabel := make([]string, len(order))
	for idx, it := range order {
		frag, lbl := buildTrimmedChain(idx, spec.OutW, spec.OutH, it.Overlay)
		parts = append(parts, frag)
		label := lbl
		if it.OverlayPNGDir != "" {
			ovLbl := lbl + "p"
			parts = append(parts, OverlayFilterFragment(lbl, pngIdx, ovLbl))
			overlayDirs = append(overlayDirs, it.OverlayPNGDir)
			pngIdx++
			label = ovLbl
		}
		chainLabel[idx] = label
	}

	outLabel = "vout"
	if len(spec.Rear) == 0 {
		concatIn := ""
		for _, l := range chainLabel {
			concatIn += "[" + l + "]"
		}
		parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[%s]", concatIn, len(chainLabel), outLabel))
		return strings.Join(parts, ";"), outLabel, overlayDirs
	}

	// order is interleaved front,rear,front,rear,...; pair them back up
	// for hstack, then concat every hstacked pair in item order.
	hstackLabels := make([]string, len(spec.Items))
	for i := range spec.Items {
		frontLbl, rearLbl := chainLabel[2*i], chainLabel[2*i+1]
		hLbl := fmt.Sprintf("h%d", i)
		parts = append(parts, fmt.Sprintf("[%s][%s]hstack=inputs=2[%s]", frontLbl, rearLbl, hLbl))
		hstackLabels[i] = hLbl
	}
	concatIn := ""
	for _, l := range hstackLabels {
		concatIn += "[" + l + "]"
	}
	parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[%s]", concatIn, len(hstackLabels), outLabel))
	return strings.Join(parts, ";"), outLabel, overlayDirs
}

// ReencodeInputArgs returns the `-ss ... -to ... -i path` args for one
// trimmed input, in the order ffmpeg expects (input-seeking flags before
// -i). OutS<=InS omits -to (read to EOF).
func ReencodeInputArgs(in ReencodeInput) []string {
	args := []string{}
	if in.InS > 0 {
		args = append(args, "-ss", formatSeconds(in.InS))
	}
	if in.OutS > in.InS {
		args = append(args, "-to", formatSeconds(in.OutS))
	}
	return append(args, "-i", in.Path)
}

// ReencodeArgs assembles the complete ffmpeg argv for a full-re-encode
// export: video inputs in videoInputOrder(spec)'s order, then overlayDirs'
// PNG-sequence inputs (must be BuildFilterComplex's returned overlayDirs,
// in that same order — see its doc comment), the filter_complex script,
// and the output codec/mux flags. filterScriptPath must already contain
// the text from BuildFilterComplex.
func ReencodeArgs(spec ReencodeSpec, overlayDirs []string, filterScriptPath, outLabel, out string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if (spec.Mode == ModeQSV || spec.Mode == ModeNVENC) && spec.Device != "" {
		// Bind the encoder to the configured device (DRI render node for
		// qsv, CUDA index for nvenc — matches thumb.go's hwaccel pattern)
		// rather than letting the encoder pick ffmpeg's default device,
		// which may not be the configured one on a multi-GPU host. The
		// filter graph itself stays software either way (nvenc, like
		// qsv's h264_qsv path here, uploads sw frames itself — see
		// reencodeCodecArgs), so this is only ever the device-init pair,
		// never a per-input hwInputArgs.
		args = append(args, hwInitArgs(spec.Mode, spec.Device)...)
	}
	for _, it := range videoInputOrder(spec) {
		args = append(args, ReencodeInputArgs(it)...)
	}
	for _, dir := range overlayDirs {
		args = append(args, OverlayInputArgs(dir)...)
	}
	args = append(args, "-filter_complex_script", filterScriptPath, "-map", "["+outLabel+"]", "-an")
	args = append(args, reencodeCodecArgs(spec.Codec, spec.Mode, spec.Quality, spec.Container, spec.Caps)...)
	args = append(args, muxArgs(spec.Container)...)
	args = append(args, out)
	return args
}

// ---- keyframe probing (fast-path eligibility for a partial trim) ----

// ProbeKeyframeTimes runs ffprobe to list keyframe (I-frame) presentation
// timestamps in path, restricted to a window around atS (atS-windowS to
// atS+windowS) so probing a trim point near the end of a long file doesn't
// require decoding frame headers for the whole thing.
func ProbeKeyframeTimes(ctx context.Context, path string, atS, windowS float64) ([]float64, error) {
	start := atS - windowS
	if start < 0 {
		start = 0
	}
	args := []string{
		"-v", "error",
		"-read_intervals", fmt.Sprintf("%s%%+%s", formatSeconds(start), formatSeconds(2*windowS)),
		"-skip_frame", "nokey",
		"-show_entries", "frame=pts_time",
		"-of", "csv=p=0",
		path,
	}
	out, err := runFFprobeText(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("media: probe keyframes %s: %w", path, err)
	}
	var times []float64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if v, err := strconv.ParseFloat(line, 64); err == nil {
			times = append(times, v)
		}
	}
	return times, nil
}

// NearestKeyframe reports whether atS is within toleranceS of any of
// times — a pure helper split out from ProbeKeyframeTimes so the
// alignment decision itself is unit-testable without ffprobe.
func NearestKeyframe(times []float64, atS, toleranceS float64) bool {
	for _, t := range times {
		d := t - atS
		if d < 0 {
			d = -d
		}
		if d <= toleranceS {
			return true
		}
	}
	return false
}

// ---- output paths ----

// ExportOutputPath returns the cache path an export job writes its
// download-target output to, before the job handler moves/records it:
// $DASHCAM_CACHE/exports/<id>.<ext>, ext from ContainerExt(container).
func ExportOutputPath(cacheDir string, exportID int64, container Container) string {
	return filepath.Join(cacheDir, "exports", fmt.Sprintf("%d.%s", exportID, ContainerExt(container)))
}

// EnsureExportDir creates the directory for out (an export output path)
// if it doesn't already exist.
func EnsureExportDir(out string) error {
	return os.MkdirAll(filepath.Dir(out), 0o755)
}

// RunConcatCopy runs the fast-path ffmpeg concat-copy export.
func RunConcatCopy(ctx context.Context, listPath, out string, container Container, progress ProgressFunc) error {
	if err := EnsureExportDir(out); err != nil {
		return fmt.Errorf("media: export mkdir: %w", err)
	}
	args := ConcatCopyArgs(listPath, out, container)
	if progress != nil {
		args = append(args, "-progress", "pipe:1", "-nostats")
	}
	if err := runFFmpeg(ctx, args, progress); err != nil {
		return fmt.Errorf("media: export concat copy: %w", err)
	}
	return nil
}

// RunReencode runs the full-re-encode export, falling back to software
// libx264 once if the hardware (QSV/NVENC) pipeline fails (mirroring
// Thumbnail/Sprite's hwaccel-failure fallback behavior).
func RunReencode(ctx context.Context, spec ReencodeSpec, overlayDirs []string, filterScriptPath, outLabel, out string, progress ProgressFunc) error {
	if err := EnsureExportDir(out); err != nil {
		return fmt.Errorf("media: export mkdir: %w", err)
	}
	args := ReencodeArgs(spec, overlayDirs, filterScriptPath, outLabel, out)
	if progress != nil {
		args = append(args, "-progress", "pipe:1", "-nostats")
	}
	err := runFFmpeg(ctx, args, progress)
	if err != nil && spec.Mode != ModeSoftware {
		swSpec := spec
		swSpec.Mode = ModeSoftware
		swArgs := ReencodeArgs(swSpec, overlayDirs, filterScriptPath, outLabel, out)
		if progress != nil {
			swArgs = append(swArgs, "-progress", "pipe:1", "-nostats")
		}
		err = runFFmpeg(ctx, swArgs, progress)
	}
	if err != nil {
		return fmt.Errorf("media: export re-encode: %w", err)
	}
	return nil
}
