package media

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountBoxes(t *testing.T) {
	frames := []Frame{
		{AtS: 0, Boxes: []Box{{}, {}}},
		{AtS: 1, Boxes: []Box{{}}},
	}
	if got := CountBoxes(frames); got != 3 {
		t.Errorf("CountBoxes = %d, want 3", got)
	}
}

func TestBuildDrawFiltersGeneratesDrawboxPerBox(t *testing.T) {
	frames := []Frame{
		{AtS: 1, Boxes: []Box{{X1: 0.1, Y1: 0.2, X2: 0.3, Y2: 0.4, Label: "car", Color: ColorAmber}}},
	}
	script := BuildDrawFilters(frames, 1000, 500, "/tmp/font.ttf")
	for _, want := range []string{
		"drawbox=x=100:y=100:w=199:h=100",
		"color=orange",
		"enable='between(t\\,1\\,1.5)'",
		"drawtext=fontfile='/tmp/font.ttf':text='car'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("BuildDrawFilters missing %q: %s", want, script)
		}
	}
}

func TestBuildDrawFiltersSkipsDegenerateBoxes(t *testing.T) {
	frames := []Frame{{AtS: 0, Boxes: []Box{{X1: 0.5, Y1: 0.5, X2: 0.5, Y2: 0.9}}}}
	script := BuildDrawFilters(frames, 100, 100, "/tmp/font.ttf")
	if script != "" {
		t.Errorf("BuildDrawFilters(zero-width box) = %q, want empty", script)
	}
}

func TestEscapeFFTextEscapesSpecialChars(t *testing.T) {
	got := escapeFFText(`ab'c:d\e,f`)
	want := `ab\'c\:d\\e\,f`
	if got != want {
		t.Errorf("escapeFFText = %q, want %q", got, want)
	}
}

func TestWriteDefaultFontWritesReadableTTF(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sub/font.ttf"
	if err := WriteDefaultFont(path); err != nil {
		t.Fatalf("WriteDefaultFont: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written font: %v", err)
	}
	if len(data) == 0 {
		t.Errorf("WriteDefaultFont wrote an empty file")
	}
}

func TestHoldDurationCapsToNextFrameGap(t *testing.T) {
	frames := []Frame{{AtS: 0}, {AtS: 0.2}, {AtS: 5}}
	if d := holdDuration(frames, 0); d != 0.2 {
		t.Errorf("holdDuration(short gap) = %v, want 0.2", d)
	}
	if d := holdDuration(frames, 1); d != defaultHoldS {
		t.Errorf("holdDuration(long gap) = %v, want default %v", d, defaultHoldS)
	}
	if d := holdDuration(frames, 2); d != defaultHoldS {
		t.Errorf("holdDuration(last frame) = %v, want default %v", d, defaultHoldS)
	}
}

// TestRenderOverlayFramesProducesExpectedPixels renders a single detection
// box covering the left half of a small frame and checks that the written
// PNG has an outline pixel where expected and stays fully transparent
// where no box was drawn.
func TestRenderOverlayFramesProducesExpectedPixels(t *testing.T) {
	dir := t.TempDir()
	frames := []Frame{
		{AtS: 0, Boxes: []Box{{X1: 0, Y1: 0, X2: 0.5, Y2: 1, Color: ColorRed}}},
	}
	n, err := RenderOverlayFrames(frames, 20, 20, 0.05, dir)
	if err != nil {
		t.Fatalf("RenderOverlayFrames: %v", err)
	}
	if n < 1 {
		t.Fatalf("RenderOverlayFrames wrote %d frames, want >= 1", n)
	}

	f, err := os.Open(filepath.Join(dir, "overlay_00000.png"))
	if err != nil {
		t.Fatalf("open overlay_00000.png: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode overlay_00000.png: %v", err)
	}

	// The box outline is drawn at x0=0 (box left edge), 2px thick, so pixel
	// (0,5) should be the red outline color.
	r, g, b, a := img.At(0, 5).RGBA()
	want := ColorRed.rgba()
	if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(b>>8) != want.B || a == 0 {
		t.Errorf("overlay pixel at box edge (0,5) = rgba(%d,%d,%d,%d), want red outline", r>>8, g>>8, b>>8, a)
	}

	// Far outside the box (bottom-right corner) should be untouched —
	// fully transparent.
	_, _, _, a2 := img.At(19, 19).RGBA()
	if a2 != 0 {
		t.Errorf("overlay pixel outside any box (19,19) alpha = %d, want 0 (transparent)", a2)
	}
}

func TestDrawBoxOutlineDrawsOnlyBorder(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 30, 30))
	drawBoxOutline(img, 5, 5, 25, 25, color.RGBA{R: 255, A: 255})
	// Interior (well inside the border) must remain untouched.
	if _, _, _, a := img.At(15, 15).RGBA(); a != 0 {
		t.Errorf("box interior should be transparent (outline only), got alpha %d", a)
	}
	// Top border pixel must be drawn.
	if _, _, _, a := img.At(15, 5).RGBA(); a == 0 {
		t.Errorf("box top border pixel should be drawn, got alpha 0")
	}
}
