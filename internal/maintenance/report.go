// Package maintenance implements the "maintenance tooling" job bodies used
// by cmd/api's db_verify/db_prune jobs: a read-only consistency report
// (Verify) and a deletion pass over the same findings (Prune). Both work
// directly against store.Read()/Write() (like internal/scan does) rather
// than growing internal/db with a long tail of single-use query methods —
// this package owns the maintenance-specific SQL.
package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dashcam/internal/db"
)

// integrityCheckThreshold is the database file size below which Verify
// runs the slower, more thorough PRAGMA integrity_check instead of
// PRAGMA quick_check — cheap enough on a small library to always prefer
// the stronger check; above it, quick_check keeps a maintenance run from
// stalling the single write connection for minutes on a large database.
const integrityCheckThreshold = 2 << 30 // 2GB

// Report is the JSON shape persisted as job result / settings key
// "last_db_verify" and served by GET /api/maintenance/report.
type Report struct {
	RanAt string `json:"ran_at"`

	IntegrityCheckKind string   `json:"integrity_check_kind"` // "quick_check" or "integrity_check"
	IntegrityOK        bool     `json:"integrity_ok"`
	IntegrityMessages  []string `json:"integrity_messages,omitempty"`

	MissingMediaSegments   int64   `json:"missing_media_segments"`
	MissingMediaSegmentIDs []int64 `json:"missing_media_segment_ids,omitempty"`

	// SourcesUnavailable lists every source (label: reason) whose
	// root_path isn't currently mounted/present — see sources.go's
	// checkSources. When non-empty, MissingMediaSegments/
	// MissingMediaSegmentIDs above deliberately exclude that source's
	// segments (MissingMediaCheckIncomplete is set) rather than reporting
	// them as missing: an unmounted source's segments *look* like every
	// file vanished, which is misleading, not a real finding.
	SourcesUnavailable          []string `json:"sources_unavailable,omitempty"`
	MissingMediaCheckIncomplete bool     `json:"missing_media_check_incomplete,omitempty"`

	OrphanDetections  int64 `json:"orphan_detections"`
	OrphanDetObjects  int64 `json:"orphan_det_objects"`
	OrphanDetPlates   int64 `json:"orphan_det_plates"`
	OrphanDetVehicles int64 `json:"orphan_det_vehicles"`
	OrphanDetTracks   int64 `json:"orphan_det_tracks"`

	SegmentsMissingDetectionsRaw int64 `json:"segments_missing_detections_raw"`

	OrphanThumbFiles int64    `json:"orphan_thumb_files"`
	OrphanThumbPaths []string `json:"orphan_thumb_paths,omitempty"`

	DBSizeBytes   int64 `json:"db_size_bytes"`
	WALSizeBytes  int64 `json:"wal_size_bytes"`
	FreelistPages int64 `json:"freelist_pages"`
	PageCount     int64 `json:"page_count"`
	PageSizeBytes int64 `json:"page_size_bytes"`
}

// maxListed caps how many ids/paths a report includes inline, so a
// library with a huge number of orphans doesn't blow up the job result /
// settings row — the counts above are always exact regardless.
const maxListed = 200

// Verify runs every consistency check described in report.go's Report and
// returns the result. dbPath is the sqlite file's path (for size/WAL
// measurement — not available from *db.DB itself), cacheDir is
// DASHCAM_CACHE (for the orphan-thumbnail-file scan).
func Verify(ctx context.Context, store *db.DB, dbPath, cacheDir string) (Report, error) {
	rep := Report{RanAt: time.Now().UTC().Format(time.RFC3339)}

	if err := runIntegrityCheck(ctx, store, dbPath, &rep); err != nil {
		return Report{}, err
	}

	sources, err := checkSources(ctx, store)
	if err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: check sources: %w", err)
	}
	available, unavailable := splitSources(sources)
	rep.SourcesUnavailable = unavailable
	rep.MissingMediaCheckIncomplete = len(unavailable) > 0

	missing, err := findMissingMediaSegments(ctx, store, available)
	if err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: missing media: %w", err)
	}
	rep.MissingMediaSegments = int64(len(missing))
	rep.MissingMediaSegmentIDs = capInt64(missing, maxListed)

	counts, err := countOrphans(ctx, store)
	if err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: orphans: %w", err)
	}
	rep.OrphanDetections = counts.detections
	rep.OrphanDetObjects = counts.detObjects
	rep.OrphanDetPlates = counts.detPlates
	rep.OrphanDetVehicles = counts.detVehicles
	rep.OrphanDetTracks = counts.detTracks

	n, err := countSegmentsMissingDetectionsRaw(ctx, store)
	if err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: missing detections_raw: %w", err)
	}
	rep.SegmentsMissingDetectionsRaw = n

	orphanThumbs, err := findOrphanThumbFiles(ctx, store, cacheDir)
	if err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: orphan thumb files: %w", err)
	}
	rep.OrphanThumbFiles = int64(len(orphanThumbs))
	rep.OrphanThumbPaths = capStrings(orphanThumbs, maxListed)

	if err := statDBSizes(dbPath, &rep); err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: db size: %w", err)
	}
	if err := readPragmaSizes(ctx, store, &rep); err != nil {
		return Report{}, fmt.Errorf("maintenance: verify: pragmas: %w", err)
	}

	return rep, nil
}

func runIntegrityCheck(ctx context.Context, store *db.DB, dbPath string, rep *Report) error {
	kind := "quick_check"
	if info, err := os.Stat(dbPath); err == nil && info.Size() <= integrityCheckThreshold {
		kind = "integrity_check"
	}
	rep.IntegrityCheckKind = kind

	rows, err := store.Read().QueryContext(ctx, "PRAGMA "+kind)
	if err != nil {
		return fmt.Errorf("maintenance: %s: %w", kind, err)
	}
	defer rows.Close()

	var msgs []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return fmt.Errorf("maintenance: %s: scan: %w", kind, err)
		}
		if msg != "ok" {
			msgs = append(msgs, msg)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("maintenance: %s: %w", kind, err)
	}
	rep.IntegrityOK = len(msgs) == 0
	rep.IntegrityMessages = msgs
	return nil
}

// findMissingMediaSegments returns the ids of every non-archived segment,
// belonging to a source in availableSourceIDs, whose media file isn't
// found on disk. archived_at segments are deliberately excluded: their
// file being gone is already known and intentional (see schema.go's
// segments.archived_at comment), so re-reporting them as "missing" here
// would just be noise (and pruning them would be wrong — they're kept on
// purpose for provenance). A segment whose source isn't in
// availableSourceIDs (see checkSources/splitSources) is silently skipped
// rather than checked — its source isn't mounted, so "the file isn't
// where root_path says it is" tells us nothing about whether the file is
// actually gone.
func findMissingMediaSegments(ctx context.Context, store *db.DB, availableSourceIDs map[int64]bool) ([]int64, error) {
	rows, err := store.Read().QueryContext(ctx, `
		SELECT s.id, s.source_id, src.root_path, s.rel_path
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.archived_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id, sourceID int64
		var root, rel string
		if err := rows.Scan(&id, &sourceID, &root, &rel); err != nil {
			return nil, err
		}
		if !availableSourceIDs[sourceID] {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, rel)); os.IsNotExist(statErr) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// orphanCounts is the shared shape between countOrphans (Verify) and
// deleteOrphans (Prune) — every field's SQL is defined once in
// orphanQueries so the two can never drift apart.
type orphanCounts struct {
	detections, detObjects, detPlates, detVehicles, detTracks int64
}

// The count/delete SQL pairs below share the same WHERE clause by
// construction, so Verify's counts and Prune's deletes can never drift
// apart. Every *_id FK here is declared ON DELETE CASCADE in schema.go, so
// under normal operation these should always be empty; they exist as a
// safety net for rows written while foreign_keys enforcement was off, or a
// bug/migration that bypassed it.
const (
	orphanDetectionsCountSQL  = `SELECT COUNT(*) FROM detections d LEFT JOIN segments s ON s.id = d.segment_id WHERE s.id IS NULL`
	orphanDetectionsDeleteSQL = `DELETE FROM detections WHERE id IN (
		SELECT d.id FROM detections d LEFT JOIN segments s ON s.id = d.segment_id WHERE s.id IS NULL)`

	orphanDetObjectsCountSQL  = `SELECT COUNT(*) FROM det_objects o LEFT JOIN detections d ON d.id = o.det_id WHERE d.id IS NULL`
	orphanDetObjectsDeleteSQL = `DELETE FROM det_objects WHERE id IN (
		SELECT o.id FROM det_objects o LEFT JOIN detections d ON d.id = o.det_id WHERE d.id IS NULL)`

	orphanDetPlatesCountSQL  = `SELECT COUNT(*) FROM det_plates p LEFT JOIN detections d ON d.id = p.det_id WHERE d.id IS NULL`
	orphanDetPlatesDeleteSQL = `DELETE FROM det_plates WHERE id IN (
		SELECT p.id FROM det_plates p LEFT JOIN detections d ON d.id = p.det_id WHERE d.id IS NULL)`

	orphanDetVehiclesCountSQL  = `SELECT COUNT(*) FROM det_vehicles v LEFT JOIN detections d ON d.id = v.det_id WHERE d.id IS NULL`
	orphanDetVehiclesDeleteSQL = `DELETE FROM det_vehicles WHERE id IN (
		SELECT v.id FROM det_vehicles v LEFT JOIN detections d ON d.id = v.det_id WHERE d.id IS NULL)`

	orphanDetTracksCountSQL  = `SELECT COUNT(*) FROM det_tracks t LEFT JOIN segments s ON s.id = t.segment_id WHERE s.id IS NULL`
	orphanDetTracksDeleteSQL = `DELETE FROM det_tracks WHERE id IN (
		SELECT t.id FROM det_tracks t LEFT JOIN segments s ON s.id = t.segment_id WHERE s.id IS NULL)`

	segmentsMissingDetectionsRawSQL = `SELECT COUNT(*) FROM segments s
		WHERE s.has_detections = 1 AND NOT EXISTS (SELECT 1 FROM detections_raw r WHERE r.segment_id = s.id)`
)

func countOrphans(ctx context.Context, store *db.DB) (orphanCounts, error) {
	var c orphanCounts
	queries := []struct {
		sql string
		dst *int64
	}{
		{orphanDetectionsCountSQL, &c.detections},
		{orphanDetObjectsCountSQL, &c.detObjects},
		{orphanDetPlatesCountSQL, &c.detPlates},
		{orphanDetVehiclesCountSQL, &c.detVehicles},
		{orphanDetTracksCountSQL, &c.detTracks},
	}
	for _, q := range queries {
		if err := store.Read().QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return orphanCounts{}, err
		}
	}
	return c, nil
}

func countSegmentsMissingDetectionsRaw(ctx context.Context, store *db.DB) (int64, error) {
	var n int64
	err := store.Read().QueryRowContext(ctx, segmentsMissingDetectionsRawSQL).Scan(&n)
	return n, err
}

// countAllSegments is the denominator for Prune's plausibility-threshold
// check (see plausibilityThreshold in prune.go).
func countAllSegments(ctx context.Context, store *db.DB) (int64, error) {
	var n int64
	err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments`).Scan(&n)
	return n, err
}

// findOrphanThumbFiles walks cacheDir's thumbs/ and sprites/ subtrees and
// returns the paths of every file whose basename (id.jpg / id.vtt) names a
// segment id that doesn't exist in segments anymore.
func findOrphanThumbFiles(ctx context.Context, store *db.DB, cacheDir string) ([]string, error) {
	ids, err := allSegmentIDs(ctx, store)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, sub := range []string{"thumbs", "sprites"} {
		dir := filepath.Join(cacheDir, sub)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && path == dir {
					return nil // cache subtree doesn't exist yet — nothing to scan
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			base := filepath.Base(path)
			ext := filepath.Ext(base)
			idStr := base[:len(base)-len(ext)]
			id, convErr := parseSegmentID(idStr)
			if convErr != nil {
				return nil // not a segment-id-named file; ignore rather than fail the whole scan
			}
			if !ids[id] {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("maintenance: walk %s: %w", dir, err)
		}
	}
	return out, nil
}

func allSegmentIDs(ctx context.Context, store *db.DB) (map[int64]bool, error) {
	rows, err := store.Read().QueryContext(ctx, `SELECT id FROM segments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func parseSegmentID(s string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(s, "%d", &id)
	if err != nil {
		return 0, err
	}
	// Reject anything that scanned a prefix but wasn't a clean integer
	// (e.g. "12abc" would Sscanf "%d" into 12 without error).
	if fmt.Sprintf("%d", id) != s {
		return 0, fmt.Errorf("not a segment id: %q", s)
	}
	return id, nil
}

func statDBSizes(dbPath string, rep *Report) error {
	info, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	rep.DBSizeBytes = info.Size()

	if walInfo, err := os.Stat(dbPath + "-wal"); err == nil {
		rep.WALSizeBytes = walInfo.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readPragmaSizes(ctx context.Context, store *db.DB, rep *Report) error {
	if err := store.Read().QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&rep.FreelistPages); err != nil {
		return err
	}
	if err := store.Read().QueryRowContext(ctx, "PRAGMA page_count").Scan(&rep.PageCount); err != nil {
		return err
	}
	if err := store.Read().QueryRowContext(ctx, "PRAGMA page_size").Scan(&rep.PageSizeBytes); err != nil {
		return err
	}
	return nil
}

func capInt64(in []int64, n int) []int64 {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func capStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}
