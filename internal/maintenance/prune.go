package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dashcam/internal/db"
)

// recentlyModifiedWindow: a segment whose file is missing but whose
// recorded mtime (segments.mtime, the source file's mtime as of the last
// scan — see internal/scan/walker.go's MtimeUX) is within this window of
// "now" is left alone rather than deleted. This is the same kind of
// still-copying guard jobs_runtime.go's inputMaybeStillCopying uses for
// thumbnails: an in-flight ingest/scan can produce a segment row slightly
// ahead of its file actually landing on disk (or briefly moved/renamed
// mid-copy), and without this window a prune racing that window would
// permanently delete a segment that was never actually missing, just not
// finished yet.
const recentlyModifiedWindow = 10 * time.Minute

// plausibilityThresholdPercent / plausibilityThresholdCap: Prune refuses
// to apply when the segments it would remove exceed min(10% of the
// library, 500) — whichever is smaller — unless opts.Force is set. This
// is the safety net for "SSD unplugged" style failures that checkSources
// doesn't already catch (e.g. a genuinely mounted but somehow largely-
// empty source), not a replacement for it.
const (
	plausibilityThresholdPercent = 10
	plausibilityThresholdCap     = 500
)

func plausibilityThreshold(totalSegments int64) int64 {
	pct := totalSegments * plausibilityThresholdPercent / 100
	if pct > plausibilityThresholdCap {
		return plausibilityThresholdCap
	}
	return pct
}

// PruneOpts configures Prune.
type PruneOpts struct {
	// Apply must be explicitly true to perform any deletion; the zero
	// value (false) always computes and returns the same counts a real
	// run would delete without touching the database or filesystem — an
	// absent/empty request body must default to a safe dry run, never a
	// destructive one.
	Apply bool
	// Vacuum, when true (and Apply), runs PRAGMA wal_checkpoint(TRUNCATE)
	// followed by VACUUM after the deletes commit — skipped (with
	// VacuumSkippedReason set) if the database's filesystem doesn't have
	// at least 2x the database's size free.
	Vacuum bool
	// Force bypasses the plausibility-threshold abort below. It does NOT
	// bypass the source-availability refusal (see checkSources) — that
	// one is never overridable, since running anyway is exactly the
	// "SSD unplugged, prune wipes the library" failure mode this whole
	// fix round exists to prevent.
	Force bool
}

// PruneResult is the JSON shape returned as the db_prune job's result.
type PruneResult struct {
	DryRun bool `json:"dry_run"`

	// SegmentsRemoved is the count of non-archived segments whose media
	// file was missing on disk and were deleted (cascading, per
	// schema.go's ON DELETE CASCADE FKs, to their detections/det_objects/
	// det_plates/det_vehicles/det_tracks/segment_summary/segment_gps/
	// detections_raw/search_terms rows). For a dry run this is a preview
	// count (re-checked against the read pool, same filters as a real
	// run would apply); for a real run it's the count actually deleted
	// after the authoritative in-transaction recheck (see
	// filterMissingCandidates) — the two can differ slightly if a file
	// appeared or a segment's mtime crossed recentlyModifiedWindow
	// between the preview and the delete.
	SegmentsRemoved int64 `json:"segments_removed"`
	// SegmentsSkippedRecentlyModified counts missing-media candidates
	// left alone because their recorded mtime is within
	// recentlyModifiedWindow of now (see its doc comment) — not deleted,
	// not an error, just deferred to a later prune run.
	SegmentsSkippedRecentlyModified int64 `json:"segments_skipped_recently_modified,omitempty"`

	OrphanDetectionsRemoved  int64 `json:"orphan_detections_removed"`
	OrphanDetObjectsRemoved  int64 `json:"orphan_det_objects_removed"`
	OrphanDetPlatesRemoved   int64 `json:"orphan_det_plates_removed"`
	OrphanDetVehiclesRemoved int64 `json:"orphan_det_vehicles_removed"`
	OrphanDetTracksRemoved   int64 `json:"orphan_det_tracks_removed"`

	OrphanThumbFilesRemoved int64 `json:"orphan_thumb_files_removed"`

	WALCheckpointed bool `json:"wal_checkpointed"`
	Vacuumed        bool `json:"vacuumed"`
	// VacuumSkippedReason is set (Vacuum requested, Apply true) when
	// VACUUM did NOT run — insufficient free space, or a failure while
	// checking free space.
	VacuumSkippedReason string `json:"vacuum_skipped_reason,omitempty"`

	// SegmentsMissingDetectionsRaw is reported but never deleted — an
	// incompletely-indexed segment is a valid row waiting on a re-index
	// (e.g. via index_detections), not an orphan; see report.go's
	// countSegmentsMissingDetectionsRaw.
	SegmentsMissingDetectionsRaw int64 `json:"segments_missing_detections_raw"`
}

// Prune deletes exactly what Verify would report as orphaned/missing (see
// report.go): non-archived segments whose media file is gone (cascading
// away their dependent rows), det_objects/det_plates/det_vehicles/
// det_tracks/detections rows with no live parent, and cached
// thumbnail/sprite files with no matching segment. !opts.Apply computes
// the same counts without deleting anything.
//
// Refuses to run at all (returns an error, no reads/writes beyond the
// availability check itself) if any source's root_path isn't currently
// mounted/present — see checkSources' doc comment for why. When
// opts.Apply, also refuses (error, nothing deleted) if the missing-media
// segment count exceeds plausibilityThreshold, unless opts.Force.
//
// All database deletes run inside a single write transaction so a
// mid-run failure leaves the database exactly as it was; the optional
// wal_checkpoint/VACUUM run afterward, on the same write connection, once
// the delete transaction has committed — readers on the read pool keep
// working against WAL throughout (VACUUM only ever touches the single
// write connection, never taking a lock the read pool needs, though it
// does serialize behind that connection's own writes for its duration —
// see the "vacuum blocks writes" note surfaced in the job message).
func Prune(ctx context.Context, store *db.DB, dbPath, cacheDir string, opts PruneOpts) (PruneResult, error) {
	res := PruneResult{DryRun: !opts.Apply}

	sources, err := checkSources(ctx, store)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: %w", err)
	}
	available, unavailable := splitSources(sources)
	if len(unavailable) > 0 {
		return PruneResult{}, fmt.Errorf(
			"maintenance: prune: refusing to run — source(s) not mounted/available (running anyway would misclassify their segments' media as missing and delete them): %s",
			strings.Join(unavailable, "; "))
	}

	candidates, err := findMissingMediaSegments(ctx, store, available)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: missing media: %w", err)
	}

	// Preview via the read pool: same filters a real run's in-tx recheck
	// applies, just not authoritative (nothing is locked against
	// concurrent change). Good enough for a dry-run's reported counts and
	// for the plausibility-threshold check below.
	previewConfirmed, skippedRecent, err := filterMissingCandidates(ctx, store.Read(), candidates)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: preview missing media: %w", err)
	}
	res.SegmentsRemoved = int64(len(previewConfirmed))
	res.SegmentsSkippedRecentlyModified = skippedRecent

	counts, err := countOrphans(ctx, store)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: orphans: %w", err)
	}
	res.OrphanDetectionsRemoved = counts.detections
	res.OrphanDetObjectsRemoved = counts.detObjects
	res.OrphanDetPlatesRemoved = counts.detPlates
	res.OrphanDetVehiclesRemoved = counts.detVehicles
	res.OrphanDetTracksRemoved = counts.detTracks

	rawMissing, err := countSegmentsMissingDetectionsRaw(ctx, store)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: missing detections_raw: %w", err)
	}
	res.SegmentsMissingDetectionsRaw = rawMissing

	orphanThumbs, err := findOrphanThumbFiles(ctx, store, cacheDir)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: orphan thumb files: %w", err)
	}
	res.OrphanThumbFilesRemoved = int64(len(orphanThumbs))

	if !opts.Apply {
		return res, nil
	}

	if !opts.Force {
		total, err := countAllSegments(ctx, store)
		if err != nil {
			return PruneResult{}, fmt.Errorf("maintenance: prune: count segments: %w", err)
		}
		threshold := plausibilityThreshold(total)
		if int64(len(previewConfirmed)) > threshold {
			return PruneResult{}, fmt.Errorf(
				"maintenance: prune: refusing to apply — %d segments would be removed as missing-media, exceeding the plausibility threshold of %d (min of %d%% of %d total segments and %d); this usually means a source looks emptier than it should (e.g. a mostly-unmounted share) rather than genuinely missing files — pass force:true to override if this is expected",
				len(previewConfirmed), threshold, plausibilityThresholdPercent, total, plausibilityThresholdCap)
		}
	}

	var deletedSegments []int64
	if err := store.Tx(ctx, func(tx *sql.Tx) error {
		// Authoritative recheck: re-verify file-existence (and the
		// recently-modified skip) against the live filesystem from
		// inside this transaction, immediately before deleting — closes
		// the race where a file legitimately appears (an in-flight
		// ingest/scan completes) between the preview above and now.
		confirmed, _, err := filterMissingCandidates(ctx, tx, candidates)
		if err != nil {
			return fmt.Errorf("recheck missing media: %w", err)
		}
		deletedSegments = confirmed
		if len(confirmed) > 0 {
			if err := deleteSegmentsByID(ctx, tx, confirmed); err != nil {
				return fmt.Errorf("delete missing-media segments: %w", err)
			}
		}

		deletes := []string{
			orphanDetectionsDeleteSQL,
			orphanDetObjectsDeleteSQL,
			orphanDetPlatesDeleteSQL,
			orphanDetVehiclesDeleteSQL,
			orphanDetTracksDeleteSQL,
		}
		for _, sqlStmt := range deletes {
			if _, err := tx.ExecContext(ctx, sqlStmt); err != nil {
				return fmt.Errorf("delete orphans: %w", err)
			}
		}
		return nil
	}); err != nil {
		return PruneResult{}, err
	}
	res.SegmentsRemoved = int64(len(deletedSegments))

	var removedFiles int64
	for _, path := range orphanThumbs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return PruneResult{}, fmt.Errorf("maintenance: prune: remove %s: %w", path, err)
		}
		removedFiles++
	}
	res.OrphanThumbFilesRemoved = removedFiles

	// PRAGMA wal_checkpoint(TRUNCATE) returns one row (busy, log,
	// checkpointed) rather than signaling failure only via an error —
	// busy=1 means another connection blocked the checkpoint (e.g. a
	// long-running read holding the WAL back), which is not itself a
	// query error, so it must be read from the row rather than assumed
	// from a nil Exec error.
	var busy, walFrames, checkpointedFrames int
	if err := store.Write().QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &walFrames, &checkpointedFrames); err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: wal_checkpoint: %w", err)
	}
	res.WALCheckpointed = busy == 0

	if opts.Vacuum {
		ok, free, needed, spaceErr := hasEnoughFreeSpaceForVacuum(dbPath)
		switch {
		case spaceErr != nil:
			res.VacuumSkippedReason = fmt.Sprintf("could not check free space: %v", spaceErr)
		case !ok:
			res.VacuumSkippedReason = fmt.Sprintf("insufficient free space: %d bytes free, need ~%d (2x database size)", free, needed)
		default:
			vacCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			if _, err := store.Write().ExecContext(vacCtx, "VACUUM"); err != nil {
				return PruneResult{}, fmt.Errorf("maintenance: prune: vacuum: %w", err)
			}
			res.Vacuumed = true
		}
	}

	return res, nil
}

// filterMissingCandidates re-checks each of candidateIDs against the live
// filesystem (via q — either store.Read() for an advisory preview, or the
// delete transaction itself for the authoritative recheck immediately
// before deleting) and drops:
//   - any id whose file now exists (it was never really gone, or an
//     in-flight copy has since landed) — not counted anywhere, just
//     excluded, since it's not a stale "missing" finding at all;
//   - any id whose recorded mtime is missing or within
//     recentlyModifiedWindow of now (skippedRecent) — plausibly still
//     mid-ingest/mid-copy rather than genuinely gone.
//
// q is deliberately typed as the minimal interface both *sql.DB and
// *sql.Tx satisfy, so the exact same logic backs both the preview and the
// in-transaction recheck.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func filterMissingCandidates(ctx context.Context, q queryer, candidateIDs []int64) (confirmed []int64, skippedRecent int64, err error) {
	if len(candidateIDs) == 0 {
		return nil, 0, nil
	}

	const chunkSize = 500
	now := time.Now().Unix()
	for i := 0; i < len(candidateIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		chunk := candidateIDs[i:end]

		placeholders, args := idPlaceholders(chunk)
		rows, err := q.QueryContext(ctx, fmt.Sprintf(`
			SELECT s.id, src.root_path, s.rel_path, s.mtime
			FROM segments s
			JOIN sources src ON src.id = s.source_id
			WHERE s.id IN (%s)`, placeholders), args...)
		if err != nil {
			return nil, 0, err
		}

		scanErr := func() error {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var root, rel string
				var mtime sql.NullInt64
				if err := rows.Scan(&id, &root, &rel, &mtime); err != nil {
					return err
				}
				if _, statErr := os.Stat(filepath.Join(root, rel)); statErr == nil {
					continue // file exists now — not missing, don't delete
				} else if !os.IsNotExist(statErr) {
					continue // ambiguous stat error (permission, I/O) — don't delete on unclear evidence
				}
				if !mtime.Valid || now-mtime.Int64 < int64(recentlyModifiedWindow.Seconds()) {
					skippedRecent++
					continue
				}
				confirmed = append(confirmed, id)
			}
			return rows.Err()
		}()
		if scanErr != nil {
			return nil, 0, scanErr
		}
	}
	return confirmed, skippedRecent, nil
}

func idPlaceholders(ids []int64) (string, []any) {
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = id
	}
	return string(placeholders), args
}

// hasEnoughFreeSpaceForVacuum reports whether the filesystem holding
// dbPath has at least 2x the database's current size free — VACUUM
// rewrites the whole database into a fresh file before replacing the
// original, so it needs roughly that much headroom, and running it
// without enough free space risks failing mid-rewrite (or filling the
// disk entirely, taking down everything else that writes to it).
func hasEnoughFreeSpaceForVacuum(dbPath string) (ok bool, freeBytes, neededBytes int64, err error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return false, 0, 0, err
	}
	needed := info.Size() * 2

	var stat syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(dbPath), &stat); err != nil {
		return false, 0, needed, err
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return free >= needed, free, needed, nil
}

// deleteSegmentsByID deletes rows from segments in chunks (sqlite's
// default SQLITE_MAX_VARIABLE_NUMBER-adjacent caution — chunking rather
// than relying on a single huge IN() list) — cascading, per schema.go's ON
// DELETE CASCADE foreign keys, to every dependent table.
func deleteSegmentsByID(ctx context.Context, tx *sql.Tx, ids []int64) error {
	const chunkSize = 500
	for i := 0; i < len(ids); i += chunkSize {
		end := i + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders, args := idPlaceholders(ids[i:end])
		stmt := fmt.Sprintf(`DELETE FROM segments WHERE id IN (%s)`, placeholders)
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}
