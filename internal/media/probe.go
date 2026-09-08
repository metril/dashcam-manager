package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ProbeResult is the subset of ffprobe's format/stream JSON that
// dashcam-manager cares about for a segment video file.
type ProbeResult struct {
	DurationS float64
	Width     int
	Height    int
	CodecName string
	FPS       float64
}

// probeFormat/probeStream mirror ffprobe's -show_format -show_streams JSON
// shape, decoded loosely (only the fields used are named; everything else
// is dropped).
type probeJSON struct {
	Format  probeFormat   `json:"format"`
	Streams []probeStream `json:"streams"`
}

type probeFormat struct {
	Duration string `json:"duration"`
}

type probeStream struct {
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"r_frame_rate"`
	AvgFrameRate string `json:"avg_frame_rate"`
}

// Probe runs ffprobe against path and returns duration, the first video
// stream's dimensions/codec, and its frame rate.
func Probe(ctx context.Context, path string) (ProbeResult, error) {
	args := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams", path}
	out, err := exec.CommandContext(ctx, ffprobeBin(), args...).Output()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("media: ffprobe %s: %w", path, err)
	}

	var raw probeJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return ProbeResult{}, fmt.Errorf("media: parse ffprobe output for %s: %w", path, err)
	}

	var result ProbeResult
	if raw.Format.Duration != "" {
		if d, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
			result.DurationS = d
		}
	}
	for _, s := range raw.Streams {
		if s.CodecType != "video" {
			continue
		}
		result.Width = s.Width
		result.Height = s.Height
		result.CodecName = s.CodecName
		rate := s.AvgFrameRate
		if rate == "" || rate == "0/0" {
			rate = s.RFrameRate
		}
		result.FPS = parseFrameRate(rate)
		break
	}
	return result, nil
}

// runFFprobeText execs ffprobe with args and returns its stdout as text,
// wrapping any error (including stderr) — used by ProbeKeyframeTimes,
// which needs ffprobe's plain csv frame listing rather than its JSON
// format/stream summary (Probe above).
func runFFprobeText(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, ffprobeBin(), args...)
	var stderrBuf limitedBuffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", ffprobeBin(), err, stderrBuf.String())
	}
	return string(out), nil
}

// parseFrameRate parses ffprobe's "num/den" frame rate strings (e.g.
// "30000/1001" or "30/1"), returning 0 for anything malformed or 0/0.
func parseFrameRate(rate string) float64 {
	num, den, ok := strings.Cut(rate, "/")
	if !ok {
		f, _ := strconv.ParseFloat(rate, 64)
		return f
	}
	n, errN := strconv.ParseFloat(num, 64)
	d, errD := strconv.ParseFloat(den, 64)
	if errN != nil || errD != nil || d == 0 {
		return 0
	}
	return n / d
}
