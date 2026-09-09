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
	for _, want := range []string{"-ss 5.00", "-i in.mp4", "scale=w=480:h=-1,format=yuvj420p", "out.jpg"} {
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
		// format=yuvj420p re-tags the decoded nv12 frame as full-range
		// before the mjpeg encoder sees it: without this, mjpeg's
		// "blind init" path (reached when a seek lands with zero frames
		// actually decoded, e.g. because the DB's recorded segment
		// duration is stale relative to the file on disk) hard-fails
		// with "Non full-range YUV is non-standard" /
		// ff_frame_thread_encoder_init failed instead of just warning,
		// on both the hw and sw paths — see runFFmpegExpectOutput for
		// the companion empty-output guard this doesn't cover.
		"scale_qsv=w=320:h=-1,hwdownload,format=nv12,format=yuvj420p",
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
		// No extra format=nv12 filter between scale_vaapi and hwdownload
		// beyond the one already there (M8 review finding #4); the
		// trailing format=yuvj420p after hwdownload,format=nv12 is the
		// full-range re-tag for mjpeg, same as the QSV path above.
		"scale_vaapi=w=480:h=-1,hwdownload,format=nv12,format=yuvj420p",
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
		"tile=5x2", "scale=w=320:h=-1", "-vsync 0", "tile=5x2,format=yuvj420p",
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
		`select='eq(pict_type\,I)*(isnan(prev_selected_t)+gte(t-prev_selected_t\,3))',scale_qsv=w=320:h=-1,hwdownload,format=nv12,tile=5x2,format=yuvj420p`,
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
	if !strings.Contains(joined, `select='eq(pict_type\,I)*(isnan(prev_selected_t)+gte(t-prev_selected_t\,3))',scale_vaapi=w=320:h=-1,hwdownload,format=nv12,tile=5x2,format=yuvj420p`) {
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
// arguments and writes a non-empty file at its last argument (the -y
// output path), standing in for a real ffmpeg binary in tests that need
// Thumbnail/Sprite to actually "succeed" without a real encoder present.
// Non-empty matters: runFFmpegExpectOutput treats a 0-byte output as a
// failure (real ffmpeg can exit 0 while writing nothing), so a bare
// "touch" here would make every one of these tests look like that
// failure mode instead of a clean success.
func writeFakeFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ffmpeg.sh")
	script := "#!/bin/sh\neval out=\\${$#}\necho fake-jpeg-bytes > \"$out\"\n"
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
		"xstack=inputs=10:layout=0_0|320_0|640_0|960_0|1280_0|0_180|320_180|640_180|960_180|1280_180[stack];[stack]format=yuvj420p[out]",
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
// the n inputs carries its own -hwaccel/-hwaccel_device/
// -hwaccel_output_format flags (per-input options, fine to repeat)
// against the one -init_hw_device/-filter_hw_device declared up front —
// -filter_hw_device is a single *global* option and ffmpeg rejects it
// ("Only one filter device can be used") if passed once per input, which
// is exactly what production logs showed (exit status 234) for every
// QSV/VAAPI sprite before this fix. The per-input filter downloads back
// to software (nv12) before xstack combines them — xstack itself has no
// hw-surface-aware variant.
func TestSpriteSeekArgsQSV(t *testing.T) {
	args := spriteSeekArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeQSV, Device: "/dev/dri/renderD128"}, 5, 2, 320, 180, 3, 10)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"-filter_hw_device hw",
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
	if got := strings.Count(joined, "-filter_hw_device"); got != 1 {
		t.Errorf("spriteSeekArgs should declare -filter_hw_device exactly once (it's a global option, not per-input), got %d: %q", got, joined)
	}
}

func TestSpriteSeekArgsVAAPI(t *testing.T) {
	args := spriteSeekArgs("in.mp4", "out.jpg", SpriteOptions{Mode: ModeVAAPI, Device: "/dev/dri/renderD128"}, 5, 2, 320, 180, 3, 10)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device vaapi=hw:/dev/dri/renderD128",
		"-filter_hw_device hw",
		"-hwaccel_device hw", "-hwaccel vaapi", "-hwaccel_output_format vaapi",
		"scale_vaapi=w=320:h=180,hwdownload,format=nv12[v0]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("spriteSeekArgs vaapi = %q, missing %q", joined, want)
		}
	}
	if got := strings.Count(joined, "-filter_hw_device"); got != 1 {
		t.Errorf("spriteSeekArgs should declare -filter_hw_device exactly once, got %d: %q", got, joined)
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
	body := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\ncase \"$*\" in\n *%s*) exit 1 ;;\nesac\neval out=\\${$#}\necho fake-jpeg-bytes > \"$out\"\n", log, failSubstr)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return script, log
}

// TestThumbnailTreatsEmptyOutputAsHWFailure covers the flip side of the
// format=yuvj420p fix above: ffmpeg can exit 0 while writing a 0-byte
// file ("Output file is empty, nothing was encoded") when it has to
// initialize its encoder "blind" without ever seeing a real frame — e.g.
// a seek landing past the segment's actual duration because the DB's
// recorded duration_s is stale relative to the file on disk. A bare
// exit-status check would let that empty file get renamed into the cache
// and marked "done"; runFFmpegExpectOutput must catch it and make
// Thumbnail retry in software the same as any other hw failure.
func TestThumbnailTreatsEmptyOutputAsHWFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	// "scale=w=480:h=-1" (no scale_qsv/scale_vaapi prefix) only appears in
	// the software attempt's -vf, so only that retry gets a real frame.
	fakeFFmpeg, callLog := writeFakeFFmpegEmptyOutputUnless(t, dir, "scale=w=480:h=-1,format=yuvj420p")
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	out := filepath.Join(t.TempDir(), "thumb.jpg")
	var fallbacks int
	err := Thumbnail(context.Background(), "in.mp4", out, ThumbOptions{
		Mode: ModeQSV, Device: "/dev/dri/renderD128", AtSec: 5, Width: 480,
		OnHWFallback: func(error) { fallbacks++ },
	})
	if err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	if fallbacks != 1 {
		t.Errorf("OnHWFallback called %d times, want 1 (empty QSV output should count as a failure)", fallbacks)
	}
	info, statErr := os.Stat(out)
	if statErr != nil {
		t.Fatalf("stat output: %v", statErr)
	}
	if info.Size() == 0 {
		t.Errorf("final thumbnail is 0 bytes — empty ffmpeg output was wrongly accepted as success")
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	// 3, not 2: the empty QSV attempt now also gets an overshoot retry at
	// -ss 0.50 (still QSV, still empty against this fake) before Thumbnail
	// falls back to software, which succeeds on its first (AtSec=5) try
	// since okSubstr only requires the software -vf, not any particular
	// -ss.
	if got := strings.Count(strings.TrimSpace(string(calls)), "\n") + 1; got != 3 {
		t.Errorf("ffmpeg invocation count = %d, want 3 (empty QSV attempt + QSV overshoot retry + software retry): %q", got, calls)
	}
}

// writeFakeFFmpegEmptyOutputUnless writes a fake ffmpeg that logs each
// invocation, then produces a 0-byte output file (mimicking ffmpeg's own
// "exit 0 but nothing encoded" behavior) unless the invocation's args
// contain okSubstr, in which case it writes a real, non-empty file.
func writeFakeFFmpegEmptyOutputUnless(t *testing.T, dir, okSubstr string) (script, log string) {
	t.Helper()
	log = filepath.Join(dir, "calls.log")
	script = filepath.Join(dir, "fake-ffmpeg-empty.sh")
	body := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
eval out=\${$#}
case "$*" in
 *"%s"*) echo "not empty" > "$out" ;;
 *) : > "$out" ;;
esac
`, log, okSubstr)
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

// TestThumbnailRetriesAtStartOnSeekOvershoot covers the seek-overshoot
// fix: a stale DB duration_s can make AtSec land past the file's real
// duration, producing ffmpeg's "exit 0, empty output" failure mode.
// Thumbnail must retry once at overshootRetrySec (0.5s) before giving up,
// since a real frame almost certainly exists near the start of the file.
func TestThumbnailRetriesAtStartOnSeekOvershoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	// Only the retry at -ss 0.50 gets a real frame; the initial -ss 79.00
	// (mimicking a stale 79s duration_s against a 3.5s file) is empty.
	fakeFFmpeg, callLog := writeFakeFFmpegEmptyOutputUnless(t, dir, "-ss 0.50")
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	out := filepath.Join(t.TempDir(), "thumb.jpg")
	err := Thumbnail(context.Background(), "in.mp4", out, ThumbOptions{
		Mode: ModeSoftware, AtSec: 79, Width: 480,
	})
	if err != nil {
		t.Fatalf("Thumbnail: %v", err)
	}
	info, statErr := os.Stat(out)
	if statErr != nil {
		t.Fatalf("stat output: %v", statErr)
	}
	if info.Size() == 0 {
		t.Errorf("final thumbnail is 0 bytes — overshoot retry was not accepted")
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ffmpeg invocation count = %d, want 2 (overshot attempt + retry at start): %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "-ss 79.00") {
		t.Errorf("first call should use the original (overshooting) seek: %q", lines[0])
	}
	if !strings.Contains(lines[1], "-ss 0.50") {
		t.Errorf("second call should retry at overshootRetrySec: %q", lines[1])
	}
}

// TestThumbnailNoOvershootRetryWhenAtSecAlreadyZero covers the guard on
// the overshoot retry: if the seek was already 0 (or negative, clamped to
// 0), retrying at 0.5s wouldn't fix anything a plain empty-output failure
// didn't already rule out, so Thumbnail must not spend a second ffmpeg
// invocation on it.
func TestThumbnailNoOvershootRetryWhenAtSecAlreadyZero(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	// okSubstr that never matches -> every invocation is empty.
	fakeFFmpeg, callLog := writeFakeFFmpegEmptyOutputUnless(t, dir, "never-matches")
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	out := filepath.Join(t.TempDir(), "thumb.jpg")
	err := Thumbnail(context.Background(), "in.mp4", out, ThumbOptions{
		Mode: ModeSoftware, AtSec: 0, Width: 480,
	})
	if err == nil {
		t.Fatalf("Thumbnail: expected error, got nil")
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 1 {
		t.Errorf("ffmpeg invocation count = %d, want 1 (no pointless retry when AtSec is already 0): %q", len(lines), lines)
	}
}

// TestSpriteProbesAndRegeneratesOnSeekOvershoot covers the sprite side of
// the seek-overshoot fix: SegmentDurationS (from a stale duration_s) makes
// every seek in the primary (xstack) attempt land past the file's real
// duration and, since generateSpriteFrame's keyframe-select fallback also
// fails against the same fake ffmpeg, Sprite must probe the real duration
// via ffprobe, regenerate using the interval that duration implies, and
// report the corrected duration back via OnDurationCorrected.
func TestSpriteProbesAndRegeneratesOnSeekOvershoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg/ffprobe scripts assume a POSIX shell")
	}
	dir := t.TempDir()

	// cols=1,rows=1 (n=1): original SegmentDurationS=100 -> interval=100,
	// seek T_0=(0+0.5)*100="-ss 50.00" (overshoots a 3.5s file). Probed
	// duration 3.5 -> corrected interval=3.5, T_0=(0+0.5)*3.5="-ss 1.75",
	// which is the only invocation the fake ffmpeg accepts. spriteArgs'
	// keyframe-select fallback never passes "-ss" at all, so it can't
	// accidentally match either okSubstr and stays empty every time,
	// exactly like a real file with no keyframes near either guess.
	fakeFFmpeg, ffmpegLog := writeFakeFFmpegEmptyOutputUnless(t, dir, "-ss 1.75")
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	fakeFFprobe := writeFakeFFprobeDuration(t, dir, "3.5")
	t.Setenv("DASHCAM_FFPROBE", fakeFFprobe)

	cacheDir := t.TempDir()
	jpg := filepath.Join(cacheDir, "sprites", "9", "9.jpg")
	vtt := filepath.Join(cacheDir, "sprites", "9", "9.vtt")

	var correctedTo float64
	var correctedCalls int
	err := Sprite(context.Background(), "in.mp4", jpg, vtt, SpriteOptions{
		Mode: ModeSoftware, Cols: 1, Rows: 1, TileWidth: 320, SegmentDurationS: 100,
		OnDurationCorrected: func(d float64) {
			correctedCalls++
			correctedTo = d
		},
	})
	if err != nil {
		t.Fatalf("Sprite: %v", err)
	}
	if _, statErr := os.Stat(jpg); statErr != nil {
		t.Errorf("sprite jpg not created after overshoot recovery: %v", statErr)
	}
	if correctedCalls != 1 {
		t.Fatalf("OnDurationCorrected called %d times, want 1", correctedCalls)
	}
	if correctedTo != 3.5 {
		t.Errorf("OnDurationCorrected duration = %v, want 3.5", correctedTo)
	}

	calls, err := os.ReadFile(ffmpegLog)
	if err != nil {
		t.Fatalf("read ffmpeg call log: %v", err)
	}
	joined := string(calls)
	if !strings.Contains(joined, "-ss 50.00") {
		t.Errorf("expected an overshooting attempt at -ss 50.00 (original duration): %q", joined)
	}
	if !strings.Contains(joined, "-ss 1.75") {
		t.Errorf("expected the corrected-duration retry at -ss 1.75: %q", joined)
	}
}

// writeFakeFFprobeDuration writes a shell script standing in for ffprobe:
// it ignores its arguments and prints minimal -show_format JSON reporting
// durationS as format.duration, enough for media.Probe to parse.
func writeFakeFFprobeDuration(t *testing.T, dir, durationS string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ffprobe.sh")
	script := fmt.Sprintf("#!/bin/sh\ncat <<EOF\n{\"format\":{\"duration\":\"%s\"},\"streams\":[]}\nEOF\n", durationS)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	return path
}

func TestUnreadableReasonNil(t *testing.T) {
	if reason, ok := UnreadableReason(nil); ok {
		t.Errorf("UnreadableReason(nil) = (%q, true), want ok=false", reason)
	}
}

func TestUnreadableReasonMoovAtom(t *testing.T) {
	err := fmt.Errorf("media: ffmpeg /usr/bin/ffmpeg: exit status 1: in.mp4: moov atom not found")
	reason, ok := UnreadableReason(err)
	if !ok || reason != "Missing moov atom" {
		t.Errorf("UnreadableReason(moov atom) = (%q, %v), want (\"Missing moov atom\", true)", reason, ok)
	}
}

func TestUnreadableReasonCorruptHeaderVariants(t *testing.T) {
	cases := []string{
		"error reading header",
		"Invalid data found when processing input",
		"invalid STSD",
	}
	for _, stderr := range cases {
		err := fmt.Errorf("media: ffmpeg /usr/bin/ffmpeg: exit status 1: in.mp4: %s", stderr)
		reason, ok := UnreadableReason(err)
		if !ok || reason != "Corrupt header" {
			t.Errorf("UnreadableReason(%q) = (%q, %v), want (\"Corrupt header\", true)", stderr, reason, ok)
		}
	}
}

func TestUnreadableReasonEmptyOutputAfterRetry(t *testing.T) {
	err := fmt.Errorf("%w: %w", errEmptyOutputAfterRetry, errEmptyOutput)
	reason, ok := UnreadableReason(err)
	if !ok || reason != "No decodable frames" {
		t.Errorf("UnreadableReason(empty after retry) = (%q, %v), want (\"No decodable frames\", true)", reason, ok)
	}
}

func TestUnreadableReasonTransientNotClassified(t *testing.T) {
	cases := []error{
		fmt.Errorf("media: ffmpeg /usr/bin/ffmpeg: exit status 1: device or resource busy"),
		errEmptyOutput, // a plain overshoot before any retry is not (yet) unreadable
		fmt.Errorf("media: thumbnail mkdir: permission denied"),
	}
	for _, err := range cases {
		if reason, ok := UnreadableReason(err); ok {
			t.Errorf("UnreadableReason(%v) = (%q, true), want ok=false", err, reason)
		}
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
