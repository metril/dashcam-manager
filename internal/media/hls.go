// hls.go: on-demand per-segment transcode to fMP4 HLS, for browsers that
// can't play the source HEVC directly (see docs/superpowers/specs/
// 2026-09-08-dashcam-manager-design.md's HLS fallback requirement — the
// M-series milestones never wired this up; final-fix-wave item 11 closes
// that gap). Segments are ~30s, so unlike a long-form video HLS pipeline
// there's no benefit to generating chunks incrementally as they're
// requested: GenerateHLS transcodes the whole segment in one ffmpeg run
// and writes a complete VOD playlist (master.m3u8 + an fMP4 init segment +
// a handful of .m4s media segments) to outDir in one shot.
package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// HLSMasterName is the playlist filename GenerateHLS writes into outDir,
// and the name GET /api/segments/{id}/hls/master.m3u8 serves.
const HLSMasterName = "master.m3u8"

// HLSInitName is the fMP4 initialization segment's filename.
const HLSInitName = "init.mp4"

// hlsSegmentTimeS is the target duration of each .m4s media segment.
// Segments here are always ~30s clips, so this just controls how many
// chunks a single segment's playlist has (30s / 4s ≈ 7-8) — short enough
// for a player to start rendering quickly, long enough not to multiply
// ffmpeg's per-segment muxing overhead pointlessly.
const hlsSegmentTimeS = 4

// HLSDir returns the cache directory holding segmentID's generated HLS
// playlist/segments, under cacheDir (DASHCAM_CACHE) — sharded the same
// way as ThumbPath/SpritePaths so a large library doesn't dump every
// segment's HLS output into one flat directory.
func HLSDir(cacheDir string, segmentID int64) string {
	return filepath.Join(cacheDir, "hls", shardDir(segmentID), fmt.Sprintf("%d", segmentID))
}

// hlsSegmentFilePattern matches the .m4s media segment filenames
// GenerateHLS's ffmpeg run produces (see hlsArgs' -hls_segment_filename):
// used by the HTTP handler to validate a requested file path component
// without a directory listing, alongside the two fixed names above.
var hlsSegmentFilePattern = regexp.MustCompile(`^seg[0-9]{3}\.m4s$`)

// IsValidHLSFile reports whether name is a filename GenerateHLS could have
// produced (the master playlist, the init segment, or a numbered .m4s
// media segment) — used to keep the on-disk HLS cache as the only files a
// GET /api/segments/{id}/hls/{file} request can ever read, rejecting
// anything else (including path traversal attempts) before it ever
// reaches the filesystem.
func IsValidHLSFile(name string) bool {
	return name == HLSMasterName || name == HLSInitName || hlsSegmentFilePattern.MatchString(name)
}

// HLSOptions configures a GenerateHLS run.
type HLSOptions struct {
	Mode   Mode
	Device string // DRI render node, used when Mode is qsv
}

// GenerateHLS transcodes in (a source segment, any codec/resolution) into
// a complete fMP4 HLS VOD playlist under outDir: capped at 1080p (see
// ResolveOutputDims — smaller sources are left at their own resolution,
// never upscaled), h264_qsv when opts.Mode is qsv, falling back to
// software libx264 on any hardware-path failure (mirrors Thumbnail's
// fallback behavior) or when opts.Mode is anything else. The source's
// audio is dropped (-an) since dashcam segments never carry an audio
// stream — see the README's "no audio" note; ffprobe was used to confirm
// this against real segments before writing this file.
//
// Writes via a temp sibling directory + rename so a killed/failed run (or
// a concurrent caller racing this one, though callers are expected to
// serialize via singleflight — see cmd/api/handlers_hls.go) never leaves a
// partially-written playlist at outDir: a player that read a half-written
// master.m3u8 or a missing .m4s file would fail confusingly rather than
// with a clean "not found yet".
func GenerateHLS(ctx context.Context, in, outDir string, opts HLSOptions) error {
	parent := filepath.Dir(outDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("media: hls mkdir: %w", err)
	}
	tmpDir, err := os.MkdirTemp(parent, ".hls-*")
	if err != nil {
		return fmt.Errorf("media: hls temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // no-op once the rename below succeeds

	probe, err := Probe(ctx, in)
	if err != nil {
		return fmt.Errorf("media: hls probe %s: %w", in, err)
	}
	outW, outH := ResolveOutputDims("1080p", probe.Width, probe.Height)

	err = runFFmpeg(ctx, hlsArgs(in, tmpDir, opts, outW, outH), nil)
	if err != nil && opts.Mode != ModeSoftware {
		log.Printf("[media] hls %s: %s hwaccel failed, falling back to software: %v", in, opts.Mode, err)
		swOpts := opts
		swOpts.Mode = ModeSoftware
		err = runFFmpeg(ctx, hlsArgs(in, tmpDir, swOpts, outW, outH), nil)
	}
	if err != nil {
		return fmt.Errorf("media: hls %s: %w", in, err)
	}

	// Best-effort stale-directory removal: a previous partial/failed run
	// (e.g. the process was killed between a prior GenerateHLS's rename
	// and this one) could theoretically leave outDir present; os.Rename
	// over a non-empty existing directory fails on Linux, so clear it
	// first. Not itself a correctness requirement under normal operation
	// (GenerateHLS callers only run when outDir's master playlist is
	// missing), just defense in depth.
	_ = os.RemoveAll(outDir)
	if err := os.Rename(tmpDir, outDir); err != nil {
		return fmt.Errorf("media: hls rename into place: %w", err)
	}
	return nil
}

// hlsArgs builds the ffmpeg argv for one GenerateHLS attempt.
func hlsArgs(in, tmpDir string, opts HLSOptions, outW, outH int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}

	switch opts.Mode {
	case ModeQSV:
		args = append(args,
			"-init_hw_device", "qsv=hw:"+opts.Device,
			"-hwaccel_device", "hw",
			"-filter_hw_device", "hw",
			"-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
			"-i", in,
			"-vf", fmt.Sprintf("scale_qsv=w=%d:h=%d", outW, outH),
			"-c:v", "h264_qsv", "-preset", "veryfast", "-global_quality", "22",
		)
	default:
		args = append(args,
			"-i", in,
			"-vf", fmt.Sprintf("scale=w=%d:h=%d", outW, outH),
			"-c:v", "libx264", "-crf", "22", "-preset", "veryfast",
		)
	}

	args = append(args,
		"-an",
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", hlsSegmentTimeS),
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", HLSInitName,
		"-hls_segment_filename", filepath.Join(tmpDir, "seg%03d.m4s"),
		filepath.Join(tmpDir, HLSMasterName),
	)
	return args
}
