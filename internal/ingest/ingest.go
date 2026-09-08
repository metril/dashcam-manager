// Package ingest copies segments (video + sidecars) from a scanned SSD
// source to a mounted share, verifying every copy (size, and — by
// default — a streamed sha256) before recording it as done in the
// `ingests` table, and never re-copying a segment a previous run already
// verified (resumable). internal/ingest/cleanup.go builds on the same
// `ingests` rows to safely delete verified SSD originals.
package ingest

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/share"
)

// Verify mode values accepted by Opts.Verify.
const (
	VerifySHA256 = "sha256"
	VerifySize   = "size"

	// internal alias kept short for use throughout this file/copy.go.
	verifySHA256 = VerifySHA256
)

// sidecarExts are the sidecar files (relative to a segment's base path,
// i.e. its .mp4 path with the extension stripped) copied alongside the
// video, mirroring internal/scan/walker.go's sidecarExts.
var sidecarExts = []string{".timestamp.json", ".detections.json", ".gps.json"}

// allowLocalSharePathsKey is a hidden settings key (never surfaced in the
// Settings UI/handlers_settings.go's known-keys list): when set to true,
// ResolveTargetMount trusts a share's stored mount_point directly instead
// of requiring share.IsMounted to see it as a live CIFS mount — so tests
// (and, if ever needed, a manual local-disk target) can point a share row
// at a plain temp/local directory without going through mount.cifs.
const allowLocalSharePathsKey = "allow_local_share_paths"

// Opts configures a Run call.
type Opts struct {
	// SourceID is the scanned source (normally the SSD) to ingest from.
	SourceID int64
	// TargetShareID is the destination share.
	TargetShareID int64
	// TripIDs restricts ingest to these trips; empty means every trip
	// currently indexed for SourceID.
	TripIDs []int64
	// Verify is "sha256" (default) or "size".
	Verify string
}

// Stats summarizes one Run call.
type Stats struct {
	SegmentsTotal   int
	SegmentsCopied  int
	SegmentsSkipped int // already verified from a previous run (dedupe)
	SegmentsFailed  int
	BytesCopied     int64
	Duration        time.Duration
	ThroughputMBs   float64
}

// ReportFunc reports ingest progress: doneSegments/totalSegments,
// bytesDone/bytesTotal (across every segment, updated as each file
// streams), and a short message (the segment currently being copied).
type ReportFunc func(doneSegments, totalSegments int, bytesDone, bytesTotal int64, message string)

// ResolveTargetMount returns the filesystem path to ingest into for
// shareID, failing if the share has no mount_point configured or isn't
// actually available: normally that means share.IsMounted(mount_point);
// with the allow_local_share_paths setting on, a plain existing directory
// is accepted instead (see allowLocalSharePathsKey).
func ResolveTargetMount(ctx context.Context, store *db.DB, shareID int64) (string, error) {
	s, err := store.ShareGet(ctx, shareID)
	if err != nil {
		return "", fmt.Errorf("ingest: target share %d: %w", shareID, err)
	}
	if s.MountPoint == "" {
		return "", fmt.Errorf("ingest: target share %d has no mount point configured", shareID)
	}

	if allowLocalSharePaths(ctx, store) {
		info, err := os.Stat(s.MountPoint)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("ingest: target share %d mount point %s is not an existing directory", shareID, s.MountPoint)
		}
		return s.MountPoint, nil
	}

	mounted, err := share.IsMounted(s.MountPoint)
	if err != nil {
		return "", fmt.Errorf("ingest: target share %d: %w", shareID, err)
	}
	if !mounted {
		return "", fmt.Errorf("ingest: target share %d is not mounted at %s", shareID, s.MountPoint)
	}
	return s.MountPoint, nil
}

func allowLocalSharePaths(ctx context.Context, store *db.DB) bool {
	raw, err := store.SettingsGet(ctx, allowLocalSharePathsKey)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == "true"
}

// Run ingests every candidate segment (SourceID, optionally narrowed to
// TripIDs) into targetMount, which the caller must already have resolved
// (see ResolveTargetMount) and confirmed is a real, writable destination.
// A per-segment failure (unreadable source, verify mismatch, ...) is
// logged and counted in Stats.SegmentsFailed rather than aborting the
// whole run — mirroring the thumbs job's "a few bad files shouldn't fail
// the batch" behavior — so callers should check SegmentsFailed rather than
// Run's error return to detect partial failure; a non-nil error means Run
// couldn't even get started (bad source/verify mode) or ctx was cancelled.
func Run(ctx context.Context, store *db.DB, targetMount string, opts Opts, report ReportFunc) (Stats, error) {
	verify := opts.Verify
	if verify == "" {
		verify = VerifySHA256
	}
	if verify != VerifySHA256 && verify != VerifySize {
		return Stats{}, fmt.Errorf("ingest: invalid verify mode %q", verify)
	}

	if _, err := store.SourceGet(ctx, opts.SourceID); err != nil {
		return Stats{}, fmt.Errorf("ingest: source %d: %w", opts.SourceID, err)
	}

	segs, err := store.IngestCandidates(ctx, opts.SourceID, opts.TripIDs)
	if err != nil {
		return Stats{}, fmt.Errorf("ingest: candidates: %w", err)
	}

	var bytesTotal int64
	for _, s := range segs {
		bytesTotal += s.Bytes
	}

	stats := Stats{SegmentsTotal: len(segs)}
	start := time.Now()
	var bytesDone int64

	report0 := func(done int, msg string) {
		if report != nil {
			report(done, len(segs), bytesDone, bytesTotal, msg)
		}
	}
	report0(0, "starting")

	for i, seg := range segs {
		if err := ctx.Err(); err != nil {
			stats.Duration = time.Since(start)
			return stats, err
		}

		onBytes := func(n int) {
			bytesDone += int64(n)
			report0(i, seg.RelPath)
		}

		skipped, ingestErr := ingestOne(ctx, store, seg, targetMount, opts.TargetShareID, verify, onBytes)
		switch {
		case ingestErr != nil:
			stats.SegmentsFailed++
			log.Printf("[ingest] segment %d (%s): %v", seg.ID, seg.RelPath, ingestErr)
		case skipped:
			stats.SegmentsSkipped++
			bytesDone += seg.Bytes
		default:
			stats.SegmentsCopied++
		}
		report0(i+1, seg.RelPath)
	}

	stats.BytesCopied = bytesDone
	stats.Duration = time.Since(start)
	if stats.Duration.Seconds() > 0 {
		stats.ThroughputMBs = float64(stats.BytesCopied) / (1024 * 1024) / stats.Duration.Seconds()
	}
	return stats, nil
}

// ingestOne copies (or dedupe-skips) one segment's video plus its sidecars
// into targetMount, returning skipped=true when a previous run already
// verified this exact destination.
func ingestOne(ctx context.Context, store *db.DB, seg db.IngestSegment, targetMount string, shareID int64, verify string, onBytes func(n int)) (skipped bool, err error) {
	srcPath := filepath.Join(seg.SourceRoot, seg.RelPath)
	destPath := filepath.Join(targetMount, seg.RelPath)

	if existing, gerr := store.IngestGet(ctx, seg.ID, shareID); gerr == nil && existing != nil && existing.VerifiedAt != nil {
		if verr := VerifyFile(destPath, existing.Bytes, existing.SHA256, verify); verr == nil {
			return true, nil
		}
		// Destination went missing or changed since it was last verified
		// (e.g. an operator touched the share) — fall through and
		// re-copy rather than trusting a stale verified row.
	}

	if _, statErr := os.Stat(srcPath); statErr != nil {
		return false, fmt.Errorf("source file: %w", statErr)
	}

	result, err := streamCopy(ctx, srcPath, destPath, onBytes)
	if err != nil {
		return false, err
	}

	if err := store.IngestRecordAttempt(ctx, seg.ID, shareID, seg.RelPath, result.Bytes, result.SHA256); err != nil {
		return false, fmt.Errorf("record attempt: %w", err)
	}

	if err := VerifyFile(destPath, result.Bytes, result.SHA256, verify); err != nil {
		return false, fmt.Errorf("post-copy verify: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.IngestMarkVerified(ctx, seg.ID, shareID, now); err != nil {
		return false, fmt.Errorf("mark verified: %w", err)
	}
	if err := store.SegmentSetSHA256(ctx, seg.ID, result.SHA256); err != nil {
		log.Printf("[ingest] segment %d: set sha256: %v", seg.ID, err)
	}

	base := strings.TrimSuffix(seg.RelPath, ".mp4")
	for _, ext := range sidecarExts {
		s := filepath.Join(seg.SourceRoot, base+ext)
		d := filepath.Join(targetMount, base+ext)
		if err := copySidecarBestEffort(ctx, s, d); err != nil {
			log.Printf("[ingest] segment %d: sidecar %s: %v", seg.ID, ext, err)
		}
	}

	return false, nil
}
