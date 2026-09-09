// legacydb.go enriches segment_summary rows for segments that have no
// .detections.json sidecar by pulling counts from the recorder's legacy
// dashcam.db (which lags the sidecar-writing recorder by ~2 months and is
// opened strictly read-only/immutable — it is never the source of truth,
// only a best-effort enrichment source for older segments).
package scan

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"dashcam/internal/db"
)

// legacyClassCounts is class -> count for one legacy segment (by its
// rel_path, i.e. legacy `path` with the recorder's "/data/" prefix
// stripped).
type legacySummary struct {
	classes map[string]int
	plates  int
}

// EnrichFromLegacyDB opens "<rootPath>/dashcam.db" (a no-op, not an error,
// if it doesn't exist) and, for every segment under sourceID that has no
// detections sidecar (has_detections = 0), fills in segment_summary from
// the legacy per-class object/plate counts it finds for that segment's rel
// path. Best-effort throughout: any legacy query error is logged and
// enrichment simply stops (already-enriched segments are unaffected).
// Returns the number of segments enriched.
func EnrichFromLegacyDB(ctx context.Context, database *db.DB, sourceID int64, rootPath string) (int, error) {
	legacyPath := filepath.Join(rootPath, "dashcam.db")
	if _, err := os.Stat(legacyPath); err != nil {
		return 0, nil // no legacy db for this root — not an error
	}
	legacy, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&immutable=1", legacyPath))
	if err != nil {
		log.Printf("[scan] legacydb: open %s: %v (skipping legacy enrichment)", legacyPath, err)
		return 0, nil
	}
	defer legacy.Close()

	classCounts, err := legacyClassCountsByPath(ctx, legacy)
	if err != nil {
		log.Printf("[scan] legacydb: %v (skipping legacy enrichment)", err)
		return 0, nil
	}
	plateCounts, err := legacyPlateCountsByPath(ctx, legacy)
	if err != nil {
		log.Printf("[scan] legacydb: %v (skipping legacy enrichment)", err)
		return 0, nil
	}
	if len(classCounts) == 0 && len(plateCounts) == 0 {
		return 0, nil
	}

	rows, err := database.Read().QueryContext(ctx,
		`SELECT id, rel_path FROM segments WHERE source_id = ? AND has_detections = 0`, sourceID)
	if err != nil {
		return 0, fmt.Errorf("scan: legacydb: query segments: %w", err)
	}
	type target struct {
		id      int64
		relPath string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.relPath); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan: legacydb: scan segment: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan: legacydb: %w", err)
	}

	n := 0
	err = database.Tx(ctx, func(tx *sql.Tx) error {
		upSummary, err := tx.PrepareContext(ctx, `
			INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates, top_classes)
			VALUES (?,?,?,?,?,?,?,?,?)
			ON CONFLICT(segment_id) DO UPDATE SET
				n_car = excluded.n_car, n_truck = excluded.n_truck, n_bus = excluded.n_bus,
				n_person = excluded.n_person, n_bicycle = excluded.n_bicycle, n_motorcycle = excluded.n_motorcycle,
				n_plates = excluded.n_plates, top_classes = excluded.top_classes`)
		if err != nil {
			return err
		}
		defer upSummary.Close()

		for _, t := range targets {
			classes, hasClasses := classCounts[t.relPath]
			plates := plateCounts[t.relPath]
			if !hasClasses && plates == 0 {
				continue
			}
			top := topClasses(classes)
			topJSON, _ := json.Marshal(top)
			if _, err := upSummary.ExecContext(ctx, t.id,
				classes["car"], classes["truck"], classes["bus"],
				classes["person"], classes["bicycle"], classes["motorcycle"],
				plates, string(topJSON),
			); err != nil {
				return fmt.Errorf("upsert summary for %s: %w", t.relPath, err)
			}
			n++
		}
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("scan: legacydb: write: %w", err)
	}
	return n, nil
}

// legacyClassCountsByPath returns, for every legacy segment, its object
// class -> count map keyed by rel_path (legacy `path` with the leading
// "/<anything>/" data-root prefix stripped to "front/..."/"rear/...").
func legacyClassCountsByPath(ctx context.Context, legacy *sql.DB) (map[string]map[string]int, error) {
	rows, err := legacy.QueryContext(ctx, `
		SELECT s.path, o.class, COUNT(*)
		FROM segments s
		JOIN detections d ON d.segment_id = s.id
		JOIN objects o ON o.detection_id = d.id
		GROUP BY s.path, o.class`)
	if err != nil {
		return nil, fmt.Errorf("query legacy class counts: %w", err)
	}
	defer rows.Close()

	out := map[string]map[string]int{}
	for rows.Next() {
		var legacyPath, class string
		var n int
		if err := rows.Scan(&legacyPath, &class, &n); err != nil {
			return nil, err
		}
		rel := legacyRelPath(legacyPath)
		if rel == "" {
			continue
		}
		if out[rel] == nil {
			out[rel] = map[string]int{}
		}
		out[rel][class] += n
	}
	return out, rows.Err()
}

// legacyPlateCountsByPath returns, for every legacy segment, its plate
// count keyed by rel_path.
func legacyPlateCountsByPath(ctx context.Context, legacy *sql.DB) (map[string]int, error) {
	rows, err := legacy.QueryContext(ctx, `
		SELECT s.path, COUNT(*)
		FROM segments s
		JOIN detections d ON d.segment_id = s.id
		JOIN plates p ON p.detection_id = d.id
		GROUP BY s.path`)
	if err != nil {
		return nil, fmt.Errorf("query legacy plate counts: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var legacyPath string
		var n int
		if err := rows.Scan(&legacyPath, &n); err != nil {
			return nil, err
		}
		if rel := legacyRelPath(legacyPath); rel != "" {
			out[rel] = n
		}
	}
	return out, rows.Err()
}

// legacyRelPath converts a legacy absolute path (e.g.
// "/data/front/2026/02/27/recording_.../segment_00000.mp4") into our
// rel_path form ("front/2026/02/27/recording_.../segment_00000.mp4") by
// keeping everything from the first "front/" or "rear/" component onward.
func legacyRelPath(legacyPath string) string {
	p := filepath.ToSlash(legacyPath)
	for _, cam := range []string{"front/", "rear/"} {
		if i := strings.Index(p, cam); i >= 0 {
			return p[i:]
		}
	}
	return ""
}

// topClasses orders classes by count descending, ties broken by the
// canonical class order then alphabetically — same rule as
// Detections.Summary's TopClasses.
func topClasses(counts map[string]int) []string {
	classRank := make(map[string]int, len(DetectionClasses))
	for i, c := range DetectionClasses {
		classRank[c] = i
	}
	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		ci, cj := classes[i], classes[j]
		if counts[ci] != counts[cj] {
			return counts[ci] > counts[cj]
		}
		ri, iok := classRank[ci]
		rj, jok := classRank[cj]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		default:
			return ci < cj
		}
	})
	return classes
}
