package media

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestThumbArgsSoftware(t *testing.T) {
	args := thumbArgs("in.mp4", "out.jpg", ThumbOptions{Mode: ModeSoftware, AtSec: 5, Width: 480})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-ss 5.00", "-i in.mp4", "scale=w=480:h=-1", "out.jpg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("thumbArgs software = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "hwaccel") {
		t.Errorf("software args should not mention hwaccel: %q", joined)
	}
}

func TestThumbArgsQSV(t *testing.T) {
	args := thumbArgs("in.mp4", "out.jpg", ThumbOptions{Mode: ModeQSV, Device: "/dev/dri/renderD128", AtSec: 2.5, Width: 320})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"-hwaccel qsv",
		"-hwaccel_output_format qsv",
		"scale_qsv=w=320:h=-1,hwdownload,format=nv12",
		"-ss 2.50",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("thumbArgs qsv = %q, missing %q", joined, want)
		}
	}
}

func TestThumbArgsVAAPI(t *testing.T) {
	args := thumbArgs("in.mp4", "out.jpg", ThumbOptions{Mode: ModeVAAPI, Device: "/dev/dri/renderD128"})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device vaapi=hw:/dev/dri/renderD128",
		"-hwaccel vaapi",
		// No format=nv12 filter between scale_vaapi and hwdownload: the
		// VAAPI frame is already in the right surface format coming out
		// of scale_vaapi, and inserting an extra software-side format
		// conversion before hwdownload is both redundant and what broke
		// the pipeline on the HD 630 (M8 review finding #4).
		"scale_vaapi=w=480:h=-1,hwdownload,format=nv12",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("thumbArgs vaapi = %q, missing %q", joined, want)
		}
	}
}

// TestThumbArgsHwaccelDevice covers M8 review finding #3: both QSV and
// VAAPI must pass -hwaccel_device hw (in addition to -init_hw_device and
// -filter_hw_device) or ffmpeg's hwaccel frames context isn't bound to the
// device the qsv=hw:/vaapi=hw: handle just initialized.
func TestThumbArgsHwaccelDevice(t *testing.T) {
	for _, mode := range []Mode{ModeQSV, ModeVAAPI} {
		args := thumbArgs("in.mp4", "out.jpg", ThumbOptions{Mode: mode, Device: "/dev/dri/renderD128"})
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-hwaccel_device hw") {
			t.Errorf("thumbArgs %s missing -hwaccel_device hw: %q", mode, joined)
		}
	}
}

func TestThumbArgsNegativeAtSecClampedToZero(t *testing.T) {
	args := thumbArgs("in.mp4", "out.jpg", ThumbOptions{Mode: ModeSoftware, AtSec: -3})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-ss 0.00") {
		t.Errorf("expected -ss 0.00 for negative AtSec, got %q", joined)
	}
}

func TestSpriteArgsSoftwareTileAndFPS(t *testing.T) {
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeSoftware}, 5, 2, 320, 3)
	joined := strings.Join(args, " ")
	// fps is (1/interval)*spriteFPSMargin, not the exact 1/3 rate: a small
	// margin over-samples so the tile filter never comes up one frame
	// short of cols*rows and leaves a blank tile (see spriteFPSMargin).
	for _, want := range []string{"tile=5x2", "fps=0.35", "scale=w=320:h=-1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteArgs software = %q, missing %q", joined, want)
		}
	}
}

func TestSpriteArgsQSV(t *testing.T) {
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeQSV, Device: "/dev/dri/renderD128"}, 5, 2, 320, 3)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"scale_qsv=w=320:h=-1,hwdownload,format=nv12,tile=5x2",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteArgs qsv = %q, missing %q", joined, want)
		}
	}
}

func TestSpriteArgsVAAPIChainNoDoubleFormat(t *testing.T) {
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeVAAPI, Device: "/dev/dri/renderD128"}, 5, 2, 320, 3)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hwaccel_device hw") {
		t.Errorf("spriteArgs vaapi missing -hwaccel_device hw: %q", joined)
	}
	if !strings.Contains(joined, "scale_vaapi=w=320:h=-1,hwdownload,format=nv12,tile=5x2") {
		t.Errorf("spriteArgs vaapi chain wrong (want scale_vaapi straight into hwdownload,format=nv12): %q", joined)
	}
}

// TestSpriteArgsFPSMargin covers the fps-rounding fix (M8 review, minor):
// the sampling rate must be strictly higher than the exact
// n_tiles/duration rate so the tile filter is never one input frame short
// of cols*rows, which would leave a blank tile in the mosaic.
func TestSpriteArgsFPSMargin(t *testing.T) {
	const interval = 3.0
	exact := 1.0 / interval
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeSoftware}, 5, 2, 320, interval)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "fps="+formatFPS(exact)) {
		t.Errorf("spriteArgs fps should be inflated above the exact rate %v, got: %q", exact, joined)
	}
}

func TestTempOutputPathIsUniqueAndInDir(t *testing.T) {
	dir := t.TempDir()
	a, err := tempOutputPath(dir, ".thumb-*.jpg")
	if err != nil {
		t.Fatalf("tempOutputPath: %v", err)
	}
	b, err := tempOutputPath(dir, ".thumb-*.jpg")
	if err != nil {
		t.Fatalf("tempOutputPath: %v", err)
	}
	if a == b {
		t.Fatalf("tempOutputPath returned the same path twice: %q", a)
	}
	if filepath.Dir(a) != dir || filepath.Dir(b) != dir {
		t.Errorf("tempOutputPath escaped dir: a=%q b=%q dir=%q", a, b, dir)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("tempOutputPath %q not created: %v", p, err)
		}
	}
}

// TestSpriteVTTUsesServedBasenameNotCacheFilename covers M8 review finding
// #1: the cache stores a sprite sheet as "{segment_id}.jpg" (see
// SpritePaths), but the browser fetches it as
// /api/segments/{id}/sprite.jpg — so the cue file's #xywh URLs must
// reference "sprite.jpg", never the cache's numeric filename, or every
// hover-scrub tile 404s. Exercises the real Sprite() entry point (not just
// spriteArgs) with a fake ffmpeg script standing in for the real binary.
func TestSpriteVTTUsesServedBasenameNotCacheFilename(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	fakeFFmpeg := writeFakeFFmpeg(t, dir)
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	cacheDir := t.TempDir()
	jpg := filepath.Join(cacheDir, "sprites", "5", "5.jpg") // cache filename is "5.jpg", not "sprite.jpg"
	vtt := filepath.Join(cacheDir, "sprites", "5", "5.vtt")

	if err := Sprite(context.Background(), "in.mp4", jpg, vtt, SpriteOptions{Mode: ModeSoftware, SegmentDurationS: 30}); err != nil {
		t.Fatalf("Sprite: %v", err)
	}

	got, err := os.ReadFile(vtt)
	if err != nil {
		t.Fatalf("read vtt: %v", err)
	}
	if strings.Contains(string(got), "5.jpg#xywh") {
		t.Errorf("vtt references the cache filename %q instead of the served name: %s", "5.jpg", got)
	}
	if !strings.Contains(string(got), "sprite.jpg#xywh") {
		t.Errorf("vtt missing served basename sprite.jpg: %s", got)
	}
}

// writeFakeFFmpeg writes a shell script into dir that ignores its
// arguments and just creates an empty file at its last argument (the -y
// output path), standing in for a real ffmpeg binary in tests that need
// Thumbnail/Sprite to actually "succeed" without a real encoder present.
func writeFakeFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ffmpeg.sh")
	script := "#!/bin/sh\neval out=\\${$#}\ntouch \"$out\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

func TestSpriteVTTLayout(t *testing.T) {
	vtt := SpriteVTT("sprite.jpg", 5, 2, 320, 180, 3, 10)
	if !strings.HasPrefix(vtt, "WEBVTT\n\n") {
		t.Fatalf("vtt missing WEBVTT header: %q", vtt[:20])
	}
	// First cue: 0 -> 3s, tile (0,0).
	if !strings.Contains(vtt, "00:00:00.000 --> 00:00:03.000\nsprite.jpg#xywh=0,0,320,180") {
		t.Errorf("first cue wrong, got: %q", vtt)
	}
	// 6th cue (index 5) starts a new row: row=1, col=0 -> y=180.
	if !strings.Contains(vtt, "00:00:15.000 --> 00:00:18.000\nsprite.jpg#xywh=0,180,320,180") {
		t.Errorf("6th cue wrong, got: %q", vtt)
	}
	// Last cue: index 9 -> row=1,col=4 -> x=1280,y=180; start=27s end=30s.
	if !strings.Contains(vtt, "00:00:27.000 --> 00:00:30.000\nsprite.jpg#xywh=1280,180,320,180") {
		t.Errorf("last cue wrong, got: %q", vtt)
	}
	cueCount := strings.Count(vtt, "-->")
	if cueCount != 10 {
		t.Errorf("cue count = %d, want 10", cueCount)
	}
}

func TestVTTTimestamp(t *testing.T) {
	cases := map[float64]string{
		0:      "00:00:00.000",
		3.5:    "00:00:03.500",
		61:     "00:01:01.000",
		3661.2: "01:01:01.200",
	}
	for in, want := range cases {
		if got := vttTimestamp(in); got != want {
			t.Errorf("vttTimestamp(%v) = %q, want %q", in, got, want)
		}
	}
}
