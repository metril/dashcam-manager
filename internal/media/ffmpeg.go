// Package media wraps ffmpeg/ffprobe subprocess invocations for
// dashcam-manager's thumbnail and hover-sprite generation: hardware-
// accelerated (QSV/VAAPI) with automatic fallback to software encoding.
package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
)

// stderrTailBytes bounds how much of a failed ffmpeg run's stderr is kept
// for the wrapped error message, so a noisy failure doesn't blow up logs.
const stderrTailBytes = 4000

// binaries holds the ffmpeg/ffprobe names set by SetBinaries (the
// ffmpeg_bin/ffprobe_bin settings; jellyfin-ffmpeg is installed under a
// non-standard PATH entry in the container image).
var binaries atomic.Pointer[[2]string]

// SetBinaries sets the ffmpeg and ffprobe binaries exec'd from now on; an
// empty name selects the default ("ffmpeg"/"ffprobe" from PATH).
func SetBinaries(ffmpeg, ffprobe string) {
	binaries.Store(&[2]string{ffmpeg, ffprobe})
}

// ffmpegBin returns the ffmpeg binary to exec.
func ffmpegBin() string { return binaryOr(0, "DASHCAM_FFMPEG", "ffmpeg") }

// ffprobeBin returns the ffprobe binary to exec.
func ffprobeBin() string { return binaryOr(1, "DASHCAM_FFPROBE", "ffprobe") }

// binaryOr prefers the SetBinaries value, then the DASHCAM_FFMPEG/
// DASHCAM_FFPROBE environment variable (local dev and tests that never go
// through the settings table), then the default.
func binaryOr(i int, env, def string) string {
	if b := binaries.Load(); b != nil && b[i] != "" {
		return b[i]
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// hwaccelName maps a hardware Mode to the name ffmpeg's -init_hw_device/
// -hwaccel flags use for it (qsv, vaapi, cuda) — the single place
// hwInitArgs/hwInputArgs and any new hw arg-builder needs this mapping.
// Returns "" for ModeSoftware or an unrecognized mode.
func hwaccelName(mode Mode) string {
	switch mode {
	case ModeQSV:
		return "qsv"
	case ModeVAAPI:
		return "vaapi"
	case ModeNVENC:
		return "cuda"
	default:
		return ""
	}
}

// hwInitArgs returns the `-init_hw_device <name>=hw:<device>
// -filter_hw_device hw` pair that initializes mode's hwaccel device once
// per ffmpeg invocation (QSV/VAAPI/NVENC all follow this same pattern;
// -filter_hw_device is a global option ffmpeg rejects if passed more than
// once, see spriteSeekArgs). Returns nil for ModeSoftware or an
// unrecognized mode.
func hwInitArgs(mode Mode, device string) []string {
	name := hwaccelName(mode)
	if name == "" {
		return nil
	}
	return []string{"-init_hw_device", name + "=hw:" + device, "-filter_hw_device", "hw"}
}

// hwInputArgs returns the per-input `-hwaccel_device hw -hwaccel <name>
// -hwaccel_output_format <name>` triple, repeated once per -i (unlike
// hwInitArgs' -filter_hw_device, these are input options — see
// spriteSeekArgs' doc comment). Returns nil for ModeSoftware or an
// unrecognized mode.
func hwInputArgs(mode Mode) []string {
	name := hwaccelName(mode)
	if name == "" {
		return nil
	}
	return []string{"-hwaccel_device", "hw", "-hwaccel", name, "-hwaccel_output_format", name}
}

// ProgressFunc receives updates parsed from an ffmpeg `-progress pipe:1`
// stream as they arrive. outTimeUS is the out_time_us field in
// microseconds (-1 until ffmpeg reports one), speed is the reported
// encoding speed multiplier (0 until reported).
type ProgressFunc func(outTimeUS int64, speed float64)

// runFFmpeg execs ffmpeg with args, returning a wrapped error (including
// the tail of stderr) on failure. ctx cancellation kills the process
// (exec.CommandContext's default behavior). If progress is non-nil, args
// must already include "-progress", "pipe:1" — stdout is then parsed
// incrementally instead of being buffered.
func runFFmpeg(ctx context.Context, args []string, progress ProgressFunc) error {
	cmd := exec.CommandContext(ctx, ffmpegBin(), args...)

	var stderrBuf limitedBuffer
	cmd.Stderr = &stderrBuf

	if progress == nil {
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("media: %s: %w: %s", ffmpegBin(), err, stderrBuf.String())
		}
		return nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("media: ffmpeg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("media: start %s: %w", ffmpegBin(), err)
	}
	scanProgress(stdout, progress)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("media: %s: %w: %s", ffmpegBin(), err, stderrBuf.String())
	}
	return nil
}

// scanProgress reads ffmpeg's `-progress pipe:1` key=value stream from r,
// calling progress once per reported block (each block ends with a
// "progress=continue" or "progress=end" line). It always reads r to EOF
// (draining any bytes ffmpeg writes after its final "progress=end" line)
// rather than returning as soon as "end" is seen: exec.Cmd.Wait, called by
// the caller right after this returns, closes the pipe once the process
// exits, and per its docs it is incorrect to call Wait before all reads
// from a StdoutPipe have completed — an early return here could leave
// unread buffered output and deadlock the process on a full pipe.
func scanProgress(r io.Reader, progress ProgressFunc) {
	sc := bufio.NewScanner(r)
	outTimeUS := int64(-1)
	var speed float64
	ended := false
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "out_time_us":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				outTimeUS = n
			}
		case "speed":
			if f, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil {
				speed = f
			}
		case "progress":
			// After "end", keep scanning (to drain r to EOF, see doc
			// comment above) but stop invoking progress — ffmpeg
			// shouldn't emit another block after "end", but if it did,
			// callers only expect one terminal report.
			if !ended {
				progress(outTimeUS, speed)
				ended = value == "end"
			}
		}
	}
}

// limitedBuffer is a bytes.Buffer that discards leading bytes once it
// exceeds stderrTailBytes, keeping only the most recent output — useful
// for capturing "the last N bytes of stderr" from a long-running ffmpeg
// invocation without unbounded memory growth.
type limitedBuffer struct {
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n, err := b.buf.Write(p)
	if over := b.buf.Len() - stderrTailBytes; over > 0 {
		b.buf.Next(over)
	}
	return n, err
}

func (b *limitedBuffer) String() string {
	return strings.TrimSpace(b.buf.String())
}
