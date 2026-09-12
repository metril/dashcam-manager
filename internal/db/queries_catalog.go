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
	// Backup (local-storage plan task 9, fix round 1): an aggregate over
	// EVERY segment matching this catalog value — not just the sample —
	// since a value like a plate or make can span dozens of segments and
	// a single sample segment's backup status would misrepresent the
	// rest. Populated by the handler via one batched CatalogBackupSummary
	// call across the whole page — see cmd/api's handleCatalogGet.
	Backup CatalogBackupSummary `json:"backup"`
}

// CatalogBackupSummary is one catalog value's backup status, aggregated
// over every segment that matches it (see CatalogBackupSummaries).
type CatalogBackupSummary struct {
	// Segments is the number of distinct segments matching this value.
	Segments int64 `json:"segments"`
	// BackedUp is how many of those segments have at least one persistent
	// copy — same rule as SegmentCopies: the segment's own storage (if
	// share/local-backed) or a verified ingest/pull.
	BackedUp int64 `json:"backed_up"`
	// SSDOnly is how many of those segments are non-archived on the
	// recorder's SSD with no persistent copy at all — i.e. "not ingested
	// yet". A segment can be neither backed up nor ssd_only (e.g.
	// archived with no verified ingest — shouldn't normally happen, but
	// isn't double-counted either way).
	SSDOnly int64 `json:"ssd_only"`
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

// CatalogBackupSummaries returns, for each of values (a page of distinct
// catalog values of the given kind), an aggregate backup status over
// EVERY segment matching that value — not just catalog_terms'
// sample_segment_id, since a plate/make/etc. can span many segments and a
// single sample's status wouldn't represent the rest.
//
// One query covers the whole page: a "matched" CTE joins the kind's
// detection table through detections to segments (same join shape
// RefreshCatalog uses) for every requested value at once, a "copy_info"
// CTE computes each matched segment's own-storage/verified-ingest/
// verified-pull copy count and whether it's a non-archived ssd-sourced
// row, and the final SELECT aggregates both per value. "Backed up" is the
// same rule as SegmentCopies (own persistent storage or a verified
// ingest/pull); "ssd_only" is a non-archived ssd-sourced segment with zero
// such copies. A value absent from the returned map has no matching
// segments (shouldn't happen for a real catalog_terms row).
func (d *DB) CatalogBackupSummaries(ctx context.Context, kind string, values []string) (map[string]CatalogBackupSummary, error) {
	out := map[string]CatalogBackupSummary{}
	if len(values) == 0 {
		return out, nil
	}
	cfg, ok := catalogKinds[kind]
	if !ok {
		return nil, fmt.Errorf("db: catalog backup summaries: unknown kind %q", kind)
	}

	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}

	query := fmt.Sprintf(`
		WITH matched AS (
			SELECT DISTINCT t.%[1]s AS value, d.segment_id AS segment_id
			FROM %[2]s t
			JOIN detections d ON d.id = t.det_id
			WHERE t.%[1]s IN (%[3]s)
		),
		copy_info AS (
			SELECT s.id AS segment_id,
				(CASE WHEN src.share_id IS NOT NULL THEN 1 ELSE 0 END
					+ (SELECT COUNT(*) FROM ingests i WHERE i.segment_id = s.id AND i.verified_at IS NOT NULL)
					+ (SELECT COUNT(*) FROM dashcam_pulls p WHERE p.indexed_segment_id = s.id AND p.verified_at IS NOT NULL)
				) AS copy_count,
				(CASE WHEN src.kind = 'ssd' AND s.archived_at IS NULL THEN 1 ELSE 0 END) AS on_ssd_own
			FROM segments s
			JOIN sources src ON src.id = s.source_id
			WHERE s.id IN (SELECT segment_id FROM matched)
		)
		SELECT m.value,
			COUNT(DISTINCT m.segment_id) AS segments,
			COUNT(DISTINCT CASE WHEN ci.copy_count > 0 THEN m.segment_id END) AS backed_up,
			COUNT(DISTINCT CASE WHEN ci.copy_count = 0 AND ci.on_ssd_own = 1 THEN m.segment_id END) AS ssd_only
		FROM matched m
		JOIN copy_info ci ON ci.segment_id = m.segment_id
		GROUP BY m.value
	`, cfg.column, cfg.table, strings.Join(placeholders, ","))

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: catalog backup summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		var s CatalogBackupSummary
		if err := rows.Scan(&value, &s.Segments, &s.BackedUp, &s.SSDOnly); err != nil {
			return nil, fmt.Errorf("db: catalog backup summaries: %w", err)
		}
		out[value] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: catalog backup summaries: %w", err)
	}
	return out, nil
}
