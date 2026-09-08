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
)

// stderrTailBytes bounds how much of a failed ffmpeg run's stderr is kept
// for the wrapped error message, so a noisy failure doesn't blow up logs.
const stderrTailBytes = 4000

// ffmpegBin returns the ffmpeg binary to exec, honoring DASHCAM_FFMPEG
// (jellyfin-ffmpeg is installed under a non-standard PATH entry in the
// container image; local dev can point this at any ffmpeg build).
func ffmpegBin() string { return envOr("DASHCAM_FFMPEG", "ffmpeg") }

// ffprobeBin returns the ffprobe binary to exec, honoring DASHCAM_FFPROBE.
func ffprobeBin() string { return envOr("DASHCAM_FFPROBE", "ffprobe") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
