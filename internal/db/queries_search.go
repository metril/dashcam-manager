// queries_search.go implements GET /api/search: full-text + filtered
// lookup over detections, grouped by segment. It leans on the search_fts
// trigram index (schema.go) for the free-text query and on
// det_objects/det_vehicles/det_plates for the structured filters
// (class/make/type/plate), composing all of them as EXISTS clauses over a
// candidate set of `detections` rows so any combination can be applied
// together.
package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SearchFilter is the parsed set of GET /api/search query parameters.
type SearchFilter struct {
	Q          string  // free text: matches plate text, vehicle make, vehicle type, object class (trigram)
	Class      string  // exact object class (det_objects.class)
	Make       string  // exact vehicle make (det_vehicles.make)
	Type       string  // exact vehicle type (det_vehicles.type)
	Plate      string  // substring match on plate text (det_plates.text)
	From       string  // YYYY-MM-DD, inclusive
	To         string  // YYYY-MM-DD, inclusive
	MinConf    float64 // 0 = no filter
	MinSpeedMS float64 // 0 = no filter (detections.speed_mps, m/s)
	Cursor     string  // opaque offset token
	Limit      int
}

// SearchHit is one matched detection within a segment's result.
type SearchHit struct {
	PtsOffset  float64 `json:"pts_offset"`
	Kind       string  `json:"kind"` // "class" | "plate" | "make" | "type"
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

// SearchResult is one segment's grouped hits for GET /api/search.
type SearchResult struct {
	Segment   SearchSegmentInfo `json:"segment"`
	Trip      *SearchTripInfo   `json:"trip"`
	StartTime string            `json:"start_time"`
	Thumb     string            `json:"thumb"`
	Hits      []SearchHit       `json:"hits"`
}

type SearchSegmentInfo struct {
	ID     int64  `json:"id"`
	Camera string `json:"camera"`
	Seq    int    `json:"seq"`
}

type SearchTripInfo struct {
	ID      int64  `json:"id"`
	TripKey string `json:"trip_key"`
}

// maxHitsPerSegment bounds how many hit rows are returned per segment card
// so one segment with thousands of matching frames can't blow out a page.
const maxHitsPerSegment = 12

// candidateSQL builds the WHERE-clause fragment (against `detections d`,
// with det_objects/det_vehicles/det_plates/search_terms/search_fts
// available to EXISTS/IN subqueries) and its bound args for every filter in
// f except the date range and paging (those apply to `segments`/paging
// separately). Shared between the segment-listing query and the
// per-segment hits query so both see exactly the same candidate set.
func candidateSQL(f SearchFilter) (string, []any) {
	var clauses []string
	var args []any

	if f.Q != "" {
		clauses = append(clauses, `d.id IN (
			SELECT st.det_id FROM search_terms st
			WHERE st.det_id IS NOT NULL AND st.id IN (
				SELECT rowid FROM search_fts WHERE search_fts MATCH ?
			)
		)`)
		args = append(args, ftsQuery(f.Q))
	}
	// class/make/type/plate/min_conf are all written as an uncorrelated
	// "d.id IN (SELECT det_id FROM ... WHERE ...)" rather than a
	// correlated "EXISTS (... WHERE det_id = d.id AND ...)": with very
	// common values (e.g. class='car' matching millions of det_objects
	// rows), sqlite's planner was choosing to evaluate the correlated form
	// once per candidate detection (or even fall back to a full table
	// scan of det_objects per row) instead of using the det_id index, which
	// made the query effectively O(detections × matches) and took minutes.
	// The IN-subquery form is evaluated once, materialized as a transient
	// index, and then probed per outer row — the same pattern already used
	// for the `q` (search_fts) filter above.
	if f.Class != "" {
		clauses = append(clauses, `d.id IN (SELECT det_id FROM det_objects WHERE class = ?)`)
		args = append(args, f.Class)
	}
	if f.Make != "" {
		clauses = append(clauses, `d.id IN (SELECT det_id FROM det_vehicles WHERE make = ?)`)
		args = append(args, f.Make)
	}
	if f.Type != "" {
		clauses = append(clauses, `d.id IN (SELECT det_id FROM det_vehicles WHERE type = ?)`)
		args = append(args, f.Type)
	}
	if f.Plate != "" {
		clauses = append(clauses, `d.id IN (SELECT det_id FROM det_plates WHERE text LIKE ? ESCAPE '\')`)
		args = append(args, "%"+escapeLike(f.Plate)+"%")
	}
	if f.MinConf > 0 {
		clauses = append(clauses, `d.id IN (
			SELECT det_id FROM det_objects WHERE confidence >= ?
			UNION
			SELECT det_id FROM det_plates WHERE confidence >= ?
			UNION
			SELECT det_id FROM det_vehicles WHERE confidence >= ?
		)`)
		args = append(args, f.MinConf, f.MinConf, f.MinConf)
	}
	if f.MinSpeedMS > 0 {
		clauses = append(clauses, `d.speed_mps >= ?`)
		args = append(args, f.MinSpeedMS)
	}

	if len(clauses) == 0 {
		return "1=1", nil
	}
	return strings.Join(clauses, " AND "), args
}

// ftsQuery quotes q as a trigram MATCH literal: FTS5 syntax treats bare
// tokens specially (AND/OR/NOT, column filters, "*"...), and search text
// here (plate fragments, makes) should always be matched literally.
func ftsQuery(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Search runs filter against detections/segments and returns one
// SearchResult per matching segment (most recent first), plus a next-page
// cursor (empty when there are no more).
func (d *DB) Search(ctx context.Context, f SearchFilter) ([]SearchResult, string, error) {
	offset := 0
	if f.Cursor != "" {
		n, err := strconv.Atoi(f.Cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("db: search: invalid cursor %q", f.Cursor)
		}
		offset = n
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 30
	}

	cond, args := candidateSQL(f)

	var dateClause string
	if f.From != "" {
		start, _, err := dayRange(f.From)
		if err != nil {
			return nil, "", fmt.Errorf("db: search: %w", err)
		}
		dateClause += " AND s.start_time >= ?"
		args = append(args, start)
	}
	if f.To != "" {
		_, end, err := dayRange(f.To)
		if err != nil {
			return nil, "", fmt.Errorf("db: search: %w", err)
		}
		dateClause += " AND s.start_time < ?"
		args = append(args, end)
	}

	segQuery := fmt.Sprintf(`
		SELECT s.id, s.camera, s.seq, s.start_time, s.trip_id, t.trip_key
		FROM segments s
		JOIN trips t ON t.id = s.trip_id
		WHERE EXISTS (SELECT 1 FROM detections d WHERE d.segment_id = s.id AND (%s))%s
		ORDER BY s.start_time DESC, s.id DESC
		LIMIT ? OFFSET ?
	`, cond, dateClause)
	segArgs := append(append([]any{}, args...), limit+1, offset)

	rows, err := d.read.QueryContext(ctx, segQuery, segArgs...)
	if err != nil {
		return nil, "", fmt.Errorf("db: search: %w", err)
	}
	type segRow struct {
		id        int64
		camera    string
		seq       int
		startTime string
		tripID    *int64
		tripKey   *string
	}
	var segs []segRow
	for rows.Next() {
		var sr segRow
		if err := rows.Scan(&sr.id, &sr.camera, &sr.seq, &sr.startTime, &sr.tripID, &sr.tripKey); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("db: search: %w", err)
		}
		segs = append(segs, sr)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, "", fmt.Errorf("db: search: %w", err)
	}
	rows.Close()

	nextCursor := ""
	if len(segs) > limit {
		segs = segs[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}

	out := make([]SearchResult, 0, len(segs))
	for _, sr := range segs {
		hits, err := d.searchHitsForSegment(ctx, sr.id, cond, args)
		if err != nil {
			return nil, "", err
		}
		res := SearchResult{
			Segment:   SearchSegmentInfo{ID: sr.id, Camera: sr.camera, Seq: sr.seq},
			StartTime: sr.startTime,
			Thumb:     fmt.Sprintf("/api/segments/%d/thumb.jpg", sr.id),
			Hits:      hits,
		}
		if sr.tripID != nil && sr.tripKey != nil {
			res.Trip = &SearchTripInfo{ID: *sr.tripID, TripKey: *sr.tripKey}
		}
		out = append(out, res)
	}
	return out, nextCursor, nil
}

// searchHitsForSegment re-applies cond/args (the same candidate-detection
// filter used to select the segment) scoped to one segment, and expands
// each matching detection into its class/plate/make/type hit rows.
func (d *DB) searchHitsForSegment(ctx context.Context, segmentID int64, cond string, args []any) ([]SearchHit, error) {
	query := fmt.Sprintf(`
		SELECT d.pts_offset, 'class' AS kind, o.class AS text, COALESCE(o.confidence, 0)
			FROM det_objects o JOIN detections d ON d.id = o.det_id
			WHERE d.segment_id = ? AND (%[1]s)
		UNION ALL
		SELECT d.pts_offset, 'plate', p.text, COALESCE(p.confidence, 0)
			FROM det_plates p JOIN detections d ON d.id = p.det_id
			WHERE d.segment_id = ? AND (%[1]s)
		UNION ALL
		SELECT d.pts_offset, 'make', v.make, COALESCE(v.confidence, 0)
			FROM det_vehicles v JOIN detections d ON d.id = v.det_id
			WHERE d.segment_id = ? AND v.make IS NOT NULL AND v.make != '' AND (%[1]s)
		UNION ALL
		SELECT d.pts_offset, 'type', v.type, COALESCE(v.confidence, 0)
			FROM det_vehicles v JOIN detections d ON d.id = v.det_id
			WHERE d.segment_id = ? AND v.type IS NOT NULL AND v.type != '' AND (%[1]s)
		ORDER BY pts_offset
		LIMIT ?
	`, cond)

	var qargs []any
	for i := 0; i < 4; i++ {
		qargs = append(qargs, segmentID)
		qargs = append(qargs, args...)
	}
	qargs = append(qargs, maxHitsPerSegment)

	rows, err := d.read.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, fmt.Errorf("db: search hits: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.PtsOffset, &h.Kind, &h.Text, &h.Confidence); err != nil {
			return nil, fmt.Errorf("db: search hits: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// GPSPoint is one sample along a trip's track for GET /api/gps/track.
type GPSPoint struct {
	Lat      float64
	Lon      float64
	SpeedMps *float64
	T        string
}

// TripGPSTrack returns every segment_gps sample for tripID's segments,
// ordered by wall-clock time, preferring the front camera's GPS log when
// both cameras have one (front and rear GPS logs cover the same physical
// path, so mixing both would just duplicate points and add jitter from two
// independent GPS reads of the same moment).
func (d *DB) TripGPSTrack(ctx context.Context, tripID int64) ([]GPSPoint, error) {
	camera := "front"
	var frontHasGPS int
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments s JOIN segment_gps g ON g.segment_id = s.id WHERE s.trip_id = ? AND s.camera = 'front'`,
		tripID).Scan(&frontHasGPS)
	if err != nil {
		return nil, fmt.Errorf("db: trip gps track: %w", err)
	}
	if frontHasGPS == 0 {
		camera = "rear"
	}

	rows, err := d.read.QueryContext(ctx, `
		SELECT g.lat, g.lon, g.speed_mps, g.t
		FROM segment_gps g
		JOIN segments s ON s.id = g.segment_id
		WHERE s.trip_id = ? AND s.camera = ?
		ORDER BY g.t
	`, tripID, camera)
	if err != nil {
		return nil, fmt.Errorf("db: trip gps track: %w", err)
	}
	defer rows.Close()

	var out []GPSPoint
	for rows.Next() {
		var p GPSPoint
		if err := rows.Scan(&p.Lat, &p.Lon, &p.SpeedMps, &p.T); err != nil {
			return nil, fmt.Errorf("db: trip gps track: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
