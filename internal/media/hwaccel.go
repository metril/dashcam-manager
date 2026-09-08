package media

import (
	"context"
	"log"
	"os/exec"
	"strings"
)

// Mode names the ffmpeg hardware-acceleration pipeline to use for
// thumbnail/sprite generation.
type Mode string

const (
	ModeQSV      Mode = "qsv"
	ModeVAAPI    Mode = "vaapi"
	ModeSoftware Mode = "software"
)

// probeTimeoutArgs is a minimal single-frame encode used to confirm a
// hwaccel actually initializes against device, not just that ffmpeg was
// compiled with the entry: `-hwaccels` lists compiled-in support, but
// init can still fail at runtime without the right driver/permissions on
// /dev/dri.
func probeQSV(ctx context.Context, device string) bool {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "qsv=hw:" + device,
		"-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1",
		"-vf", "hwupload=extra_hw_frames=8,scale_qsv=w=32:h=32,hwdownload,format=nv12",
		"-frames:v", "1", "-f", "null", "-",
	}
	return exec.CommandContext(ctx, ffmpegBin(), args...).Run() == nil
}

func probeVAAPI(ctx context.Context, device string) bool {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "vaapi=hw:" + device,
		"-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1",
		"-vf", "format=nv12,hwupload,scale_vaapi=w=32:h=32,hwdownload",
		"-frames:v", "1", "-f", "null", "-",
	}
	return exec.CommandContext(ctx, ffmpegBin(), args...).Run() == nil
}

// Detect probes ffmpeg for usable hardware acceleration against device
// (the DRI render node): it checks `ffmpeg -hwaccels` lists qsv or vaapi,
// then confirms the pipeline actually initializes with a throwaway
// single-frame encode, preferring qsv over vaapi. Falls back to
// ModeSoftware if ffmpeg itself can't be run, neither hwaccel is listed,
// or both fail to initialize.
func Detect(ctx context.Context, device string) Mode {
	out, err := exec.CommandContext(ctx, ffmpegBin(), "-hwaccels").CombinedOutput()
	if err != nil {
		log.Printf("[media] ffmpeg -hwaccels failed, using software: %v", err)
		return ModeSoftware
	}
	list := string(out)

	if strings.Contains(list, "qsv") && probeQSV(ctx, device) {
		return ModeQSV
	}
	if strings.Contains(list, "vaapi") && probeVAAPI(ctx, device) {
		return ModeVAAPI
	}
	return ModeSoftware
}
