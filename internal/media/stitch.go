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
// the quoted value is written as `'\''` (close the quote, an escaped
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
// copy, faststart moov atom placement for progressive download/seeking.
func ConcatCopyArgs(listPath, out string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy", "-movflags", "+faststart",
		out,
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
// path: hardware QSV when mode is qsv, otherwise software libx264. VAAPI
// re-encode (as opposed to thumbnail scale_vaapi) isn't wired up here —
// the qsv/software split matches what internal/media already supports
// elsewhere, and the M11 brief only calls out "h264_qsv or software
// libx264 fallback".
func reencodeCodecArgs(mode Mode) []string {
	if mode == ModeQSV {
		return []string{"-c:v", "h264_qsv", "-global_quality", "22"}
	}
	return []string{"-c:v", "libx264", "-crf", "22", "-preset", "veryfast"}
}

// ReencodeInput is one trimmed clip contributing to a full-re-encode
// export: Path plus its [InS,OutS) trim window (input-relative seconds;
// OutS<=InS means "to end of file"). Overlay, if non-empty, is this
// input's burn-in filter chain fragment (see overlay.go's
// BuildDrawFilters), already time-shifted so t=0 is the trimmed clip's
// start.
type ReencodeInput struct {
	Path    string
	InS     float64
	OutS    float64
	Overlay string
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
// point) + the input's own burn-in overlay (if any), ending in a labeled
// output pad `[vN]`.
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
// re-encode export: per-input scale+trim+burn-in, then either a straight
// concat (single camera) or a per-item hstack (front|rear side by side)
// followed by concat ("both" camera). Returns the graph text (suitable
// for -filter_complex_script) and the output pad name to -map (without
// brackets).
func BuildFilterComplex(spec ReencodeSpec) (script, outLabel string) {
	var parts []string
	nextIdx := 0

	if len(spec.Rear) == 0 {
		// single camera: [0:v]...[v0];[1:v]...[v1];...;[v0][v1]...concat
		labels := make([]string, len(spec.Items))
		for i, it := range spec.Items {
			frag, lbl := buildTrimmedChain(nextIdx, spec.OutW, spec.OutH, it.Overlay)
			parts = append(parts, frag)
			labels[i] = lbl
			nextIdx++
		}
		concatIn := ""
		for _, l := range labels {
			concatIn += "[" + l + "]"
		}
		outLabel = "vout"
		parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[%s]", concatIn, len(labels), outLabel))
		return strings.Join(parts, ";"), outLabel
	}

	// "both": each item pairs a front input with a rear input, hstacked,
	// then every pair is concatenated in order.
	hstackLabels := make([]string, len(spec.Items))
	for i, it := range spec.Items {
		frontFrag, frontLbl := buildTrimmedChain(nextIdx, spec.OutW, spec.OutH, it.Overlay)
		parts = append(parts, frontFrag)
		nextIdx++

		rearFrag, rearLbl := buildTrimmedChain(nextIdx, spec.OutW, spec.OutH, spec.Rear[i].Overlay)
		parts = append(parts, rearFrag)
		nextIdx++

		hLbl := fmt.Sprintf("h%d", i)
		parts = append(parts, fmt.Sprintf("[%s][%s]hstack=inputs=2[%s]", frontLbl, rearLbl, hLbl))
		hstackLabels[i] = hLbl
	}
	concatIn := ""
	for _, l := range hstackLabels {
		concatIn += "[" + l + "]"
	}
	outLabel = "vout"
	parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[%s]", concatIn, len(hstackLabels), outLabel))
	return strings.Join(parts, ";"), outLabel
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
// export: every input (front items, then paired rear items if any), the
// filter_complex script, and the output codec/mux flags. filterScriptPath
// must already contain the text from BuildFilterComplex.
func ReencodeArgs(spec ReencodeSpec, filterScriptPath, outLabel, out string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	for _, it := range spec.Items {
		args = append(args, ReencodeInputArgs(it)...)
	}
	for _, it := range spec.Rear {
		args = append(args, ReencodeInputArgs(it)...)
	}
	args = append(args, "-filter_complex_script", filterScriptPath, "-map", "["+outLabel+"]", "-an")
	args = append(args, reencodeCodecArgs(spec.Mode)...)
	args = append(args, "-movflags", "+faststart", out)
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
// $DASHCAM_CACHE/exports/<id>.mp4.
func ExportOutputPath(cacheDir string, exportID int64) string {
	return filepath.Join(cacheDir, "exports", fmt.Sprintf("%d.mp4", exportID))
}

// EnsureExportDir creates the directory for out (an export output path)
// if it doesn't already exist.
func EnsureExportDir(out string) error {
	return os.MkdirAll(filepath.Dir(out), 0o755)
}

// RunConcatCopy runs the fast-path ffmpeg concat-copy export.
func RunConcatCopy(ctx context.Context, listPath, out string, progress ProgressFunc) error {
	if err := EnsureExportDir(out); err != nil {
		return fmt.Errorf("media: export mkdir: %w", err)
	}
	args := ConcatCopyArgs(listPath, out)
	if progress != nil {
		args = append(args, "-progress", "pipe:1", "-nostats")
	}
	if err := runFFmpeg(ctx, args, progress); err != nil {
		return fmt.Errorf("media: export concat copy: %w", err)
	}
	return nil
}

// RunReencode runs the full-re-encode export, falling back to software
// libx264 once if the QSV pipeline fails (mirroring Thumbnail/Sprite's
// hwaccel-failure fallback behavior).
func RunReencode(ctx context.Context, spec ReencodeSpec, filterScriptPath, outLabel, out string, progress ProgressFunc) error {
	if err := EnsureExportDir(out); err != nil {
		return fmt.Errorf("media: export mkdir: %w", err)
	}
	args := ReencodeArgs(spec, filterScriptPath, outLabel, out)
	if progress != nil {
		args = append(args, "-progress", "pipe:1", "-nostats")
	}
	err := runFFmpeg(ctx, args, progress)
	if err != nil && spec.Mode == ModeQSV {
		swSpec := spec
		swSpec.Mode = ModeSoftware
		swArgs := ReencodeArgs(swSpec, filterScriptPath, outLabel, out)
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
