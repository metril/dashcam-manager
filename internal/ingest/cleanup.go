package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dashcam/internal/db"
)

// CleanupOpts configures a Cleanup call.
type CleanupOpts struct {
	// SourceID is required: cleanup only ever makes sense against one
	// specific scanned source (normally the SSD), never implicitly every
	// source in the DB.
	SourceID int64
	// TripIDs restricts cleanup to these trips; empty means every
	// verified segment of SourceID ("all verified" per the M10 brief).
	TripIDs []int64
	// SegmentIDs, when non-empty, restricts cleanup to exactly these
	// segment ids (further narrowing TripIDs, if both are given) — used by
	// the ingest job's delete_after path to scope cleanup to precisely the
	// segments that job itself just verified, rather than every verified
	// segment across the requested trips (which could include segments
	// verified by an earlier, unrelated run).
	SegmentIDs []int64
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
	// Note explains a dry-run's limits: it checks existence and size only.
	Note string `json:"note,omitempty"`
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
	// Per-reason skip counters. MissingOnSSD is informational (nothing
	// left to delete) and is not part of SegmentsSkipped; the rest sum to
	// SegmentsSkipped.
	MissingOnSSD   int `json:"missing_on_ssd"`
	SizeMismatch   int `json:"size_mismatch"`
	DestMismatch   int `json:"dest_mismatch"`
	DestUnresolved int `json:"dest_unresolved"`
	UnsafePath     int `json:"unsafe_path"`
	RemoveFailed   int `json:"remove_failed"`
	StatFailed     int `json:"stat_failed"`
	// RecorderDBRowsDeleted counts recorder dashcam.db segment rows pruned
	// after deleting files; RecorderDBError is set when pruning failed
	// (the file deletions still stand).
	RecorderDBRowsDeleted int    `json:"recorder_db_rows_deleted"`
	RecorderDBError       string `json:"recorder_db_error,omitempty"`
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
// excludes unverified segments entirely). Never touches logs/ or
// training/; the recorder's dashcam.db is only pruned of rows for segments
// removed (or already gone) in this run, see PruneRecorderDB. Only files reachable via a segment's own
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
	if opts.SourceID == 0 {
		return CleanupStats{}, fmt.Errorf("cleanup: source_id is required")
	}

	candidates, err := store.CleanupCandidates(ctx, opts.SourceID, opts.TripIDs, opts.SegmentIDs)
	if err != nil {
		return CleanupStats{}, fmt.Errorf("cleanup: candidates: %w", err)
	}

	stats := CleanupStats{DryRun: opts.DryRun}
	if opts.DryRun {
		stats.Note = "Preview checks existence and size only; sha256 verification happens when cleanup runs."
	}

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

	// dirsToCheck maps a recording directory to the source root it's under
	// (see removeIfEmpty's containment check) — every candidate here
	// shares opts.SourceID, but a segment's own db.CleanupCandidate.SourceRoot
	// is used rather than assuming they're all identical, since that's
	// what's actually authoritative per-row.
	dirsToCheck := map[string]string{}
	// pruneByRoot collects rel paths whose recorder DB rows are now stale:
	// files deleted this run plus ones already missing on the SSD.
	pruneByRoot := map[string][]string{}

	for i, c := range candidates {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if report != nil {
			report(i, len(candidates), c.RelPath)
		}

		// Defense in depth, mirroring ingestOne's guard: refuse to touch
		// anything a corrupted or crafted rel_path/dest_rel_path could
		// point outside the intended source/share tree via ".." or an
		// absolute path.
		if !filepath.IsLocal(c.RelPath) || !filepath.IsLocal(c.DestRelPath) {
			stats.SegmentsSkipped++
			stats.UnsafePath++
			log.Printf("[cleanup] segment %d: unsafe path (rel_path=%q dest_rel_path=%q), skipping", c.SegmentID, c.RelPath, c.DestRelPath)
			continue
		}

		srcPath := filepath.Join(c.SourceRoot, c.RelPath)
		info, statErr := os.Stat(srcPath)
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			// Unknown state (I/O error, permissions, ...): don't claim
			// the file is gone and don't prune its recorder rows.
			stats.SegmentsSkipped++
			stats.StatFailed++
			log.Printf("[cleanup] segment %d: stat_failed %s: %v", c.SegmentID, srcPath, statErr)
			continue
		}
		if statErr != nil {
			// Already gone (e.g. a previous cleanup run got partway
			// through and was interrupted) — nothing left to delete, and
			// definitely not an error worth failing the whole run over.
			stats.MissingOnSSD++
			dirsToCheck[filepath.Dir(srcPath)] = c.SourceRoot
			pruneByRoot[c.SourceRoot] = append(pruneByRoot[c.SourceRoot], c.RelPath)
			log.Printf("[cleanup] segment %d: missing_on_ssd (%s), skipping", c.SegmentID, srcPath)
			continue
		}
		if info.Size() != c.IngestBytes {
			// The SSD original itself doesn't match what was ingested —
			// exceedingly unlikely (the SSD is read-only in normal
			// operation) but refuse rather than delete something that
			// doesn't match the verified copy's provenance.
			stats.SegmentsSkipped++
			stats.SizeMismatch++
			log.Printf("[cleanup] segment %d: size_mismatch (ssd %d, ingested %d), skipping", c.SegmentID, info.Size(), c.IngestBytes)
			continue
		}

		destPath, err := destPathFor(ctx, store, c)
		if err != nil {
			stats.SegmentsSkipped++
			stats.DestUnresolved++
			log.Printf("[cleanup] segment %d: dest_unresolved: %v", c.SegmentID, err)
			continue
		}
		// Re-verify against sha256 when the original ingest recorded one
		// (Opts.Verify == VerifySHA256), not just size — a same-sized but
		// bit-rotted or otherwise corrupted destination file would
		// otherwise pass this check and the SSD's only good copy would be
		// deleted out from under it. A dry-run preview only checks
		// existence + size (hashing every candidate synchronously in an HTTP
		// request takes minutes for tens of GB); the real job hashes.
		verifyMode := VerifySize
		if c.IngestSHA256 != "" && !opts.DryRun {
			verifyMode = VerifySHA256
		}
		if verr := VerifyFile(destPath, c.IngestBytes, c.IngestSHA256, verifyMode); verr != nil {
			// Destination no longer matches the verified copy (moved,
			// truncated, deleted, corrupted) — refuse to delete the only
			// remaining copy.
			stats.SegmentsSkipped++
			stats.DestMismatch++
			log.Printf("[cleanup] segment %d: dest_mismatch: %v", c.SegmentID, verr)
			continue
		}

		if opts.DryRun {
			stats.add(CleanupItem{SegmentID: c.SegmentID, RelPath: c.RelPath, Bytes: c.Bytes})
			continue
		}

		if err := os.Remove(srcPath); err != nil {
			stats.SegmentsSkipped++
			stats.RemoveFailed++
			log.Printf("[cleanup] remove_failed %s: %v", srcPath, err)
			continue
		}
		removeSidecars(c.SourceRoot, destPath, c.RelPath)
		stats.add(CleanupItem{SegmentID: c.SegmentID, RelPath: c.RelPath, Bytes: c.Bytes})
		dirsToCheck[filepath.Dir(srcPath)] = c.SourceRoot
		pruneByRoot[c.SourceRoot] = append(pruneByRoot[c.SourceRoot], c.RelPath)
	}

	if !opts.DryRun {
		// Deepest first so a recording dir is gone before its parents are
		// examined; the map already dedupes.
		dirs := make([]string, 0, len(dirsToCheck))
		for dir := range dirsToCheck {
			dirs = append(dirs, dir)
		}
		sort.Slice(dirs, func(i, j int) bool {
			if len(dirs[i]) != len(dirs[j]) {
				return len(dirs[i]) > len(dirs[j])
			}
			return dirs[i] < dirs[j]
		})
		for _, dir := range dirs {
			stats.DirsRemoved += removeEmptyTree(dir, dirsToCheck[dir])
		}
	}

	if !opts.DryRun {
		for root, rels := range pruneByRoot {
			rs, err := PruneRecorderDB(ctx, root, rels)
			stats.RecorderDBRowsDeleted += rs.SegmentsDeleted
			if err != nil {
				log.Printf("[cleanup] prune recorder db under %s: %v", root, err)
				stats.RecorderDBError = err.Error()
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

// removeSidecars removes a segment's sidecar files (timestamp/detections/
// gps JSON) alongside its video — but only once each destination sidecar
// is re-confirmed (by size) to still match the SSD original being
// deleted; a sidecar whose destination is missing or a different size is
// left on the SSD rather than deleted, since ingestOne's copySidecars
// already guarantees every sidecar was verified at ingest time, so a
// mismatch here means the destination copy changed or vanished
// afterward — exactly the same reasoning Cleanup already applies to the
// video itself, just per-sidecar since sidecars aren't individually
// tracked in the `ingests` table. destVideoPath is the segment's already-
// resolved destination video path (see destPathFor); sidecar destinations
// are derived from it rather than re-resolving the share's mount point
// per sidecar.
func removeSidecars(sourceRoot, destVideoPath, relPath string) {
	srcBase := strings.TrimSuffix(relPath, ".mp4")
	destBase := strings.TrimSuffix(destVideoPath, ".mp4")
	for _, ext := range sidecarExts {
		srcPath := filepath.Join(sourceRoot, srcBase+ext)
		srcInfo, statErr := os.Stat(srcPath)
		if statErr != nil {
			continue // no such sidecar on the SSD to begin with
		}

		destInfo, destErr := os.Stat(destBase + ext)
		if destErr != nil || destInfo.Size() != srcInfo.Size() {
			log.Printf("[cleanup] sidecar %s: destination missing or size mismatch, leaving SSD original in place", srcPath)
			continue
		}

		if err := os.Remove(srcPath); err != nil && !os.IsNotExist(err) {
			log.Printf("[cleanup] remove sidecar %s: %v", srcPath, err)
		}
	}
}

// recordingDirPrefix is the directory-name prefix removeIfEmpty requires
// before it will ever remove a directory — matches the recorder's
// recording_YYYYMMDD_HHMMSS layout (internal/scan.ParseSegmentPath).
const recordingDirPrefix = "recording_"

// removeIfEmpty removes dir if it's now empty, reporting whether it did.
// Two guards keep this from ever reaching outside what a cleanup run is
// supposed to touch: dir's base name must look like a recording directory
// (recordingDirPrefix), and dir must resolve strictly below sourceRoot
// (filepath.Rel must not climb out via ".." and must not equal sourceRoot
// itself) — so neither a surprising RelPath shape nor a future bug in a
// caller can walk this up into removing sourceRoot's front/rear/date
// directories, or anything above them. Never recurses upward beyond dir
// itself even when both guards pass.
func removeIfEmpty(dir, sourceRoot string) bool {
	if !strings.HasPrefix(filepath.Base(dir), recordingDirPrefix) {
		return false
	}
	rel, err := filepath.Rel(sourceRoot, dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return false
	}

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

// isDigits reports whether s is non-empty and all ASCII digits (YYYY, MM, DD).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// removeEmptyTree removes the recording dir (see removeIfEmpty) and then
// walks up removing parents while they are empty, all-digit named (the
// YYYY/MM/DD layers) and strictly below sourceRoot. It stops at the camera
// dir, which is never all digits in practice and is guarded by the walk
// stopping at the first non-digit name. Returns the number of dirs removed.
func removeEmptyTree(dir, sourceRoot string) int {
	if !removeIfEmpty(dir, sourceRoot) {
		return 0
	}
	n := 1
	for p := filepath.Dir(dir); isDigits(filepath.Base(p)); p = filepath.Dir(p) {
		rel, err := filepath.Rel(sourceRoot, p)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			break
		}
		entries, err := os.ReadDir(p)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := os.Remove(p); err != nil {
			log.Printf("[cleanup] remove empty dir %s: %v", p, err)
			break
		}
		n++
	}
	return n
}

// PreviewFor is a convenience wrapper around Cleanup with DryRun forced
// on, for the ingest/cleanup HTTP handler's dry_run path (which the M10
// brief asks to answer synchronously rather than round-tripping through
// the job queue for something that never touches disk).
func PreviewFor(ctx context.Context, store *db.DB, sourceID int64, tripIDs []int64) (CleanupStats, error) {
	return Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, TripIDs: tripIDs, DryRun: true}, nil)
}
