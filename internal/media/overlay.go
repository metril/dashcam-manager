// overlay.go builds the M11 detection burn-in: either an ffmpeg
// drawbox/drawtext filter script (small box counts) or a rendered
// transparent PNG overlay sequence (large box counts), from a segment's
// already-normalized (0..1) detection boxes.
package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// MaxScriptBoxes is the box-count threshold below which BuildDrawFilters'
// generated filter_complex_script is used for burn-in; at or above it,
// RenderOverlayFrames' PNG-sequence path is used instead (a filter chain
// with thousands of drawbox/drawtext entries becomes slow for ffmpeg to
// parse and execute).
const MaxScriptBoxes = 2000

// overlayFPS is the frame rate rendered PNG overlays are generated at —
// detections are sampled well below source video frame rate already
// (roughly per analyzed frame, not per video frame), so 10fps is enough
// resolution for a burn-in that tracks bounding boxes without the file
// count exploding for a long clip.
const overlayFPS = 10.0

// Box is one detection's bounding box, normalized to 0..1 within the
// video frame (matching internal/scan.NormBox), plus the label text to
// draw next to it.
type Box struct {
	X1, Y1, X2, Y2 float64
	Label          string
	Color          BoxColor
}

// BoxColor selects a burn-in box's outline/label color, keeping this
// package decoupled from the detection class taxonomy (callers map class
// names to a BoxColor before building Box values).
type BoxColor int

const (
	ColorAmber BoxColor = iota
	ColorRed
	ColorCyan
)

func (c BoxColor) rgba() color.RGBA {
	switch c {
	case ColorRed:
		return color.RGBA{R: 239, G: 68, B: 68, A: 255}
	case ColorCyan:
		return color.RGBA{R: 34, G: 211, B: 238, A: 255}
	default:
		return color.RGBA{R: 245, G: 158, B: 11, A: 255}
	}
}

func (c BoxColor) ffmpegName() string {
	switch c {
	case ColorRed:
		return "red"
	case ColorCyan:
		return "cyan"
	default:
		return "orange"
	}
}

// Frame is one instant's set of detection boxes, at AtS seconds relative
// to the start of the trimmed export clip (i.e. already shifted so 0 is
// the clip's first frame, not the source segment's).
type Frame struct {
	AtS   float64
	Boxes []Box
}

// CountBoxes totals the boxes across every frame, for MaxScriptBoxes
// comparisons.
func CountBoxes(frames []Frame) int {
	n := 0
	for _, f := range frames {
		n += len(f.Boxes)
	}
	return n
}

// holdDuration returns how long frame i's boxes should stay drawn: until
// the next frame (capped at defaultHoldS, so a long gap between analyzed
// frames doesn't leave a stale box on screen), or defaultHoldS for the
// last frame.
const defaultHoldS = 0.5

func holdDuration(frames []Frame, i int) float64 {
	if i+1 < len(frames) {
		gap := frames[i+1].AtS - frames[i].AtS
		if gap > 0 && gap < defaultHoldS {
			return gap
		}
	}
	return defaultHoldS
}

// escapeFFText escapes a text value for use inside an ffmpeg filter
// option (e.g. drawtext's text=): backslash and the filter-graph special
// characters ':', ',' and single-quote must be backslash-escaped, per
// ffmpeg's filtergraph syntax documentation (an unescaped comma would
// otherwise be read as ending this filter and starting the next one in
// the chain). Applied before the value is itself wrapped in single quotes
// by the caller.
func escapeFFText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '\'', ':', ',':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapeFFPath escapes a filesystem path for use as an ffmpeg filter
// option value (e.g. drawtext's fontfile=): backslash, single-quote and
// colon are escaped the same way escapeFFText does, since Windows-style
// paths aside, a path can still contain any of these on Linux.
func escapeFFPath(s string) string { return escapeFFText(s) }

// BuildDrawFilters renders frames as a chain of ffmpeg drawbox+drawtext
// filters (suitable for appending, comma-separated, into a
// filter_complex_script fragment — see stitch.go's ReencodeInput.Overlay),
// scaled to outW x outH pixels. Each box is only visible for its frame's
// hold window via enable='between(t,start,end)', so overlapping boxes
// from different frames don't all render at once. fontFile is a path to a
// TTF file on disk (see WriteDefaultFont) — ffmpeg's drawtext filter
// requires an explicit fontfile (or libfontconfig support, which isn't
// guaranteed present in every ffmpeg build) to find a font at all.
func BuildDrawFilters(frames []Frame, outW, outH int, fontFile string) string {
	var parts []string
	for i, f := range frames {
		end := f.AtS + holdDuration(frames, i)
		for _, b := range f.Boxes {
			x := int(b.X1 * float64(outW))
			y := int(b.Y1 * float64(outH))
			w := int((b.X2 - b.X1) * float64(outW))
			h := int((b.Y2 - b.Y1) * float64(outH))
			if w <= 0 || h <= 0 {
				continue
			}
			parts = append(parts, fmt.Sprintf(
				"drawbox=x=%d:y=%d:w=%d:h=%d:color=%s:t=2:enable='between(t\\,%s\\,%s)'",
				x, y, w, h, b.Color.ffmpegName(), formatSeconds(f.AtS), formatSeconds(end)))
			if b.Label != "" && fontFile != "" {
				ty := y - 14
				if ty < 0 {
					ty = y + h + 2
				}
				parts = append(parts, fmt.Sprintf(
					"drawtext=fontfile='%s':text='%s':x=%d:y=%d:fontsize=13:fontcolor=white:box=1:boxcolor=black@0.55:boxborderw=2:enable='between(t\\,%s\\,%s)'",
					escapeFFPath(fontFile), escapeFFText(b.Label), x, ty, formatSeconds(f.AtS), formatSeconds(end)))
			}
		}
	}
	return strings.Join(parts, ",")
}

// WriteDefaultFont writes the bundled Go Regular TTF (the same font
// RenderOverlayFrames uses for PNG-overlay labels) to path, for a caller
// that needs an on-disk fontfile= argument for BuildDrawFilters — the job
// handler writes this once per job into its temp dir.
func WriteDefaultFont(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("media: write default font: %w", err)
	}
	if err := os.WriteFile(path, goregular.TTF, 0o644); err != nil {
		return fmt.Errorf("media: write default font: %w", err)
	}
	return nil
}

// ---- PNG overlay sequence (large box counts) ----

var (
	overlayFaceOnce sync.Once
	overlayFace     font.Face
	overlayFaceErr  error
)

// loadOverlayFace parses the bundled Go Regular TTF into a font.Face
// exactly once (font parsing/hinting setup isn't free, and RenderOverlayFrames
// may be called many times across a process's lifetime) — sync.Once makes
// this safe under the concurrent export jobs the queue can run.
func loadOverlayFace() (font.Face, error) {
	overlayFaceOnce.Do(func() {
		f, err := opentype.Parse(goregular.TTF)
		if err != nil {
			overlayFaceErr = fmt.Errorf("media: parse overlay font: %w", err)
			return
		}
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 13, DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			overlayFaceErr = fmt.Errorf("media: overlay font face: %w", err)
			return
		}
		overlayFace = face
	})
	return overlayFace, overlayFaceErr
}

// drawBoxOutline draws a 2px rectangle outline for b onto img.
func drawBoxOutline(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	if x1 <= x0 || y1 <= y0 {
		return
	}
	const t = 2
	fill := func(rx0, ry0, rx1, ry1 int) {
		r := image.Rect(rx0, ry0, rx1, ry1).Intersect(img.Bounds())
		if r.Empty() {
			return
		}
		draw.Draw(img, r, &image.Uniform{C: c}, image.Point{}, draw.Src)
	}
	fill(x0, y0, x1, y0+t)
	fill(x0, y1-t, x1, y1)
	fill(x0, y0, x0+t, y1)
	fill(x1-t, y0, x1, y1)
}

// drawLabel draws text with a translucent background box at (x,y) (top-left
// of the label), clipped to img's bounds.
func drawLabel(img *image.RGBA, face font.Face, text string, x, y int) {
	if text == "" {
		return
	}
	metrics := face.Metrics()
	ascent := metrics.Ascent.Ceil()
	height := (metrics.Ascent + metrics.Descent).Ceil() + 4
	width := font.MeasureString(face, text).Ceil() + 6

	bg := image.Rect(x, y, x+width, y+height).Intersect(img.Bounds())
	if !bg.Empty() {
		draw.Draw(img, bg, &image.Uniform{C: color.RGBA{A: 160}}, image.Point{}, draw.Over)
	}

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.White),
		Face: face,
		Dot:  fixed.P(x+3, y+ascent+2),
	}
	d.DrawString(text)
}

// RenderOverlayFrames renders a transparent RGBA PNG for every 1/overlayFPS
// interval spanning [0, clipDurationS), holding each detection Frame's
// boxes on screen per holdDuration, and writes them into dir as
// overlay_00000.png, overlay_00001.png, ... . Returns the count written.
func RenderOverlayFrames(frames []Frame, outW, outH int, clipDurationS float64, dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("media: overlay dir: %w", err)
	}
	face, err := loadOverlayFace()
	if err != nil {
		return 0, err
	}

	sorted := make([]Frame, len(frames))
	copy(sorted, frames)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].AtS < sorted[j].AtS })

	n := int(clipDurationS*overlayFPS) + 1
	if n < 1 {
		n = 1
	}

	for i := 0; i < n; i++ {
		t := float64(i) / overlayFPS
		img := image.NewRGBA(image.Rect(0, 0, outW, outH))
		// transparent by default (image.NewRGBA zero-values to all-zero
		// pixels, i.e. fully transparent black)

		for fi, f := range sorted {
			end := f.AtS + holdDuration(sorted, fi)
			if t < f.AtS || t >= end {
				continue
			}
			for _, b := range f.Boxes {
				x0 := int(b.X1 * float64(outW))
				y0 := int(b.Y1 * float64(outH))
				x1 := int(b.X2 * float64(outW))
				y1 := int(b.Y2 * float64(outH))
				drawBoxOutline(img, x0, y0, x1, y1, b.Color.rgba())
				if b.Label != "" {
					ly := y0 - 18
					if ly < 0 {
						ly = y1 + 2
					}
					drawLabel(img, face, b.Label, x0, ly)
				}
			}
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return i, fmt.Errorf("media: encode overlay frame %d: %w", i, err)
		}
		name := filepath.Join(dir, fmt.Sprintf("overlay_%05d.png", i))
		if err := os.WriteFile(name, buf.Bytes(), 0o644); err != nil {
			return i, fmt.Errorf("media: write overlay frame %d: %w", i, err)
		}
	}
	return n, nil
}

// OverlayInputArgs returns the extra ffmpeg input args to read a rendered
// PNG sequence (from RenderOverlayFrames) as a video input at overlayFPS.
func OverlayInputArgs(dir string) []string {
	return []string{"-framerate", strconv.FormatFloat(overlayFPS, 'f', -1, 64), "-i", filepath.Join(dir, "overlay_%05d.png")}
}

// OverlayFilterFragment returns the `overlay=...` filter fragment to
// composite the PNG-sequence input (graph index overlayInputIdx) on top of
// the base video pad baseLabel (without brackets), ending in outLabel
// (without brackets). eof_action=pass keeps the base video going once the
// (shorter, if clipDurationS was rounded down) overlay stream ends.
func OverlayFilterFragment(baseLabel string, overlayInputIdx int, outLabel string) string {
	return fmt.Sprintf("[%s][%d:v]overlay=eof_action=pass[%s]", baseLabel, overlayInputIdx, outLabel)
}
