package media

import (
	"context"
	"fmt"
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

func TestSpriteArgsSoftwareUsesKeyframeSelect(t *testing.T) {
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeSoftware}, 5, 2, 320, 3)
	joined := strings.Join(args, " ")
	// Software mode samples keyframes directly (WP2) instead of decoding
	// at a fixed fps: -skip_frame nokey at the demuxer plus a select
	// expression that both restricts to I-frames and time-distributes the
	// picks across the segment (gte(t-prev_selected_t,interval)) — see
	// TestSpriteArgsSelectIsTimeDistributedNotJustFirstN for why picking
	// just the first cols*rows keyframes would be wrong. One tile pass,
	// -vsync 0 so the output timestamps of the dropped-frame stream don't
	// confuse the muxer.
	for _, want := range []string{
		"-skip_frame nokey",
		`select='eq(pict_type\,I)*(isnan(prev_selected_t)+gte(t-prev_selected_t\,3))'`,
		"tile=5x2", "scale=w=320:h=-1", "-vsync 0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteArgs software = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "fps=") {
		t.Errorf("software spriteArgs should not use fps sampling anymore: %q", joined)
	}
}

// TestSpriteArgsSelectIsTimeDistributedNotJustFirstN covers a review
// finding on the WP2 keyframe-select path: select=eq(pict_type\,I) alone
// picks whichever I-frames come first, so on a segment with many more
// keyframes than cols*rows the tile filter (which only consumes the first
// cols*rows matches) would mosaic only the leading part of the segment —
// while the VTT cue file (built from the same uniform interval) claims
// tiles map across the *whole* duration. The select expression must also
// gate on interval-spaced time (gte(t-prev_selected_t,interval)) so the
// frames it does pick land close to the VTT's assumed i*interval cue
// times, whatever the true keyframe density is.
func TestSpriteArgsSelectIsTimeDistributedNotJustFirstN(t *testing.T) {
	const interval = 2.5
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeSoftware}, 5, 2, 320, interval)
	joined := strings.Join(args, " ")
	want := `gte(t-prev_selected_t\,2.5)`
	if !strings.Contains(joined, want) {
		t.Errorf("spriteArgs select should gate on interval-spaced time, missing %q: %q", want, joined)
	}
}

// TestSpriteArgsQSV covers the WP2 follow-up: QSV sprite generation also
// uses time-distributed keyframe sampling now (-skip_frame nokey + the
// select expression above), with select placed before scale_qsv since it
// only inspects frame metadata, not pixel data, and works the same on hw
// or sw frames.
func TestSpriteArgsQSV(t *testing.T) {
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeQSV, Device: "/dev/dri/renderD128"}, 5, 2, 320, 3)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"-skip_frame nokey",
		`select='eq(pict_type\,I)*(isnan(prev_selected_t)+gte(t-prev_selected_t\,3))',scale_qsv=w=320:h=-1,hwdownload,format=nv12,tile=5x2`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteArgs qsv = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "fps=") {
		t.Errorf("qsv spriteArgs should not use fps sampling anymore: %q", joined)
	}
}

// TestSpriteArgsVAAPIChainNoDoubleFormat covers both the WP2 follow-up
// (time-distributed keyframe sampling on the VAAPI path too) and the
// original M8 review finding #4 (no extra format filter between
// scale_vaapi and hwdownload).
func TestSpriteArgsVAAPIChainNoDoubleFormat(t *testing.T) {
	args := spriteArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeVAAPI, Device: "/dev/dri/renderD128"}, 5, 2, 320, 3)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hwaccel_device hw") {
		t.Errorf("spriteArgs vaapi missing -hwaccel_device hw: %q", joined)
	}
	if !strings.Contains(joined, "-skip_frame nokey") {
		t.Errorf("spriteArgs vaapi missing -skip_frame nokey: %q", joined)
	}
	if !strings.Contains(joined, `select='eq(pict_type\,I)*(isnan(prev_selected_t)+gte(t-prev_selected_t\,3))',scale_vaapi=w=320:h=-1,hwdownload,format=nv12,tile=5x2`) {
		t.Errorf("spriteArgs vaapi chain wrong (want select before scale_vaapi, straight into hwdownload,format=nv12): %q", joined)
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

// TestSpriteSeekArgsSoftware covers the WP2 round-3 primary strategy: a
// single ffmpeg invocation opening in n=cols*rows times, each seeking to
// that tile's midpoint T_i=(i+0.5)*interval with -noaccurate_seek (fast
// coarse seek, no decode-forward), scaling each to the fixed tile size,
// and combining them via xstack (not the tile filter — these are n
// separate input streams, not one stream of n frames).
func TestSpriteSeekArgsSoftware(t *testing.T) {
	args := spriteSeekArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeSoftware}, 5, 2, 320, 180, 3, 10)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-noaccurate_seek", "-an", "-sn",
		"-ss 1.50", // T_0 = (0+0.5)*3
		"-ss 28.50", // T_9 = (9+0.5)*3
		"scale=w=320:h=180[v0]",
		"xstack=inputs=10:layout=0_0|320_0|640_0|960_0|1280_0|0_180|320_180|640_180|960_180|1280_180[out]",
		"-map [out]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteSeekArgs software = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "hwaccel") {
		t.Errorf("software spriteSeekArgs should not mention hwaccel: %q", joined)
	}
	if got := strings.Count(joined, "-i in.mp4"); got != 10 {
		t.Errorf("spriteSeekArgs should open in.mp4 10 times (cols*rows), got %d: %q", got, joined)
	}
}

// TestSpriteSeekArgsQSV/VAAPI cover the hwaccel-aware seek path: each of
// the n inputs carries its own -hwaccel/-hwaccel_output_format flags
// against the one -init_hw_device declared up front, and the per-input
// filter downloads back to software (nv12) before xstack combines them —
// xstack itself has no hw-surface-aware variant.
func TestSpriteSeekArgsQSV(t *testing.T) {
	args := spriteSeekArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeQSV, Device: "/dev/dri/renderD128"}, 5, 2, 320, 180, 3, 10)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"-hwaccel_device hw", "-hwaccel qsv", "-hwaccel_output_format qsv",
		"-noaccurate_seek",
		"scale_qsv=w=320:h=180,hwdownload,format=nv12[v0]",
		"xstack=inputs=10:layout=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteSeekArgs qsv = %q, missing %q", joined, want)
		}
	}
	if got := strings.Count(joined, "-init_hw_device"); got != 1 {
		t.Errorf("spriteSeekArgs should declare -init_hw_device once, not once per input, got %d: %q", got, joined)
	}
}

func TestSpriteSeekArgsVAAPI(t *testing.T) {
	args := spriteSeekArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeVAAPI, Device: "/dev/dri/renderD128"}, 5, 2, 320, 180, 3, 10)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device vaapi=hw:/dev/dri/renderD128",
		"-hwaccel_device hw", "-hwaccel vaapi", "-hwaccel_output_format vaapi",
		"scale_vaapi=w=320:h=180,hwdownload,format=nv12[v0]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteSeekArgs vaapi = %q, missing %q", joined, want)
		}
	}
}

// TestXstackLayoutRowMajor covers xstackLayout's offset math directly,
// independent of spriteSeekArgs' hwaccel branching.
func TestXstackLayoutRowMajor(t *testing.T) {
	got := xstackLayout(3, 2, 100, 50)
	want := "0_0|100_0|200_0|0_50|100_50|200_50"
	if got != want {
		t.Errorf("xstackLayout(3,2,100,50) = %q, want %q", got, want)
	}
}

// TestSpriteFallsBackToSelectWhenSeekingFails covers the WP2 round-3
// fallback: when the seek-based xstack invocation fails (e.g. an
// unseekable/corrupt source), Sprite must fall back to the round-2
// keyframe-select path rather than failing outright. Exercised through
// Sprite() with a fake ffmpeg that fails only on the xstack invocation.
func TestSpriteFallsBackToSelectWhenSeekingFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	fakeFFmpeg, callLog := writeFakeFFmpegFailingOn(t, dir, "xstack")
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	cacheDir := t.TempDir()
	jpg := filepath.Join(cacheDir, "sprites", "5", "5.jpg")
	vtt := filepath.Join(cacheDir, "sprites", "5", "5.vtt")

	if err := Sprite(context.Background(), "in.mp4", jpg, vtt, SpriteOptions{
		Mode: ModeSoftware, Cols: 2, Rows: 1, TileWidth: 320, SegmentDurationS: 2,
	}); err != nil {
		t.Fatalf("Sprite: %v", err)
	}
	if _, err := os.Stat(jpg); err != nil {
		t.Errorf("sprite jpg not created after fallback: %v", err)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ffmpeg invocation count = %d, want 2 (failed seek attempt + select fallback): %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "xstack") {
		t.Errorf("first call should be the seek/xstack attempt: %q", lines[0])
	}
	if strings.Contains(lines[1], "xstack") || !strings.Contains(lines[1], "select=") {
		t.Errorf("second call should be the select-based fallback, not another xstack attempt: %q", lines[1])
	}
}

// writeFakeFFmpegFailingOn writes a shell script into dir that logs each
// invocation's arguments to a log file, then exits non-zero (without
// touching the output) for any invocation whose args contain failSubstr —
// standing in for "ffmpeg failed on this specific command" (e.g. the
// xstack seek path) — while still succeeding for every other invocation.
func writeFakeFFmpegFailingOn(t *testing.T, dir, failSubstr string) (script, log string) {
	t.Helper()
	log = filepath.Join(dir, "calls.log")
	script = filepath.Join(dir, "fake-ffmpeg-failing.sh")
	body := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\ncase \"$*\" in\n *%s*) exit 1 ;;\nesac\neval out=\\${$#}\ntouch \"$out\"\n", log, failSubstr)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return script, log
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
