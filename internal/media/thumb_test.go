package media

import (
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
		"scale_vaapi=w=480:h=-1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("thumbArgs vaapi = %q, missing %q", joined, want)
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
	for _, want := range []string{"tile=5x2", "fps=0.333", "scale=w=320:h=-1"} {
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
