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
	// BytesCopied is bytes actually streamed this run — it excludes
	// dedupe-skipped segments (see SegmentsSkipped), so ThroughputMBs
	// reflects real copy throughput rather than being inflated by
	// segments that cost near-zero wall time because nothing was copied.
	BytesCopied   int64
	Duration      time.Duration
	ThroughputMBs float64
	// VerifiedSegmentIDs is every segment that ended this Run verified —
	// copied-and-verified just now, or already-verified and dedupe-skipped
	// (excludes SegmentsFailed). Used by callers that want to scope a
	// follow-up action (e.g. delete_after cleanup) to exactly what this
	// run actually verified, rather than every candidate that was merely
	// requested.
	VerifiedSegmentIDs []int64
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

	if AllowLocalSharePaths(ctx, store) {
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

// AllowLocalSharePaths reports whether the allow_local_share_paths hidden
// setting is on (see allowLocalSharePathsKey). Exported so callers outside
// this package (e.g. cmd/api's ssd-source scan guard) can reuse the same
// escape hatch instead of re-reading the setting themselves.
func AllowLocalSharePaths(ctx context.Context, store *db.DB) bool {
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
	// bytesDone drives progress reporting (credits a dedupe-skipped
	// segment's full size immediately, so the progress bar still reaches
	// 100%); bytesCopied counts only bytes actually streamed this run and
	// becomes Stats.BytesCopied (see its doc comment) — kept as two
	// separate counters specifically so a run that's mostly dedupe-skips
	// doesn't report an inflated throughput.
	var bytesDone int64
	var bytesCopied int64

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
			bytesCopied += int64(n)
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
			stats.VerifiedSegmentIDs = append(stats.VerifiedSegmentIDs, seg.ID)
		default:
			stats.SegmentsCopied++
			stats.VerifiedSegmentIDs = append(stats.VerifiedSegmentIDs, seg.ID)
		}
		report0(i+1, seg.RelPath)
	}

	stats.BytesCopied = bytesCopied
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
	// rel_path comes from the DB (ultimately from a filesystem walk — see
	// internal/scan), but defense in depth costs nothing here: joining an
	// absolute or ".."-escaping rel_path onto sourceRoot/targetMount would
	// let a corrupted or maliciously-crafted row read from or write to
	// anywhere on disk instead of staying under the intended source/share
	// tree.
	if !filepath.IsLocal(seg.RelPath) {
		return false, fmt.Errorf("unsafe rel_path %q", seg.RelPath)
	}

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

	// Sidecars must land (and verify by size) before the segment is marked
	// verified: a caller — cleanup in particular — trusts a verified
	// `ingests` row to mean "the destination has everything this
	// segment's SSD original has", and cleanup's own sidecar removal
	// separately re-checks each sidecar's size, but that check only
	// protects a sidecar that copied cleanly in some *other* run; a
	// segment whose sidecar failed here must not be marked verified at
	// all, or a later ingest rerun would dedupe-skip it (VerifyFile only
	// checks the video) and its sidecar gap would never get another
	// chance to be fixed.
	if err := copySidecars(ctx, seg.SourceRoot, targetMount, seg.RelPath); err != nil {
		return false, fmt.Errorf("sidecars: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.IngestMarkVerified(ctx, seg.ID, shareID, now); err != nil {
		return false, fmt.Errorf("mark verified: %w", err)
	}
	if err := store.SegmentSetSHA256(ctx, seg.ID, result.SHA256); err != nil {
		log.Printf("[ingest] segment %d: set sha256: %v", seg.ID, err)
	}

	return false, nil
}
