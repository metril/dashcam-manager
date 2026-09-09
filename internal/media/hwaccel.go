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

// probeQSVEncoder runs a throwaway single-frame encode with encoder (e.g.
// "hevc_qsv", "av1_qsv") to confirm it actually initializes against device
// at runtime, not just that ffmpeg's build lists it — mirrors probeQSV,
// generalized to a specific codec so DetectCapabilities can probe more
// than just h264_qsv. Unlike probeQSV/probeVAAPI (which decode-and-drop
// via `-f null` with no downstream encoder, so they hwdownload+format the
// frame back to something a null muxer accepts), this feeds the QSV
// surface straight from scale_qsv into encoder — appending
// `hwdownload,format=nv12` here breaks the handoff to a *_qsv encoder on
// at least one confirmed runtime ("Impossible to convert between the
// formats supported by the filter 'Parsed_scale_qsv_1' and the filter
// 'auto_scale_1'"), since the encoder itself expects to consume the hw
// frame, not a downloaded one.
func probeQSVEncoder(ctx context.Context, device, encoder string) bool {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "qsv=hw:" + device,
		"-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1",
		"-vf", "hwupload=extra_hw_frames=8,scale_qsv=w=32:h=32",
		"-frames:v", "1", "-c:v", encoder, "-f", "null", "-",
	}
	return exec.CommandContext(ctx, ffmpegBin(), args...).Run() == nil
}

// Capabilities describes which video encoders this ffmpeg build/host can
// actually use, probed once at startup (mirroring Detect/Mode) so export
// codec/quality validation can reject a combination the running container
// can't produce with a clear 400 instead of failing deep inside an ffmpeg
// invocation. Software libx264 is always assumed present — every ffmpeg
// build the M11/M-export pipeline targets ships it — so it has no field
// here (see stitch.go's codec selection, which never needs to check it).
type Capabilities struct {
	H264QSV   bool
	HEVCQSV   bool
	AV1QSV    bool
	Libx265   bool
	Libsvtav1 bool
	Libaomav1 bool
}

// DetectCapabilities probes every non-h264/libx264 encoder the export
// codec matrix can select: QSV variants need both a compiled-in entry
// (`ffmpeg -encoders`) AND a working runtime init (probeQSVEncoder, same
// rationale as probeQSV/probeVAAPI — `-hwaccels` only says the entry
// exists, not that it works against this device); software encoders only
// need to be compiled in, so they're just grepped out of `-encoders`.
// Falls back to an all-false Capabilities{} if ffmpeg can't even be run.
func DetectCapabilities(ctx context.Context, device string) Capabilities {
	encOut, err := exec.CommandContext(ctx, ffmpegBin(), "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		log.Printf("[media] ffmpeg -encoders failed, assuming no hardware/extra codecs: %v", err)
		return Capabilities{}
	}
	enc := string(encOut)

	hwOut, err := exec.CommandContext(ctx, ffmpegBin(), "-hwaccels").CombinedOutput()
	hasQSV := err == nil && strings.Contains(string(hwOut), "qsv")

	var c Capabilities
	if hasQSV && strings.Contains(enc, "h264_qsv") {
		c.H264QSV = probeQSV(ctx, device)
	}
	if hasQSV && strings.Contains(enc, "hevc_qsv") {
		c.HEVCQSV = probeQSVEncoder(ctx, device, "hevc_qsv")
	}
	if hasQSV && strings.Contains(enc, "av1_qsv") {
		c.AV1QSV = probeQSVEncoder(ctx, device, "av1_qsv")
	}
	c.Libx265 = strings.Contains(enc, "libx265")
	c.Libsvtav1 = strings.Contains(enc, "libsvtav1")
	c.Libaomav1 = strings.Contains(enc, "libaom-av1")
	return c
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
