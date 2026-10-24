package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// Mode names the ffmpeg hardware-acceleration pipeline to use for
// thumbnail/sprite generation.
type Mode string

const (
	ModeNVENC    Mode = "nvenc"
	ModeQSV      Mode = "qsv"
	ModeVAAPI    Mode = "vaapi"
	ModeSoftware Mode = "software"
)

// Devices holds the hardware device identifiers ffmpeg arg-builders need,
// one per accel family: DRI is the /dev/dri render node used by QSV/VAAPI,
// CUDA is the CUDA device index used by NVENC/NVDEC.
type Devices struct {
	DRI  string
	CUDA string
}

// For returns the device identifier to use for m: CUDA for ModeNVENC
// (defaulting to "0" when unset, since ReencodeArgs/hwInitArgs gate hw
// init on Device != ""), otherwise DRI. Every other Mode field (Device
// string on ThumbOptions etc.) keeps meaning "the device for this Mode".
func (d Devices) For(m Mode) string {
	if m == ModeNVENC {
		if d.CUDA == "" {
			return "0"
		}
		return d.CUDA
	}
	return d.DRI
}

// probeFrameSize is the edge length of the throwaway probe frame. NVENC
// rejects frames below its minimum dimensions (~145x49 for h264, 129x33
// for hevc), so every probe uses a frame comfortably above that.
const probeFrameSize = 256

func probeSrc() string {
	return fmt.Sprintf("color=c=black:s=%dx%d:d=0.1", probeFrameSize, probeFrameSize)
}

func probeScale() string { return fmt.Sprintf("w=%d:h=%d", probeFrameSize, probeFrameSize) }

func qsvProbeArgs(device string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "qsv=hw:" + device,
		"-f", "lavfi", "-i", probeSrc(),
		"-vf", "hwupload=extra_hw_frames=8,scale_qsv=" + probeScale() + ",hwdownload,format=nv12",
		"-frames:v", "1", "-f", "null", "-",
	}
}

func vaapiProbeArgs(device string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "vaapi=hw:" + device,
		"-f", "lavfi", "-i", probeSrc(),
		"-vf", "format=nv12,hwupload,scale_vaapi=" + probeScale() + ",hwdownload",
		"-frames:v", "1", "-f", "null", "-",
	}
}

func qsvEncoderProbeArgs(device, encoder string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "qsv=hw:" + device,
		"-f", "lavfi", "-i", probeSrc(),
		"-vf", "hwupload=extra_hw_frames=8,scale_qsv=" + probeScale(),
		"-frames:v", "1", "-c:v", encoder, "-f", "null", "-",
	}
}

func nvdecProbeArgs(idx string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "cuda=hw:" + idx,
		"-filter_hw_device", "hw",
		"-f", "lavfi", "-i", probeSrc(),
		"-vf", "format=nv12,hwupload,scale_cuda=" + probeScale() + ",hwdownload,format=nv12",
		"-frames:v", "1", "-f", "null", "-",
	}
}

func nvencProbeArgs(idx, encoder string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "cuda=hw:" + idx,
		"-filter_hw_device", "hw",
		"-f", "lavfi", "-i", probeSrc(),
		"-vf", "format=nv12,hwupload,scale_cuda=" + probeScale() + ":format=nv12",
		"-frames:v", "1", "-c:v", encoder, "-f", "null", "-",
	}
}

// runProbe runs a throwaway ffmpeg probe and, on failure, logs the last
// line of its stderr so a rejected hwaccel is diagnosable.
func runProbe(ctx context.Context, name string, args []string) bool {
	out, err := exec.CommandContext(ctx, ffmpegBin(), args...).CombinedOutput()
	if err == nil {
		return true
	}
	last := ""
	if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) > 0 {
		last = strings.TrimSpace(lines[len(lines)-1])
	}
	if len(last) > 200 {
		last = last[:200]
	}
	log.Printf("[media] probe %s failed: %v: %s", name, err, last)
	return false
}

// probeQSV confirms QSV decode+scale initializes against device.
func probeQSV(ctx context.Context, device string) bool {
	return runProbe(ctx, "qsv", qsvProbeArgs(device))
}

func probeVAAPI(ctx context.Context, device string) bool {
	return runProbe(ctx, "vaapi", vaapiProbeArgs(device))
}

// probeQSVEncoder feeds the QSV surface straight from scale_qsv into
// encoder (no hwdownload: *_qsv encoders consume the hw frame directly).
func probeQSVEncoder(ctx context.Context, device, encoder string) bool {
	return runProbe(ctx, encoder, qsvEncoderProbeArgs(device, encoder))
}

// probeNVDEC mirrors probeQSV/probeVAAPI for CUDA.
func probeNVDEC(ctx context.Context, idx string) bool {
	return runProbe(ctx, "nvdec", nvdecProbeArgs(idx))
}

// probeNVENCEncoder mirrors probeQSVEncoder for CUDA, encoding from the
// cuda surface directly (e.g. "h264_nvenc", "hevc_nvenc", "av1_nvenc").
func probeNVENCEncoder(ctx context.Context, idx, encoder string) bool {
	return runProbe(ctx, encoder, nvencProbeArgs(idx, encoder))
}

// cudaPresent reports whether hwaccelList (an `ffmpeg -hwaccels` listing)
// includes cuda AND /dev/nvidiactl exists — the nvidia-container-toolkit
// device node only present when NVIDIA_DRIVER_CAPABILITIES includes
// "video" and the toolkit actually injected the driver. Gating every CUDA
// probe on this means an Intel-only (or CPU-only) host pays zero extra
// boot time running throwaway cuda ffmpeg invocations that could only
// ever fail.
func cudaPresent(hwaccelList string) bool {
	if !strings.Contains(hwaccelList, "cuda") {
		return false
	}
	_, err := os.Stat("/dev/nvidiactl")
	return err == nil
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
	H264NVENC bool
	HEVCNVENC bool
	AV1NVENC  bool
	Libx265   bool
	Libsvtav1 bool
	Libaomav1 bool
}

// DetectCapabilities probes every non-h264/libx264 encoder the export
// codec matrix can select: QSV/NVENC variants need both a compiled-in
// entry (`ffmpeg -encoders`) AND a working runtime init (probeQSVEncoder/
// probeNVENCEncoder, same rationale as probeQSV/probeVAAPI/probeNVDEC —
// `-hwaccels` only says the entry exists, not that it works against this
// device); software encoders only need to be compiled in, so they're just
// grepped out of `-encoders`. Falls back to an all-false Capabilities{} if
// ffmpeg can't even be run.
func DetectCapabilities(ctx context.Context, devs Devices) Capabilities {
	encOut, err := exec.CommandContext(ctx, ffmpegBin(), "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		log.Printf("[media] ffmpeg -encoders failed, assuming no hardware/extra codecs: %v", err)
		return Capabilities{}
	}
	enc := string(encOut)

	hwOut, err := exec.CommandContext(ctx, ffmpegBin(), "-hwaccels").CombinedOutput()
	hwList := string(hwOut)
	hasQSV := err == nil && strings.Contains(hwList, "qsv")
	hasCUDA := err == nil && cudaPresent(hwList)

	var c Capabilities
	if hasQSV && strings.Contains(enc, "h264_qsv") {
		c.H264QSV = probeQSV(ctx, devs.DRI)
	}
	if hasQSV && strings.Contains(enc, "hevc_qsv") {
		c.HEVCQSV = probeQSVEncoder(ctx, devs.DRI, "hevc_qsv")
	}
	if hasQSV && strings.Contains(enc, "av1_qsv") {
		c.AV1QSV = probeQSVEncoder(ctx, devs.DRI, "av1_qsv")
	}
	if hasCUDA && strings.Contains(enc, "h264_nvenc") {
		c.H264NVENC = probeNVENCEncoder(ctx, devs.For(ModeNVENC), "h264_nvenc")
	}
	if hasCUDA && strings.Contains(enc, "hevc_nvenc") {
		c.HEVCNVENC = probeNVENCEncoder(ctx, devs.For(ModeNVENC), "hevc_nvenc")
	}
	if hasCUDA && strings.Contains(enc, "av1_nvenc") {
		c.AV1NVENC = probeNVENCEncoder(ctx, devs.For(ModeNVENC), "av1_nvenc")
	}
	c.Libx265 = strings.Contains(enc, "libx265")
	c.Libsvtav1 = strings.Contains(enc, "libsvtav1")
	c.Libaomav1 = strings.Contains(enc, "libaom-av1")
	return c
}

// Detect probes ffmpeg for usable hardware acceleration against devs: it
// checks `ffmpeg -hwaccels` lists cuda/qsv/vaapi, then confirms the
// pipeline actually initializes with a throwaway single-frame encode,
// preferring nvenc over qsv over vaapi (per the "auto" mode's NVIDIA >
// QSV > VAAPI > software design). The nvenc check also probes
// h264_nvenc's encoder init, not just NVDEC decode, so a decode-only card
// doesn't get selected as if it could also encode. Falls back to
// ModeSoftware if ffmpeg itself can't be run, no hwaccel is listed, or
// all fail to initialize.
func Detect(ctx context.Context, devs Devices) Mode {
	out, err := exec.CommandContext(ctx, ffmpegBin(), "-hwaccels").CombinedOutput()
	if err != nil {
		log.Printf("[media] ffmpeg -hwaccels failed, using software: %v", err)
		return ModeSoftware
	}
	list := string(out)

	if cudaPresent(list) {
		cudaDev := devs.For(ModeNVENC)
		if probeNVDEC(ctx, cudaDev) && probeNVENCEncoder(ctx, cudaDev, "h264_nvenc") {
			return ModeNVENC
		}
	}
	if strings.Contains(list, "qsv") && probeQSV(ctx, devs.DRI) {
		return ModeQSV
	}
	if strings.Contains(list, "vaapi") && probeVAAPI(ctx, devs.DRI) {
		return ModeVAAPI
	}
	return ModeSoftware
}
