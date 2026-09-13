package media

import (
	"strings"
	"testing"
)

func TestHLSDirShardsBySegmentID(t *testing.T) {
	got := HLSDir("/cache", 1234)
	want := "/cache/hls/234/1234"
	if got != want {
		t.Errorf("HLSDir(/cache, 1234) = %q, want %q", got, want)
	}
}

func TestIsValidHLSFile(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"master.m3u8", true},
		{"init.mp4", true},
		{"seg000.m4s", true},
		{"seg123.m4s", true},
		{"seg12.m4s", false},   // wrong digit count
		{"seg0001.m4s", false}, // wrong digit count
		{"../../etc/passwd", false},
		{"seg000.m4s/../../../etc/passwd", false},
		{"", false},
		{"master.m3u8.bak", false},
		{"segabc.m4s", false},
	}
	for _, c := range cases {
		if got := IsValidHLSFile(c.name); got != c.ok {
			t.Errorf("IsValidHLSFile(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestHLSArgsSoftware(t *testing.T) {
	args := hlsArgs("in.mp4", "/tmp/out", HLSOptions{Mode: ModeSoftware}, 1920, 1080)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-i in.mp4",
		"scale=w=1920:h=1080",
		"-c:v libx264",
		"-an",
		"-f hls",
		"-hls_playlist_type vod",
		"-hls_segment_type fmp4",
		"-hls_fmp4_init_filename init.mp4",
		"-hls_segment_filename /tmp/out/seg%03d.m4s",
		"/tmp/out/master.m3u8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("hlsArgs software = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "hwaccel") {
		t.Errorf("software args should not mention hwaccel: %q", joined)
	}
}

func TestHLSArgsNVENC(t *testing.T) {
	args := hlsArgs("in.mp4", "/tmp/out", HLSOptions{Mode: ModeNVENC, Device: "0"}, 1920, 1080)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device cuda=hw:0",
		"-hwaccel cuda",
		"-hwaccel_output_format cuda",
		"scale_cuda=w=1920:h=1080:format=nv12",
		"-c:v h264_nvenc",
		"-preset p4", "-rc vbr", "-cq 22", "-b:v 0",
		"-an",
		"-hls_segment_type fmp4",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("hlsArgs nvenc = %q, missing %q", joined, want)
		}
	}
}

func TestHLSArgsQSV(t *testing.T) {
	args := hlsArgs("in.mp4", "/tmp/out", HLSOptions{Mode: ModeQSV, Device: "/dev/dri/renderD128"}, 1920, 1080)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device qsv=hw:/dev/dri/renderD128",
		"-hwaccel qsv",
		"-hwaccel_output_format qsv",
		"scale_qsv=w=1920:h=1080",
		"-c:v h264_qsv",
		"-preset veryfast",
		"-an",
		"-hls_segment_type fmp4",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("hlsArgs qsv = %q, missing %q", joined, want)
		}
	}
}
