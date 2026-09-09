// queries_catalog.go implements GET /api/catalog: a distinct-value browser
// over the same detection tables Search filters on (det_vehicles.make,
// det_vehicles.type, det_plates.text, det_objects.class), grouped by value
// with a count and first/last seen segment so the Catalog page can link
// each row into Search with that value pre-set.
package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// catalogKindConfig maps a CatalogList kind to the table/column it groups
// distinct values from. table and column are trusted constants (never
// user input) so they're safe to interpolate into the query string.
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
}

// CatalogList groups det_vehicles.make / det_vehicles.type / det_plates.text
// / det_objects.class (per f.Kind) into distinct values with a count,
// first/last-seen segment start_time, and one sample segment (the most
// recent match) for a thumbnail. Returns rows ordered by count desc, value
// asc, plus a next-page cursor (empty when there are no more).
func (d *DB) CatalogList(ctx context.Context, f CatalogFilter) ([]CatalogItem, string, error) {
	cfg, ok := catalogKinds[f.Kind]
	if !ok {
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

	var whereClauses []string
	var havingClauses []string
	var args []any

	// Every kind's grouping column can hold NULL/empty (det_vehicles.make/
	// type are nullable; a make/type detector simply didn't fire) — those
	// never count as a catalog value.
	whereClauses = append(whereClauses, fmt.Sprintf("t.%s IS NOT NULL AND t.%s != ''", cfg.column, cfg.column))

	if f.Q != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("t.%s LIKE ? ESCAPE '\\'", cfg.column))
		args = append(args, "%"+escapeLike(f.Q)+"%")
	}

	// min_count is applied as a HAVING clause (it's a filter on the
	// aggregate, not a per-row predicate) — appended to args after the
	// WHERE args since it comes later in the query text.
	if f.MinCount > 0 {
		havingClauses = append(havingClauses, "COUNT(*) >= ?")
	}

	havingSQL := ""
	if len(havingClauses) > 0 {
		havingSQL = "HAVING " + strings.Join(havingClauses, " AND ")
		args = append(args, f.MinCount)
	}
	args = append(args, limit+1, offset)

	// t.det_id joins through detections to segments for start_time
	// (first/last seen) and a sample segment id. det_vehicles/det_objects/
	// det_plates all have a det_id column (see schema.go), so this join
	// shape is identical across kinds — only the table/column differ.
	query := fmt.Sprintf(`
		SELECT
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
		WHERE %[3]s
		GROUP BY t.%[1]s
		%[4]s
		ORDER BY count DESC, value ASC
		LIMIT ? OFFSET ?
	`, cfg.column, cfg.table, strings.Join(whereClauses, " AND "), havingSQL)

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
