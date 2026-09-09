package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"dashcam/internal/db"
)

// PruneOpts configures Prune.
type PruneOpts struct {
	// DryRun, when true, computes and returns the same counts a real run
	// would delete, but touches neither the database nor the filesystem.
	DryRun bool
	// Vacuum, when true (and !DryRun), runs PRAGMA wal_checkpoint(TRUNCATE)
	// followed by VACUUM after the deletes commit.
	Vacuum bool
}

// PruneResult is the JSON shape returned as the db_prune job's result.
type PruneResult struct {
	DryRun bool `json:"dry_run"`

	// SegmentsRemoved is the count of non-archived segments whose media
	// file was missing on disk and were deleted (cascading, per
	// schema.go's ON DELETE CASCADE FKs, to their detections/det_objects/
	// det_plates/det_vehicles/det_tracks/segment_summary/segment_gps/
	// detections_raw/search_terms rows).
	SegmentsRemoved int64 `json:"segments_removed"`

	OrphanDetectionsRemoved  int64 `json:"orphan_detections_removed"`
	OrphanDetObjectsRemoved  int64 `json:"orphan_det_objects_removed"`
	OrphanDetPlatesRemoved   int64 `json:"orphan_det_plates_removed"`
	OrphanDetVehiclesRemoved int64 `json:"orphan_det_vehicles_removed"`
	OrphanDetTracksRemoved   int64 `json:"orphan_det_tracks_removed"`

	OrphanThumbFilesRemoved int64 `json:"orphan_thumb_files_removed"`

	WALCheckpointed bool `json:"wal_checkpointed"`
	Vacuumed        bool `json:"vacuumed"`

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
// thumbnail/sprite files with no matching segment. dry_run computes the
// same counts without deleting anything. All database deletes run inside a
// single write transaction so a mid-run failure leaves the database
// exactly as it was; the optional wal_checkpoint/VACUUM run afterward, on
// the same write connection, once the delete transaction has committed —
// readers on the read pool keep working against WAL throughout (VACUUM
// only ever touches the single write connection, never taking a lock the
// read pool needs).
func Prune(ctx context.Context, store *db.DB, dbPath, cacheDir string, opts PruneOpts) (PruneResult, error) {
	res := PruneResult{DryRun: opts.DryRun}

	missing, err := findMissingMediaSegments(ctx, store)
	if err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: missing media: %w", err)
	}
	res.SegmentsRemoved = int64(len(missing))

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

	if opts.DryRun {
		return res, nil
	}

	if err := store.Tx(ctx, func(tx *sql.Tx) error {
		if len(missing) > 0 {
			if err := deleteSegmentsByID(ctx, tx, missing); err != nil {
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

	var removedFiles int64
	for _, path := range orphanThumbs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return PruneResult{}, fmt.Errorf("maintenance: prune: remove %s: %w", path, err)
		}
		removedFiles++
	}
	res.OrphanThumbFilesRemoved = removedFiles

	if _, err := store.Write().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return PruneResult{}, fmt.Errorf("maintenance: prune: wal_checkpoint: %w", err)
	}
	res.WALCheckpointed = true

	if opts.Vacuum {
		vacCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if _, err := store.Write().ExecContext(vacCtx, "VACUUM"); err != nil {
			return PruneResult{}, fmt.Errorf("maintenance: prune: vacuum: %w", err)
		}
		res.Vacuumed = true
	}

	return res, nil
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
		chunk := ids[i:end]

		placeholders := make([]byte, 0, len(chunk)*2)
		args := make([]any, len(chunk))
		for j, id := range chunk {
			if j > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args[j] = id
		}
		stmt := fmt.Sprintf(`DELETE FROM segments WHERE id IN (%s)`, string(placeholders))
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}
