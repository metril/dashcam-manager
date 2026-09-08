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
	tmp := out + ".tmp"
	defer os.Remove(tmp)

	err := runFFmpeg(ctx, thumbArgs(in, tmp, opts), nil)
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
			"-filter_hw_device", "hw",
			"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi",
			"-ss", ss, "-i", in,
			"-vframes", "1",
			"-vf", fmt.Sprintf("scale_vaapi=w=%d:h=-1,format=nv12,hwdownload,format=nv12", width),
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
	tmp := outJPG + ".tmp"
	defer os.Remove(tmp)

	err := runFFmpeg(ctx, spriteArgs(in, tmp, opts, cols, rows, tileWidth, interval), nil)
	if err != nil && opts.Mode != ModeSoftware {
		log.Printf("[media] sprite %s: %s hwaccel failed, falling back to software: %v", in, opts.Mode, err)
		swOpts := opts
		swOpts.Mode = ModeSoftware
		err = runFFmpeg(ctx, spriteArgs(in, tmp, swOpts, cols, rows, tileWidth, interval), nil)
	}
	if err != nil {
		return fmt.Errorf("media: sprite %s: %w", in, err)
	}
	if err := os.Rename(tmp, outJPG); err != nil {
		return fmt.Errorf("media: sprite %s: rename: %w", in, err)
	}

	vtt := SpriteVTT(filepath.Base(outJPG), cols, rows, tileWidth, tileHeight, interval, n)
	if err := os.WriteFile(outVTT, []byte(vtt), 0o644); err != nil {
		return fmt.Errorf("media: sprite %s: write vtt: %w", in, err)
	}
	return nil
}

func spriteArgs(in, out string, opts SpriteOptions, cols, rows, tileWidth int, interval float64) []string {
	fps := 1.0 / interval
	tileSpec := fmt.Sprintf("%dx%d", cols, rows)

	switch opts.Mode {
	case ModeQSV:
		vf := fmt.Sprintf("fps=%s,scale_qsv=w=%d:h=-1,hwdownload,format=nv12,tile=%s", formatFPS(fps), tileWidth, tileSpec)
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-init_hw_device", "qsv=hw:" + opts.Device,
			"-filter_hw_device", "hw",
			"-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
			"-i", in,
			"-vf", vf,
			"-frames:v", "1", "-f", "mjpeg", "-q:v", "4", out,
		}
	case ModeVAAPI:
		vf := fmt.Sprintf("fps=%s,scale_vaapi=w=%d:h=-1,format=nv12,hwdownload,format=nv12,tile=%s", formatFPS(fps), tileWidth, tileSpec)
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-init_hw_device", "vaapi=hw:" + opts.Device,
			"-filter_hw_device", "hw",
			"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi",
			"-i", in,
			"-vf", vf,
			"-frames:v", "1", "-f", "mjpeg", "-q:v", "4", out,
		}
	default:
		vf := fmt.Sprintf("fps=%s,scale=w=%d:h=-1,tile=%s", formatFPS(fps), tileWidth, tileSpec)
		return []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-i", in,
			"-vf", vf,
			"-frames:v", "1", "-f", "mjpeg", "-q:v", "4", out,
		}
	}
}

func formatFPS(fps float64) string {
	return strconv.FormatFloat(fps, 'f', -1, 64)
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
