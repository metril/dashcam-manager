package ingest

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"dashcam/internal/db"
)

// CleanupOpts configures a Cleanup call.
type CleanupOpts struct {
	// TripIDs restricts cleanup to these trips; empty means every
	// verified segment across every source ("all verified" per the M10
	// brief).
	TripIDs []int64
	// DryRun, when true, computes and returns exactly what would be
	// deleted without deleting anything.
	DryRun bool
}

// CleanupItem is one segment Cleanup did (or, in dry-run mode, would)
// delete the SSD original of.
type CleanupItem struct {
	SegmentID int64  `json:"segment_id"`
	RelPath   string `json:"rel_path"`
	Bytes     int64  `json:"bytes"`
}

// CleanupStats summarizes one Cleanup call.
type CleanupStats struct {
	// DryRun mirrors CleanupOpts.DryRun, so callers/serializers don't need
	// to also thread the option through.
	DryRun bool `json:"dry_run"`
	// Items is every segment deleted (or, in dry-run mode, that would be
	// deleted).
	Items []CleanupItem `json:"items"`
	// SegmentsDeleted/BytesDeleted total Items (kept as separate fields,
	// alongside Items, since a caller displaying just a summary shouldn't
	// need to re-sum the list itself).
	SegmentsDeleted int   `json:"segments_deleted"`
	BytesDeleted    int64 `json:"bytes_deleted"`
	// SegmentsSkipped counts verified candidates whose destination no
	// longer matches what was recorded at verify time (deleted, resized,
	// ...) — refused rather than deleted, since the copy backing them can
	// no longer be trusted.
	SegmentsSkipped int `json:"segments_skipped"`
	DirsRemoved     int `json:"dirs_removed"`
}

func (s *CleanupStats) add(it CleanupItem) {
	s.Items = append(s.Items, it)
	s.SegmentsDeleted++
	s.BytesDeleted += it.Bytes
}

// Cleanup deletes SSD originals for every segment with a verified
// `ingests` row (optionally narrowed to opts.TripIDs), but only once the
// destination file is re-confirmed to still exist with the same size that
// was recorded when it was verified — an unverified segment, or one whose
// destination no longer matches, is left untouched rather than deleted
// (candidates are selected via db.CleanupCandidates, which already
// excludes unverified segments entirely). Never touches dashcam.db,
// logs/, or training/: only files reachable via a segment's own
// SourceRoot+RelPath (always under a source's scanned root, i.e. the
// front/ or rear/ subtree) are ever removed.
//
// A non-DryRun call write-probes every distinct source root among the
// candidates before deleting anything, so a read-only-mounted SSD (the
// default until an operator deliberately switches it to rw — see
// WriteProbe and the M10 brief) fails fast with a clear error instead of
// deleting a handful of files before hitting a permission error midway
// through.
func Cleanup(ctx context.Context, store *db.DB, opts CleanupOpts, report func(done, total int, msg string)) (CleanupStats, error) {
	candidates, err := store.CleanupCandidates(ctx, opts.TripIDs)
	if err != nil {
		return CleanupStats{}, fmt.Errorf("cleanup: candidates: %w", err)
	}

	stats := CleanupStats{DryRun: opts.DryRun}

	if !opts.DryRun {
		roots := map[string]bool{}
		for _, c := range candidates {
			roots[c.SourceRoot] = true
		}
		for root := range roots {
			if err := WriteProbe(root); err != nil {
				return CleanupStats{}, fmt.Errorf("cleanup: %w", err)
			}
		}
	}

	dirsToCheck := map[string]bool{}

	for i, c := range candidates {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if report != nil {
			report(i, len(candidates), c.RelPath)
		}

		srcPath := filepath.Join(c.SourceRoot, c.RelPath)
		info, statErr := os.Stat(srcPath)
		if statErr != nil {
			// Already gone (e.g. a previous cleanup run got partway
			// through and was interrupted) — nothing left to delete, and
			// definitely not an error worth failing the whole run over.
			continue
		}
		if info.Size() != c.IngestBytes {
			// The SSD original itself doesn't match what was ingested —
			// exceedingly unlikely (the SSD is read-only in normal
			// operation) but refuse rather than delete something that
			// doesn't match the verified copy's provenance.
			stats.SegmentsSkipped++
			continue
		}

		destPath, err := destPathFor(ctx, store, c)
		if err != nil {
			stats.SegmentsSkipped++
			log.Printf("[cleanup] segment %d: %v", c.SegmentID, err)
			continue
		}
		if verr := VerifyFile(destPath, c.IngestBytes, c.IngestSHA256, VerifySize); verr != nil {
			// Destination no longer matches the verified copy (moved,
			// truncated, deleted) — refuse to delete the only remaining
			// copy.
			stats.SegmentsSkipped++
			continue
		}

		if opts.DryRun {
			stats.add(CleanupItem{SegmentID: c.SegmentID, RelPath: c.RelPath, Bytes: c.Bytes})
			continue
		}

		if err := os.Remove(srcPath); err != nil {
			stats.SegmentsSkipped++
			log.Printf("[cleanup] remove %s: %v", srcPath, err)
			continue
		}
		removeSidecars(c.SourceRoot, c.RelPath)
		stats.add(CleanupItem{SegmentID: c.SegmentID, RelPath: c.RelPath, Bytes: c.Bytes})
		dirsToCheck[filepath.Dir(srcPath)] = true
	}

	if !opts.DryRun {
		for dir := range dirsToCheck {
			if removeIfEmpty(dir) {
				stats.DirsRemoved++
			}
		}
	}

	return stats, nil
}

// destPathFor resolves the share mount point for c.ShareID and joins it
// with c.DestRelPath. A share cache would save repeated lookups for a
// large batch, but Cleanup's candidate lists are bounded by how much a
// user selects to clean up at once, so the extra ShareGet round trips
// aren't worth the complexity.
func destPathFor(ctx context.Context, store *db.DB, c db.CleanupCandidate) (string, error) {
	s, err := store.ShareGet(ctx, c.ShareID)
	if err != nil {
		return "", fmt.Errorf("share %d: %w", c.ShareID, err)
	}
	if s.MountPoint == "" {
		return "", fmt.Errorf("share %d has no mount point", c.ShareID)
	}
	return filepath.Join(s.MountPoint, c.DestRelPath), nil
}

// removeSidecars best-effort removes a segment's sidecar files alongside
// its video (mirrors the sidecarExts copied by ingestOne).
func removeSidecars(sourceRoot, relPath string) {
	base := strings.TrimSuffix(relPath, ".mp4")
	for _, ext := range sidecarExts {
		p := filepath.Join(sourceRoot, base+ext)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("[cleanup] remove sidecar %s: %v", p, err)
		}
	}
}

// removeIfEmpty removes dir (a recording_* directory) if it's now empty,
// reporting whether it did. Never recurses upward beyond dir itself — a
// stray sibling file in the same recording (or the date/camera
// directories further up) is left alone rather than risking removal of
// something unrelated to this cleanup run.
func removeIfEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return false
	}
	if err := os.Remove(dir); err != nil {
		log.Printf("[cleanup] remove empty dir %s: %v", dir, err)
		return false
	}
	return true
}

// PreviewFor is a convenience wrapper around Cleanup with DryRun forced
// on, for the ingest/cleanup HTTP handler's dry_run path (which the M10
// brief asks to answer synchronously rather than round-tripping through
// the job queue for something that never touches disk).
func PreviewFor(ctx context.Context, store *db.DB, tripIDs []int64) (CleanupStats, error) {
	return Cleanup(ctx, store, CleanupOpts{TripIDs: tripIDs, DryRun: true}, nil)
}
