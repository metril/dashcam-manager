// queries_catalog.go implements GET /api/catalog: a distinct-value browser
// over the same detection dimensions Search filters on (det_vehicles.make,
// det_vehicles.type, det_plates.text, det_objects.class).
//
// CatalogList used to run its GROUP BY live against det_vehicles/
// det_plates/det_objects on every request; measured in production against
// a multi-million-row det_vehicles table, GET /api/catalog?kind=make took
// ~24s. It now reads from catalog_terms — a small precomputed table (one
// row per kind+value) that RefreshCatalog rebuilds in one transaction.
// RefreshCatalog runs at the end of the index_detections job (see cmd/api's
// jobs_runtime.go) and once at startup if catalog_terms is still empty
// (cmd/api/main.go), so the catalog is never more than one indexing pass
// stale and GET /api/catalog is a plain indexed lookup.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// catalogRefreshedAtKey is the settings row RefreshCatalog stamps with the
// UTC time of its last successful run; CatalogList's caller (the API
// handler) surfaces it as "refreshed_at" so the Catalog page can show
// "Updated <time ago>".
const catalogRefreshedAtKey = "catalog_refreshed_at"

// catalogKindConfig maps a CatalogList/RefreshCatalog kind to the table/
// column it groups distinct values from. table and column are trusted
// constants (never user input) so they're safe to interpolate into the
// query string.
type catalogKindConfig struct {
	table  string // det_vehicles | det_plates | det_objects
	column string // make | type | text | class
}

var catalogKinds = map[string]catalogKindConfig{
	"make":  {table: "det_vehicles", column: "make"},
	"type":  {table: "det_vehicles", column: "type"},
	"plate": {table: "det_plates", column: "text"},
	"class": {table: "det_objects", column: "class"},
}

// CatalogFilter is the parsed set of GET /api/catalog query parameters.
type CatalogFilter struct {
	Kind     string // make | type | plate | class
	MinCount int    // 0 = no filter (rows with count >= 1 always qualify)
	Q        string // substring filter on the value itself
	Cursor   string // opaque offset token
	Limit    int
}

// CatalogItem is one distinct value row for GET /api/catalog.
type CatalogItem struct {
	Value       string `json:"value"`
	Count       int    `json:"count"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
	SampleSegID int64  `json:"sample_segment_id"`
	SampleThumb string `json:"sample_thumb"`
	// Copies/OnSSD (local-storage plan task 9): the sample segment's copy
	// info, populated by the handler via one batched SegmentCopies/
	// SegmentsOnSSD call across the whole page — see cmd/api's
	// handleCatalogGet. Copies is always a non-nil (possibly empty) slice.
	Copies []CopyRef `json:"copies"`
	OnSSD  bool      `json:"on_ssd"`
}

// RefreshCatalog rebuilds catalog_terms: for each kind (make/type/plate/
// class), GROUP BY the relevant detection table's value column, joined
// through detections/segments for first/last seen and a sample (most-
// recent-match) segment id.
//
// The GROUP BY over a multi-million-row det_vehicles/det_objects/det_plates
// table is slow (~24s measured in production) — running it inside one long
// write transaction, as this used to, holds sqlite's single write lock for
// the whole rebuild and starves other writers (e.g. job progress updates)
// that only get busy_timeout's worth of patience waiting for a BEGIN. So
// each kind is instead staged into its own table (catalog_terms_new) via a
// separate, non-transactional statement — CREATE TABLE ... AS SELECT for
// the first kind, INSERT INTO ... SELECT for the rest — each one committing
// (and releasing the write lock) as soon as it finishes, giving other
// writers a chance to run between kinds. Only the final swap into
// catalog_terms — a plain DELETE+INSERT+DROP, no GROUP BY — runs inside a
// short transaction, so a reader never sees a partially rebuilt table.
// Stamps catalogRefreshedAtKey with the completion time in that same
// transaction.
//
// This is the expensive live-GROUP-BY query CatalogList used to run per
// request; run here instead, once per indexing pass, it costs the same but
// is paid once for every subsequent read rather than on every page load.
func (d *DB) RefreshCatalog(ctx context.Context) error {
	// Drop any staging table left behind by a previous run that failed
	// partway through — CREATE TABLE catalog_terms_new below would
	// otherwise fail outright.
	if _, err := d.write.ExecContext(ctx, `DROP TABLE IF EXISTS catalog_terms_new`); err != nil {
		return fmt.Errorf("db: refresh catalog: drop stale staging table: %w", err)
	}

	first := true
	for kind, cfg := range catalogKinds {
		// t.det_id joins through detections to segments for start_time
		// (first/last seen) and a sample segment id — det_vehicles/
		// det_objects/det_plates all have a det_id column (schema.go), so
		// this join shape is identical across kinds; only the table/column
		// differ. kind and cfg come from the catalogKinds constant map
		// (never user input), so interpolating them is safe.
		selectSQL := fmt.Sprintf(`
			SELECT
				'%[3]s' AS kind,
				t.%[1]s AS value,
				COUNT(*) AS count,
				MIN(s.start_time) AS first_seen,
				MAX(s.start_time) AS last_seen,
				(SELECT s2.id FROM %[2]s t2
					JOIN detections d2 ON d2.id = t2.det_id
					JOIN segments s2 ON s2.id = d2.segment_id
					WHERE t2.%[1]s = t.%[1]s
					ORDER BY s2.start_time DESC LIMIT 1) AS sample_segment_id
			FROM %[2]s t
			JOIN detections d ON d.id = t.det_id
			JOIN segments s ON s.id = d.segment_id
			WHERE t.%[1]s IS NOT NULL AND t.%[1]s != ''
			GROUP BY t.%[1]s
		`, cfg.column, cfg.table, kind)

		stmt := "INSERT INTO catalog_terms_new " + selectSQL
		if first {
			// The first kind also creates catalog_terms_new, with column
			// names/order taken from the SELECT (kind, value, count,
			// first_seen, last_seen, sample_segment_id) — matching
			// catalog_terms's own column order so the final swap can
			// `INSERT INTO catalog_terms SELECT * FROM catalog_terms_new`.
			stmt = "CREATE TABLE catalog_terms_new AS " + selectSQL
		}
		if _, err := d.write.ExecContext(ctx, stmt); err != nil {
			_, _ = d.write.ExecContext(context.Background(), `DROP TABLE IF EXISTS catalog_terms_new`)
			return fmt.Errorf("db: refresh catalog: stage kind %q: %w", kind, err)
		}
		first = false
	}

	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_terms`); err != nil {
			return fmt.Errorf("db: refresh catalog: clear: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_terms SELECT * FROM catalog_terms_new`); err != nil {
			return fmt.Errorf("db: refresh catalog: swap: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE catalog_terms_new`); err != nil {
			return fmt.Errorf("db: refresh catalog: drop staging: %w", err)
		}

		now, err := json.Marshal(time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("db: refresh catalog: marshal timestamp: %w", err)
		}
		// Raw upsert (not SettingsSet, which opens its own transaction) —
		// this needs to land inside the same transaction as the swap above
		// so a reader never observes a refreshed catalog_terms table with a
		// stale (or missing) refreshed_at marker, or vice versa.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value_json) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json`,
			catalogRefreshedAtKey, string(now),
		); err != nil {
			return fmt.Errorf("db: refresh catalog: stamp refreshed_at: %w", err)
		}
		return nil
	})
}

// CatalogIsEmpty reports whether catalog_terms has no rows yet — used at
// startup to decide whether an initial RefreshCatalog is needed (e.g. first
// boot, or upgrading a database from before catalog_terms existed).
func (d *DB) CatalogIsEmpty(ctx context.Context) (bool, error) {
	var exists int
	err := d.read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_terms)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("db: catalog is empty: %w", err)
	}
	return exists == 0, nil
}

// CatalogRefreshedAt returns the UTC RFC3339 time of the last successful
// RefreshCatalog, or "" if it has never run.
func (d *DB) CatalogRefreshedAt(ctx context.Context) (string, error) {
	raw, err := d.SettingsGet(ctx, catalogRefreshedAtKey)
	if err != nil {
		if err == ErrNotFound {
			return "", nil
		}
		return "", fmt.Errorf("db: catalog refreshed at: %w", err)
	}
	var t string
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", fmt.Errorf("db: catalog refreshed at: %w", err)
	}
	return t, nil
}

// CatalogList reads catalog_terms for f.Kind, ordered by count desc, value
// asc, plus a next-page cursor (empty when there are no more). Values come
// from the last RefreshCatalog run, not a live query — see the package doc
// comment.
func (d *DB) CatalogList(ctx context.Context, f CatalogFilter) ([]CatalogItem, string, error) {
	if _, ok := catalogKinds[f.Kind]; !ok {
		return nil, "", fmt.Errorf("db: catalog: unknown kind %q", f.Kind)
	}

	offset := 0
	if f.Cursor != "" {
		n, err := strconv.Atoi(f.Cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("db: catalog: invalid cursor %q", f.Cursor)
		}
		offset = n
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	whereClauses := []string{"kind = ?"}
	args := []any{f.Kind}

	if f.Q != "" {
		whereClauses = append(whereClauses, "value LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(f.Q)+"%")
	}
	if f.MinCount > 0 {
		whereClauses = append(whereClauses, "count >= ?")
		args = append(args, f.MinCount)
	}
	args = append(args, limit+1, offset)

	query := fmt.Sprintf(`
		SELECT value, count, first_seen, last_seen, sample_segment_id
		FROM catalog_terms
		WHERE %s
		ORDER BY count DESC, value ASC
		LIMIT ? OFFSET ?
	`, strings.Join(whereClauses, " AND "))

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("db: catalog: %w", err)
	}
	defer rows.Close()

	var items []CatalogItem
	for rows.Next() {
		var it CatalogItem
		if err := rows.Scan(&it.Value, &it.Count, &it.FirstSeen, &it.LastSeen, &it.SampleSegID); err != nil {
			return nil, "", fmt.Errorf("db: catalog: %w", err)
		}
		it.SampleThumb = fmt.Sprintf("/api/segments/%d/thumb.jpg", it.SampleSegID)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("db: catalog: %w", err)
	}

	nextCursor := ""
	if len(items) > limit {
		items = items[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}
	return items, nextCursor, nil
}
