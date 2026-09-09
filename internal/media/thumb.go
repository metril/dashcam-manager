package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultThumbWidth/defaultSpriteCols/Rows/TileWidth match the M8 brief's
// defaults; callers pass explicit values when they know better (e.g. a
// probed source aspect ratio).
const (
	defaultThumbWidth  = 480
	defaultSpriteCols  = 5
	defaultSpriteRows  = 2
	defaultTileWidth   = 320
	defaultTileAspect  = 16.0 / 9.0 // segments are 3840x2160
	defaultSegmentSecs = 30.0       // fallback when duration is unknown

	// servedSpriteBasename is the filename a browser actually requests a
	// segment's sprite sheet under (GET /api/segments/{id}/sprite.jpg),
	// independent of whatever basename the cache stores it as on disk —
	// WebVTT cues must reference this name, not the cache filename.
	servedSpriteBasename = "sprite.jpg"
)

// ThumbOptions configures a single-frame Thumbnail extraction.
type ThumbOptions struct {
	Mode   Mode
	Device string  // DRI render node, used when Mode is qsv/vaapi
	AtSec  float64 // seek position; negative treated as 0
	Width  int     // output width in px; <=0 uses defaultThumbWidth
}

// Thumbnail extracts a single JPEG frame from in at opts.AtSec into out,
// using opts.Mode's hardware pipeline. On any hardware-path failure it
// logs and retries once in software before giving up, so a QSV/VAAPI
// hiccup on one file never fails the whole thumbs job. Writes via a
// temp file + rename so a killed/failed run never leaves a corrupt/partial
// thumbnail at the final path.
func Thumbnail(ctx context.Context, in, out string, opts ThumbOptions) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("media: thumbnail mkdir: %w", err)
	}
	tmp, err := tempOutputPath(filepath.Dir(out), ".thumb-*.jpg")
	if err != nil {
		return fmt.Errorf("media: thumbnail temp file: %w", err)
	}
	defer os.Remove(tmp)

	err = runFFmpeg(ctx, thumbArgs(in, tmp, opts), nil)
	if err != nil && opts.Mode != ModeSoftware {
		log.Printf("[media] thumbnail %s: %s hwaccel failed, falling back to software: %v", in, opts.Mode, err)
		swOpts := opts
		swOpts.Mode = ModeSoftware
		err = runFFmpeg(ctx, thumbArgs(in, tmp, swOpts), nil)
	}
	if err != nil {
		return fmt.Errorf("media: thumbnail %s: %w", in, err)
	}
	return os.Rename(tmp, out)
}

func thumbArgs(in, out string, opts ThumbOptions) []string {
	width := opts.Width
	if width <= 0 {
		width = defaultThumbWidth
	}
	at := opts.AtSec
	if at < 0 {
		at = 0
	}
	ss := strconv.FormatFloat(at, 'f', 2, 64)

	switch opts.Mode {
	case ModeQSV:
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-init_hw_device", "qsv=hw:" + opts.Device,
			"-hwaccel_device", "hw",
			"-filter_hw_device", "hw",
			"-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
			"-ss", ss, "-i", in,
			"-vframes", "1",
			"-vf", fmt.Sprintf("scale_qsv=w=%d:h=-1,hwdownload,format=nv12", width),
			"-f", "mjpeg", "-q:v", "4", out,
		}
	case ModeVAAPI:
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-init_hw_device", "vaapi=hw:" + opts.Device,
			"-hwaccel_device", "hw",
			"-filter_hw_device", "hw",
			"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi",
			"-ss", ss, "-i", in,
			"-vframes", "1",
			"-vf", fmt.Sprintf("scale_vaapi=w=%d:h=-1,hwdownload,format=nv12", width),
			"-f", "mjpeg", "-q:v", "4", out,
		}
	default:
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-ss", ss, "-i", in,
			"-vframes", "1",
			"-vf", fmt.Sprintf("scale=w=%d:h=-1", width),
			"-f", "mjpeg", "-q:v", "4", out,
		}
	}
}

// SpriteOptions configures a hover-scrub sprite sheet + WebVTT cue file.
type SpriteOptions struct {
	Mode             Mode
	Device           string
	Cols             int     // <=0 uses defaultSpriteCols
	Rows             int     // <=0 uses defaultSpriteRows
	TileWidth        int     // <=0 uses defaultTileWidth
	TileHeight       int     // <=0 computed from TileWidth assuming defaultTileAspect
	SegmentDurationS float64 // <=0 uses defaultSegmentSecs
}

// Sprite generates a single JPEG tile grid (Cols x Rows evenly-spaced
// frames from in) plus a WebVTT file mapping playback time ranges to
// pixel regions of that grid, for scrub-on-hover previews. Same
// hw-failure-then-software-retry behavior as Thumbnail.
func Sprite(ctx context.Context, in, outJPG, outVTT string, opts SpriteOptions) error {
	cols := opts.Cols
	if cols <= 0 {
		cols = defaultSpriteCols
	}
	rows := opts.Rows
	if rows <= 0 {
		rows = defaultSpriteRows
	}
	tileWidth := opts.TileWidth
	if tileWidth <= 0 {
		tileWidth = defaultTileWidth
	}
	tileHeight := opts.TileHeight
	if tileHeight <= 0 {
		tileHeight = int(float64(tileWidth) / defaultTileAspect)
	}
	duration := opts.SegmentDurationS
	if duration <= 0 {
		duration = defaultSegmentSecs
	}
	n := cols * rows
	interval := duration / float64(n)
	if interval <= 0 {
		interval = 1
	}

	if err := os.MkdirAll(filepath.Dir(outJPG), 0o755); err != nil {
		return fmt.Errorf("media: sprite mkdir: %w", err)
	}
	tmp, err := tempOutputPath(filepath.Dir(outJPG), ".sprite-*.jpg")
	if err != nil {
		return fmt.Errorf("media: sprite temp file: %w", err)
	}
	defer os.Remove(tmp)

	err = generateSpriteFrame(ctx, in, tmp, opts, cols, rows, tileWidth, tileHeight, interval, n)
	if err != nil && opts.Mode != ModeSoftware {
		log.Printf("[media] sprite %s: %s hwaccel failed, falling back to software: %v", in, opts.Mode, err)
		swOpts := opts
		swOpts.Mode = ModeSoftware
		err = generateSpriteFrame(ctx, in, tmp, swOpts, cols, rows, tileWidth, tileHeight, interval, n)
	}
	if err != nil {
		return fmt.Errorf("media: sprite %s: %w", in, err)
	}
	if err := os.Rename(tmp, outJPG); err != nil {
		return fmt.Errorf("media: sprite %s: rename: %w", in, err)
	}

	// The VTT cue file references the sprite by the basename the browser
	// actually fetches it under (GET /api/segments/{id}/sprite.jpg), not
	// outJPG's on-disk cache filename (which is "{segment_id}.jpg" under a
	// shard directory) — those diverge, so hardcoding the served name here
	// keeps cues valid regardless of cache layout.
	vtt := SpriteVTT(servedSpriteBasename, cols, rows, tileWidth, tileHeight, interval, n)
	if err := os.WriteFile(outVTT, []byte(vtt), 0o644); err != nil {
		return fmt.Errorf("media: sprite %s: write vtt: %w", in, err)
	}
	return nil
}

// spriteArgs builds the keyframe-sampling ffmpeg invocation for the tile
// mosaic: -skip_frame nokey at the demuxer/decoder so only I-frames reach
// the filtergraph, plus a select expression that time-distributes which
// of those I-frames land in the mosaic. Naively selecting eq(pict_type\,I)
// alone and letting the tile filter consume the first cols*rows matches
// only covers the leading part of a segment with many keyframes, while
// the VTT cue file (built from the same uniform interval, see Sprite)
// maps tiles across the *whole* duration — so the select expression also
// requires each pick be at least interval seconds after the previous one
// (gte(t-prev_selected_t\,interval)), keeping the sampled frame times
// aligned with the VTT's assumed i*interval cue boundaries (within one
// GOP's slop, same precision the old fps-based sampling had). select only
// inspects frame metadata (pict_type, t), not pixel data, so it's placed
// immediately after decode — before scale_qsv/scale_vaapi — on the
// QSV/VAAPI paths too: it works the same on a hw surface as a sw frame,
// and dropping frames there means the costlier hwdownload/scale/tile
// stages only ever see the frames actually going into the mosaic. See
// generateSpriteFrame for the seek-extract fallback used when a file
// doesn't have enough keyframes for cols*rows tiles.
func spriteArgs(in, out string, opts SpriteOptions, cols, rows, tileWidth int, interval float64) []string {
	tileSpec := fmt.Sprintf("%dx%d", cols, rows)
	// isnan(prev_selected_t) forces the very first candidate I-frame to be
	// selected: ffmpeg's select filter reports prev_selected_t as NaN
	// until something has been selected, and gte() with a NaN operand is
	// always false — without the isnan(...) term here, select would never
	// pick anything and the tile filter would receive zero frames.
	sel := fmt.Sprintf(`select='eq(pict_type\,I)*(isnan(prev_selected_t)+gte(t-prev_selected_t\,%s))'`, formatSeconds(interval))

	switch opts.Mode {
	case ModeQSV:
		vf := fmt.Sprintf(`%s,scale_qsv=w=%d:h=-1,hwdownload,format=nv12,tile=%s`, sel, tileWidth, tileSpec)
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-init_hw_device", "qsv=hw:" + opts.Device,
			"-hwaccel_device", "hw",
			"-filter_hw_device", "hw",
			"-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
			"-skip_frame", "nokey",
			"-i", in,
			"-vf", vf,
			"-frames:v", "1", "-vsync", "0", "-f", "mjpeg", "-q:v", "4", out,
		}
	case ModeVAAPI:
		vf := fmt.Sprintf(`%s,scale_vaapi=w=%d:h=-1,hwdownload,format=nv12,tile=%s`, sel, tileWidth, tileSpec)
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-init_hw_device", "vaapi=hw:" + opts.Device,
			"-hwaccel_device", "hw",
			"-filter_hw_device", "hw",
			"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi",
			"-skip_frame", "nokey",
			"-i", in,
			"-vf", vf,
			"-frames:v", "1", "-vsync", "0", "-f", "mjpeg", "-q:v", "4", out,
		}
	default:
		vf := fmt.Sprintf(`%s,scale=w=%d:h=-1,tile=%s`, sel, tileWidth, tileSpec)
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-skip_frame", "nokey",
			"-i", in,
			"-vf", vf,
			"-frames:v", "1", "-vsync", "0", "-f", "mjpeg", "-q:v", "4", out,
		}
	}
}

// generateSpriteFrame produces the single sprite-sheet JPEG at out. WP2
// round 3: seek-based sampling (spriteSeekArgs) is the primary strategy
// for every mode now — it beat both the round-2 keyframe-select approach
// and a naive N-separate-process seek extraction in benchmarking (see
// wp2-report.md) because a single ffmpeg process opens the source n times
// with -noaccurate_seek, each seek landing on the nearest keyframe without
// decoding forward, rather than decoding every keyframe in the file (the
// round-2 regression) or every frame (the original fps-based approach).
// The round-2 select-based spriteArgs is kept only as the fallback for a
// source where seeking itself fails (e.g. an unseekable/corrupt stream).
func generateSpriteFrame(ctx context.Context, in, out string, opts SpriteOptions, cols, rows, tileWidth, tileHeight int, interval float64, n int) error {
	err := runFFmpeg(ctx, spriteSeekArgs(in, out, opts, cols, rows, tileWidth, tileHeight, interval, n), nil)
	if err != nil {
		log.Printf("[media] sprite %s: seek-based sampling failed, falling back to keyframe select: %v", in, err)
		err = runFFmpeg(ctx, spriteArgs(in, out, opts, cols, rows, tileWidth, interval), nil)
	}
	return err
}

// spriteSeekArgs builds one ffmpeg invocation that opens in n times (once
// per tile), each seeking to that tile's midpoint T_i = (i+0.5)*interval —
// the midpoint of the same [i*interval, (i+1)*interval) span the VTT cue
// for that tile covers (see SpriteVTT/Sprite), so the frame actually
// sampled matches what the cue claims it represents. "-ss" ahead of "-i"
// is ffmpeg's fast/coarse input seeking; "-noaccurate_seek" additionally
// skips the decode-forward ffmpeg does by default to land exactly on the
// requested timestamp, landing on the nearest keyframe instead — so each
// tile costs only a seek plus a handful of decodes, independent of how
// far into the segment it is or how many keyframes came before it.
// "-an -sn" skip audio/subtitle demuxing, a small fixed cost otherwise
// paid n times over. The n decoded/scaled frames are combined into one
// mosaic via xstack (each input is its own stream, so the tile filter —
// which operates on one stream's frame sequence — doesn't apply here).
func spriteSeekArgs(in, out string, opts SpriteOptions, cols, rows, tileWidth, tileHeight int, interval float64, n int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	switch opts.Mode {
	case ModeQSV:
		args = append(args, "-init_hw_device", "qsv=hw:"+opts.Device)
	case ModeVAAPI:
		args = append(args, "-init_hw_device", "vaapi=hw:"+opts.Device)
	}

	filters := make([]string, 0, n)
	labels := make([]string, 0, n)
	for i := 0; i < n; i++ {
		t := (float64(i) + 0.5) * interval
		ss := strconv.FormatFloat(t, 'f', 2, 64)
		switch opts.Mode {
		case ModeQSV:
			args = append(args,
				"-hwaccel_device", "hw", "-filter_hw_device", "hw",
				"-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
				"-an", "-sn", "-noaccurate_seek", "-ss", ss, "-i", in,
			)
			filters = append(filters, fmt.Sprintf("[%d:v]scale_qsv=w=%d:h=%d,hwdownload,format=nv12[v%d]", i, tileWidth, tileHeight, i))
		case ModeVAAPI:
			args = append(args,
				"-hwaccel_device", "hw", "-filter_hw_device", "hw",
				"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi",
				"-an", "-sn", "-noaccurate_seek", "-ss", ss, "-i", in,
			)
			filters = append(filters, fmt.Sprintf("[%d:v]scale_vaapi=w=%d:h=%d,hwdownload,format=nv12[v%d]", i, tileWidth, tileHeight, i))
		default:
			args = append(args, "-an", "-sn", "-noaccurate_seek", "-ss", ss, "-i", in)
			filters = append(filters, fmt.Sprintf("[%d:v]scale=w=%d:h=%d[v%d]", i, tileWidth, tileHeight, i))
		}
		labels = append(labels, fmt.Sprintf("[v%d]", i))
	}

	filterComplex := strings.Join(filters, ";") + ";" + strings.Join(labels, "") +
		fmt.Sprintf("xstack=inputs=%d:layout=%s[out]", n, xstackLayout(cols, rows, tileWidth, tileHeight))

	args = append(args,
		"-filter_complex", filterComplex,
		"-map", "[out]",
		"-frames:v", "1", "-f", "mjpeg", "-q:v", "4", out,
	)
	return args
}

// xstackLayout builds the "x_y|x_y|..." offset list xstack needs to place
// cols*rows tileWidth x tileHeight frames into a grid, row-major (matching
// SpriteVTT's row/col indexing).
func xstackLayout(cols, rows, tileWidth, tileHeight int) string {
	parts := make([]string, 0, cols*rows)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			parts = append(parts, fmt.Sprintf("%d_%d", c*tileWidth, r*tileHeight))
		}
	}
	return strings.Join(parts, "|")
}

// tempOutputPath reserves a unique temp file (via os.CreateTemp, matching
// pattern e.g. ".thumb-*.jpg") inside dir and returns its path, closed and
// ready for ffmpeg to overwrite (with -y) and the caller to later rename
// into place. Using CreateTemp instead of a fixed "out+.tmp" name is what
// makes concurrent generation of the same output (e.g. an on-demand HTTP
// request racing the background thumbs job) safe: each run writes its own
// file and only the winner's rename is observed.
func tempOutputPath(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return name, nil
}

// SpriteVTT builds the WebVTT thumbnails cue file for a sprite sheet
// spriteFile of cols x rows tiles (each tileWidth x tileHeight px),
// covering totalCues consecutive equal-length intervals starting at 0.
func SpriteVTT(spriteFile string, cols, rows, tileWidth, tileHeight int, interval float64, totalCues int) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for i := 0; i < totalCues; i++ {
		row := i / cols
		col := i % cols
		start := float64(i) * interval
		end := float64(i+1) * interval
		fmt.Fprintf(&b, "%s --> %s\n", vttTimestamp(start), vttTimestamp(end))
		fmt.Fprintf(&b, "%s#xywh=%d,%d,%d,%d\n\n", spriteFile, col*tileWidth, row*tileHeight, tileWidth, tileHeight)
	}
	return b.String()
}

// vttTimestamp formats seconds as WebVTT's HH:MM:SS.mmm.
func vttTimestamp(totalSeconds float64) string {
	if totalSeconds < 0 {
		totalSeconds = 0
	}
	ms := int64(totalSeconds*1000 + 0.5)
	hh := ms / 3600000
	ms -= hh * 3600000
	mm := ms / 60000
	ms -= mm * 60000
	ss := ms / 1000
	ms -= ss * 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hh, mm, ss, ms)
}
