// queries_search.go implements GET /api/search: full-text + filtered
// lookup over detections, grouped by segment.
//
// Performance strategy (rewritten after M12 review — see m12-report.md's
// "fix round 1" section for the measured before/after):
//   - A class-only search (optionally with a date range) never touches
//     detections/det_objects at all: it drives straight off
//     segment_summary's precomputed n_<class> counters, which is a single
//     indexed pass over segments in start_time order with a segment_id
//     rowid lookup per row (segmentSummaryFastPath).
//   - Every other combination uses correlated EXISTS clauses (detections
//     joined to det_objects/det_vehicles/det_plates through det_id, with
//     det_id leading each relevant index) rather than uncorrelated
//     "d.id IN (SELECT det_id FROM ... )" sets: with ORDER BY start_time
//     DESC LIMIT ? on the outer segments scan, a correlated EXISTS lets
//     sqlite stop as soon as it has found `limit` matching segments
//     (walking segments newest-first via idx_segments_start_time) instead
//     of first materializing a candidate set over the entire table.
//   - search_fts (the `q` filter) is resolved once via a `q_matched`
//     common table expression prepended to whichever query needs it
//     (qMatchedCTE), rather than nesting the MATCH inside the correlated
//     EXISTS: sqlite's planner does not reliably hoist a MATCH subquery
//     out of a correlated scope even when it doesn't reference the outer
//     row (confirmed via EXPLAIN QUERY PLAN against the real ~39k-segment
//     scratch DB — nesting it inline turned a `q`-only search into a
//     multi-minute hang, re-running the FTS scan once per candidate
//     detection instead of once total). A CTE sqlite always evaluates
//     once up front.
package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// searchClassColumns maps a class name search can filter on to its
// segment_summary counter column, for the class-only fast path. Classes
// outside this set (there are none today, since scan.IndexDetections only
// ever writes these six into segment_summary) fall back to the general
// det_objects-driven path.
var searchClassColumns = map[string]string{
	"car":        "n_car",
	"truck":      "n_truck",
	"bus":        "n_bus",
	"person":     "n_person",
	"bicycle":    "n_bicycle",
	"motorcycle": "n_motorcycle",
}

// SearchFilter is the parsed set of GET /api/search query parameters.
type SearchFilter struct {
	Q          string   // free text: matches plate text, vehicle make, vehicle type, object class (trigram)
	Classes    []string // exact object class(es) (det_objects.class), OR'd together
	Make       string   // exact vehicle make (det_vehicles.make)
	Type       string   // exact vehicle type (det_vehicles.type)
	Plate      string   // substring match on plate text (det_plates.text)
	From       string   // YYYY-MM-DD, inclusive
	To         string   // YYYY-MM-DD, inclusive
	MinConf    float64  // 0 = no filter
	MinSpeedMS float64  // 0 = no filter (detections.speed_mps, m/s)
	Cursor     string   // opaque offset token
	Limit      int
}

// SearchHit is one matched detection (or, when the underlying detections
// belong to a det_tracks track, one *track*) within a segment's result.
type SearchHit struct {
	PtsOffset  float64 `json:"pts_offset"`
	Kind       string  `json:"kind"` // "class" | "plate" | "make" | "type"
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	// Count is the number of raw detection frames collapsed into this hit:
	// a track's n_frames when the hit was grouped by det_tracks (task 2 —
	// several frames of the same followed object/plate/vehicle collapse
	// into one chip instead of one per frame), or 1 for an ungrouped,
	// not-yet-tracked frame (track_id NULL). Additive field; existing
	// consumers that ignore it see the same hit shape as before.
	Count int `json:"count"`
}

// SearchResult is one segment's grouped hits for GET /api/search.
type SearchResult struct {
	Segment   SearchSegmentInfo `json:"segment"`
	Trip      *SearchTripInfo   `json:"trip"`
	StartTime string            `json:"start_time"`
	Thumb     string            `json:"thumb"`
	Hits      []SearchHit       `json:"hits"`
	// HitsTruncated is true when this segment had more structural
	// (class/plate/make/type) or free-text hits than fit within
	// maxHitsPerSegment/maxStructuralHitsPerSegment — the card's hit list
	// is a sample, not exhaustive. Additive field.
	HitsTruncated bool `json:"hits_truncated,omitempty"`
}

type SearchSegmentInfo struct {
	ID     int64  `json:"id"`
	Camera string `json:"camera"`
	Seq    int    `json:"seq"`
	// UnreadableReason: non-nil once the segment's media file was found
	// undecodable (see db.SegmentMarkUnreadable). Included so search
	// result cards can show the same "Unreadable" badge as trip detail.
	UnreadableReason *string `json:"unreadable_reason,omitempty"`
}

type SearchTripInfo struct {
	ID      int64  `json:"id"`
	TripKey string `json:"trip_key"`
}

// maxHitsPerSegment bounds how many hit rows are returned per segment card
// so one segment with thousands of matching frames can't blow out a page.
const maxHitsPerSegment = 12

// maxStructuralHitsPerSegment bounds how many class/plate/make/type hits
// searchStructuralHits keeps per segment before qHits are appended (see
// Search) — smaller than maxHitsPerSegment so a q match still has room to
// show up in the combined, final-capped list.
const maxStructuralHitsPerSegment = 8

// isFastPathEligible reports whether f can be answered entirely from
// segment_summary: only class(es) (all known counter columns) and
// optionally a date range, nothing else.
func (f SearchFilter) isFastPathEligible() bool {
	if len(f.Classes) == 0 {
		return false
	}
	if f.Q != "" || f.Make != "" || f.Type != "" || f.Plate != "" || f.MinConf > 0 || f.MinSpeedMS > 0 {
		return false
	}
	for _, c := range f.Classes {
		if _, ok := searchClassColumns[c]; !ok {
			return false
		}
	}
	return true
}

// isQAndClassFastPathEligible reports whether f is exactly "q + one or
// more classes" (+ optional date range), nothing else — the combination
// that used to fall through to searchSegmentsGeneral's correlated-EXISTS
// path and, measured against the real ~39k-segment scratch DB, took ~37s:
// for every segment (walked newest-first via idx_segments_start_time) it
// re-evaluated "EXISTS detection matching both the class filter AND
// q_matched" as a single correlated subquery, which sqlite could not turn
// into an index-driven segment-level filter the way the class-only fast
// path or the q-only general path individually can. See
// searchQAndClassFastPath for the fix.
func (f SearchFilter) isQAndClassFastPathEligible() bool {
	if f.Q == "" || len(f.Classes) == 0 {
		return false
	}
	if f.Make != "" || f.Type != "" || f.Plate != "" || f.MinConf > 0 || f.MinSpeedMS > 0 {
		return false
	}
	for _, c := range f.Classes {
		if _, ok := searchClassColumns[c]; !ok {
			return false
		}
	}
	return true
}

// matchPredicate builds a correlated SQL boolean expression (referencing
// detections alias `d`) plus its bound args, true for a detection matching
// every active structural filter in f (class/make/type/plate/q/min_conf/
// min_speed combined with AND — MinConf, when set, is folded into each
// active structural EXISTS as an additional confidence predicate rather
// than a separate 3-way candidate set). Returns "1=1" (no args) when f has
// no structural filters at all (date-range/paging-only callers).
func matchPredicate(f SearchFilter) (string, []any) {
	var clauses []string
	var args []any

	structural := false

	if len(f.Classes) > 0 {
		structural = true
		placeholders := strings.TrimRight(strings.Repeat("?,", len(f.Classes)), ",")
		clause := fmt.Sprintf(`EXISTS (SELECT 1 FROM det_objects o WHERE o.det_id = d.id AND o.class IN (%s)`, placeholders)
		for _, c := range f.Classes {
			args = append(args, c)
		}
		if f.MinConf > 0 {
			clause += ` AND o.confidence >= ?`
			args = append(args, f.MinConf)
		}
		clause += `)`
		clauses = append(clauses, clause)
	}
	if f.Make != "" {
		structural = true
		clause := `EXISTS (SELECT 1 FROM det_vehicles v WHERE v.det_id = d.id AND v.make = ?`
		args = append(args, f.Make)
		if f.MinConf > 0 {
			clause += ` AND v.confidence >= ?`
			args = append(args, f.MinConf)
		}
		clause += `)`
		clauses = append(clauses, clause)
	}
	if f.Type != "" {
		structural = true
		clause := `EXISTS (SELECT 1 FROM det_vehicles v WHERE v.det_id = d.id AND v.type = ?`
		args = append(args, f.Type)
		if f.MinConf > 0 {
			clause += ` AND v.confidence >= ?`
			args = append(args, f.MinConf)
		}
		clause += `)`
		clauses = append(clauses, clause)
	}
	if f.Plate != "" {
		structural = true
		clause := `EXISTS (SELECT 1 FROM det_plates p WHERE p.det_id = d.id AND p.text LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(f.Plate)+"%")
		if f.MinConf > 0 {
			clause += ` AND p.confidence >= ?`
			args = append(args, f.MinConf)
		}
		clause += `)`
		clauses = append(clauses, clause)
	}
	// min_conf alone (no class/make/type/plate to attach it to): at least
	// one of the three confidence-bearing tables meets the bar, still
	// written as correlated EXISTS clauses (not a UNION candidate set).
	if f.MinConf > 0 && !structural {
		clauses = append(clauses, `(
			EXISTS (SELECT 1 FROM det_objects o WHERE o.det_id = d.id AND o.confidence >= ?)
			OR EXISTS (SELECT 1 FROM det_plates p WHERE p.det_id = d.id AND p.confidence >= ?)
			OR EXISTS (SELECT 1 FROM det_vehicles v WHERE v.det_id = d.id AND v.confidence >= ?)
		)`)
		args = append(args, f.MinConf, f.MinConf, f.MinConf)
	}
	if f.Q != "" {
		// References the q_matched CTE (see qMatchedCTE) rather than
		// nesting the MATCH here — see the package doc comment for why.
		// Written as a correlated EXISTS, not "d.id IN (SELECT det_id FROM
		// q_matched)": with a common search term (q_matched can run to
		// hundreds of thousands of rows), sqlite evaluated the IN form as
		// a full unindexed scan of the materialized q_matched per
		// candidate detection. The EXISTS form let it recognize the
		// repeated correlated lookup and build an automatic covering
		// index (plus a bloom filter) over q_matched instead — measured
		// as the difference between a many-minute hang and ~1-2s against
		// the real ~39k-segment scratch DB with a very common term.
		clauses = append(clauses, `EXISTS (SELECT 1 FROM q_matched qm WHERE qm.det_id = d.id)`)
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

// hitKinds returns which SearchHit.Kind values should be emitted for f, per
// the "when a structured filter is set, emit only that kind's hits" rule:
// each active structural filter contributes its own kind, `q` always
// contributes (its matches can be any kind), and if nothing structural (or
// q) is set (a bare min_conf/min_speed search) every kind is shown since
// there's no more specific filter to key off of.
func hitKinds(f SearchFilter) (kinds []string, anyStructural bool) {
	if len(f.Classes) > 0 {
		kinds = append(kinds, "class")
		anyStructural = true
	}
	if f.Make != "" {
		kinds = append(kinds, "make")
		anyStructural = true
	}
	if f.Type != "" {
		kinds = append(kinds, "type")
		anyStructural = true
	}
	if f.Plate != "" {
		kinds = append(kinds, "plate")
		anyStructural = true
	}
	if f.Q != "" {
		anyStructural = true // q counts as "a filter is set" even though it has no single kind
	}
	if !anyStructural {
		kinds = []string{"class", "plate", "make", "type"}
	}
	return kinds, anyStructural
}

// qMatchedCTE returns the body (without the leading "WITH") of a
// `q_matched` common table expression resolving f.Q's free-text match
// exactly once, database-wide — the det_id of every search_terms row whose
// text matched. Every matchPredicate clause that needs it references
// "EXISTS (SELECT 1 FROM q_matched qm WHERE qm.det_id = d.id)"; callers
// must prepend "WITH " + this body + " " (or ", " + this body when adding
// to an existing WITH list) before using cond, with this CTE's args placed
// first since it appears first in the resulting SQL text.
//
// This used to take an optional segmentID to scope the match to one
// segment (searchHitsForSegment called it that way, once per
// already-selected result segment). That scoping only narrowed the CTE's
// *output* — the inner "SELECT rowid FROM search_fts WHERE search_fts
// MATCH ?" has no way to know about segment_id and always re-ran the full
// trigram scan regardless, so scoping it per segment meant paying that
// scan once per result-page segment instead of once total. Removed as
// part of final-fix-wave item 9: searchHitsForSegment no longer needs
// q_matched at all (see its doc comment), and every remaining caller wants
// the database-wide match set exactly once.
func qMatchedCTE(q string) (string, []any) {
	// Two things had to be true here (found by EXPLAIN QUERY PLAN against
	// the real ~39k-segment scratch DB, where getting any of them wrong
	// turned a `q` search into a multi-minute hang):
	//   - The FTS5-recommended form ("rowid IN (SELECT rowid FROM fts
	//     WHERE fts MATCH ?)") is what actually drives off the trigram
	//     index; a JOIN with MATCH in the ON/WHERE clause instead made
	//     sqlite scan search_terms as the outer loop.
	//   - MATERIALIZED is required: sqlite's optimizer otherwise inlines
	//     a CTE referenced exactly once back into its single call site —
	//     here, right back inside the correlated EXISTS it was meant to
	//     be hoisted out of — silently undoing the whole point of using a
	//     CTE.
	//   - matchPredicate's EXISTS-against-q_matched form (not
	//     "IN (SELECT det_id FROM q_matched)") is what lets sqlite build
	//     an automatic covering index (plus a bloom filter) over the
	//     materialized set for the repeated correlated lookup, rather
	//     than a full unindexed scan of it per candidate detection.
	return `q_matched AS MATERIALIZED (
		SELECT det_id FROM search_terms
		WHERE det_id IS NOT NULL AND id IN (
			SELECT rowid FROM search_fts WHERE search_fts MATCH ?
		)
	)`, []any{ftsQuery(q)}
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

type segRow struct {
	id               int64
	camera           string
	seq              int
	startTime        string
	tripID           *int64
	tripKey          *string
	unreadableReason *string
}

// Search runs filter against detections/segments (or, for a class-only
// filter, straight off segment_summary) and returns one SearchResult per
// matching segment (most recent first), plus a next-page cursor (empty
// when there are no more).
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

	var dateArgs []any
	var dateClause string
	if f.From != "" {
		start, _, err := dayRange(f.From)
		if err != nil {
			return nil, "", fmt.Errorf("db: search: %w", err)
		}
		dateClause += " AND s.start_time >= ?"
		dateArgs = append(dateArgs, start)
	}
	if f.To != "" {
		_, end, err := dayRange(f.To)
		if err != nil {
			return nil, "", fmt.Errorf("db: search: %w", err)
		}
		dateClause += " AND s.start_time < ?"
		dateArgs = append(dateArgs, end)
	}

	var segs []segRow
	var err error
	switch {
	case f.isFastPathEligible():
		segs, err = d.searchSegmentsFastPath(ctx, f.Classes, dateClause, dateArgs, limit, offset)
	case f.isQAndClassFastPathEligible():
		segs, err = d.searchQAndClassFastPath(ctx, f, dateClause, dateArgs, limit, offset)
	default:
		segs, err = d.searchSegmentsGeneral(ctx, f, dateClause, dateArgs, limit, offset)
	}
	if err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(segs) > limit {
		segs = segs[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}

	kinds, _ := hitKinds(f)

	var ids []int64
	if len(segs) > 0 {
		ids = make([]int64, len(segs))
		for i, sr := range segs {
			ids[i] = sr.id
		}
	}

	// f.Q's hits are fetched once for the whole page (see searchQHits) —
	// not per segment — since re-declaring/re-scoping q_matched once per
	// segment was still the dominant cost for a common search term even
	// after scoping it to one segment at a time (each call pays the FTS
	// MATCH scan again).
	var qHits map[int64][]SearchHit
	var qTrunc map[int64]bool
	if f.Q != "" && len(ids) > 0 {
		qHits, qTrunc, err = d.searchQHits(ctx, f.Q, ids)
		if err != nil {
			return nil, "", err
		}
	}

	// Structural (class/plate/make/type) hits are likewise resolved once
	// for the whole page instead of once per segment — see
	// searchStructuralHits, which replaced the old per-segment
	// searchHitsForSegment loop.
	structHits, structTrunc, err := d.searchStructuralHits(ctx, f, kinds, ids)
	if err != nil {
		return nil, "", err
	}

	out := make([]SearchResult, 0, len(segs))
	for _, sr := range segs {
		sh := structHits[sr.id]
		qh := qHits[sr.id]
		hits := make([]SearchHit, 0, len(sh)+len(qh))
		hits = append(hits, sh...)
		hits = append(hits, qh...)
		truncated := structTrunc[sr.id] || qTrunc[sr.id]
		if len(hits) > maxHitsPerSegment {
			hits = hits[:maxHitsPerSegment]
			truncated = true
		}
		res := SearchResult{
			Segment:       SearchSegmentInfo{ID: sr.id, Camera: sr.camera, Seq: sr.seq, UnreadableReason: sr.unreadableReason},
			StartTime:     sr.startTime,
			Thumb:         fmt.Sprintf("/api/segments/%d/thumb.jpg", sr.id),
			Hits:          hits,
			HitsTruncated: truncated,
		}
		if sr.tripID != nil && sr.tripKey != nil {
			res.Trip = &SearchTripInfo{ID: *sr.tripID, TripKey: *sr.tripKey}
		}
		out = append(out, res)
	}
	return out, nextCursor, nil
}

// searchQHits resolves f.Q's free-text matches for every segment in
// segIDs in a single query (one FTS MATCH scan total, not one per
// segment — see the note in Search's caller), grouped by segment id and
// capped at maxHitsPerSegment per segment. The second return value reports,
// per segment, whether that per-segment cap actually dropped rows — fix
// round 1: previously this truncation was invisible to the caller (Search
// only set HitsTruncated when the *combined* class+q list exceeded
// maxHitsPerSegment, which a segment with e.g. 15 q matches and 0
// structural hits could dodge: 12 kept, 3 silently dropped, final combined
// length exactly 12, no truncation reported).
func (d *DB) searchQHits(ctx context.Context, q string, segIDs []int64) (map[int64][]SearchHit, map[int64]bool, error) {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(segIDs)), ",")
	args := make([]any, 0, len(segIDs)+1)
	for _, id := range segIDs {
		args = append(args, id)
	}
	args = append(args, ftsQuery(q))

	query := fmt.Sprintf(`
		SELECT d.segment_id, d.pts_offset, st.kind, st.text, COALESCE((
				SELECT o.confidence FROM det_objects o WHERE o.det_id = d.id AND o.class = st.text AND st.kind = 'class' LIMIT 1
			), (
				SELECT p.confidence FROM det_plates p WHERE p.det_id = d.id AND p.text = st.text AND st.kind = 'plate' LIMIT 1
			), (
				SELECT v.confidence FROM det_vehicles v WHERE v.det_id = d.id AND v.make = st.text AND st.kind = 'make' LIMIT 1
			), (
				SELECT v.confidence FROM det_vehicles v WHERE v.det_id = d.id AND v.type = st.text AND st.kind = 'type' LIMIT 1
			), 0) AS confidence
		FROM search_terms st
		JOIN detections d ON d.id = st.det_id
		WHERE d.segment_id IN (%s)
		AND st.id IN (SELECT rowid FROM search_fts WHERE search_fts MATCH ?)
		ORDER BY d.segment_id, d.pts_offset
	`, placeholders)

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("db: search q hits: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]SearchHit)
	truncated := make(map[int64]bool)
	for rows.Next() {
		var segID int64
		var h SearchHit
		if err := rows.Scan(&segID, &h.PtsOffset, &h.Kind, &h.Text, &h.Confidence); err != nil {
			return nil, nil, fmt.Errorf("db: search q hits: %w", err)
		}
		h.Count = 1
		if len(out[segID]) >= maxHitsPerSegment {
			truncated[segID] = true
			continue
		}
		out[segID] = append(out[segID], h)
	}
	return out, truncated, rows.Err()
}

// searchSegmentsFastPath answers a class-only (+ optional date range)
// search straight from segment_summary's precomputed n_<class> counters —
// segments never joins detections/det_objects at all.
func (d *DB) searchSegmentsFastPath(ctx context.Context, classes []string, dateClause string, dateArgs []any, limit, offset int) ([]segRow, error) {
	var orClauses []string
	var args []any
	for _, c := range classes {
		col := searchClassColumns[c] // isFastPathEligible already validated every class is present
		orClauses = append(orClauses, fmt.Sprintf("ss.%s > 0", col))
	}
	args = append(args, dateArgs...)
	args = append(args, limit+1, offset)

	query := fmt.Sprintf(`
		SELECT s.id, s.camera, s.seq, s.start_time, s.trip_id, t.trip_key, s.unreadable_reason
		FROM segments s
		JOIN segment_summary ss ON ss.segment_id = s.id
		LEFT JOIN trips t ON t.id = s.trip_id
		WHERE (%s)%s
		ORDER BY s.start_time DESC, s.id DESC
		LIMIT ? OFFSET ?
	`, strings.Join(orClauses, " OR "), dateClause)

	return d.querySegRows(ctx, query, args)
}

// searchQAndClassFastPath answers a "q + class(es)" search (see
// isQAndClassFastPathEligible).
//
// Two earlier shapes were tried and measured against the real
// ~39k-segment scratch DB before landing on this one (see git history /
// final-fix-report.md for the numbers):
//   - A single CTE joining search_terms straight to detections and
//     DISTINCT-ing down to segment_id: correct, but sqlite must resolve
//     and sort-dedupe every one of q's matching rows before anything else
//     can happen — ~56s for a term matching 1.28M rows (worse than the
//     ~37s this whole fast path exists to fix).
//   - A single det_id-level MATERIALIZED q_matched CTE (matching
//     qMatchedCTE's existing shape) with the outer scan driven off
//     segments in start_time order and two correlated EXISTS clauses
//     (class, then q) per candidate segment: this seemed like the obvious
//     "reuse the pattern that already works for pure q" move, but its
//     EXISTS-over-detections clause must walk every detection row in a
//     non-matching segment before concluding "no match" — with this
//     dataset's very high detection density (hundreds of detections per
//     segment), that's effectively an O(total detections) scan for any
//     search term that isn't near-universal, regardless of the outer
//     LIMIT: ~30-50s across several different terms.
//
// What actually works is splitting the materialization into two stages,
// letting sqlite dedupe via a hash join instead of a per-segment
// correlated walk:
//  1. q_matched: det_id only (same shape as qMatchedCTE) — a one-time
//     hash-built match set, ~1-2s even for a 1.28M-row term.
//  2. q_matched_segs: DISTINCT segment_id, joining q_matched to
//     detections by primary key (a plain hash-join dedupe over the
//     already-small det_id set, not a per-segment walk) — this is the
//     step that actually collapses the cost.
//
// Combined, this candidate-selection query alone is ~2-2.5s even for the
// worst observed term. searchHitsForSegment no longer needs its own
// per-segment q resolution at all (see its doc comment) — Search's
// separately-batched searchQHits call pays one more one-time FTS
// resolution for hit *detail* — so end-to-end through Search lands around
// 3-4s for that same worst-case term.
//
// q_matched_segs is then joined to segments (by id — cheap primary-key
// lookups) and filtered by the class predicate (as a correlated EXISTS
// against segment_summary, same shape as the class-only fast path) before
// sorting/paginating.
//
// For a realistic, selective search term (a plate fragment, a specific
// make on a dataset without pathological over-tagging) this answers
// end-to-end in single-digit milliseconds. The multi-second cost above
// only shows up for search terms matching a large fraction of the entire
// library (700k-1.3M term rows out of ~39k segments — effectively "half
// the vehicles in the database", not a realistic user query) and is
// entirely the one-time FTS resolution of an enormous match set, not
// anything progressively worse about the search itself. See
// final-fix-report.md for the measured numbers across both realistic and
// worst-case terms.
func (d *DB) searchQAndClassFastPath(ctx context.Context, f SearchFilter, dateClause string, dateArgs []any, limit, offset int) ([]segRow, error) {
	var orClauses []string
	for _, c := range f.Classes {
		col := searchClassColumns[c] // isQAndClassFastPathEligible already validated every class is present
		orClauses = append(orClauses, fmt.Sprintf("ss.%s > 0", col))
	}

	qCTE, qArgs := qMatchedCTE(f.Q)
	var args []any
	args = append(args, qArgs...)
	args = append(args, dateArgs...)
	args = append(args, limit+1, offset)

	query := fmt.Sprintf(`
		WITH %s,
		q_matched_segs AS MATERIALIZED (
			SELECT DISTINCT d.segment_id AS segment_id
			FROM q_matched qm JOIN detections d ON d.id = qm.det_id
		)
		SELECT s.id, s.camera, s.seq, s.start_time, s.trip_id, t.trip_key, s.unreadable_reason
		FROM q_matched_segs qs
		JOIN segments s ON s.id = qs.segment_id
		LEFT JOIN trips t ON t.id = s.trip_id
		WHERE EXISTS (SELECT 1 FROM segment_summary ss WHERE ss.segment_id = s.id AND (%s))%s
		ORDER BY s.start_time DESC, s.id DESC
		LIMIT ? OFFSET ?
	`, qCTE, strings.Join(orClauses, " OR "), dateClause)

	return d.querySegRows(ctx, query, args)
}

// searchSegmentsGeneral answers every non-fast-path search via a
// correlated EXISTS over detections (see matchPredicate).
func (d *DB) searchSegmentsGeneral(ctx context.Context, f SearchFilter, dateClause string, dateArgs []any, limit, offset int) ([]segRow, error) {
	cond, condArgs := matchPredicate(f)

	var withClause string
	var args []any
	if f.Q != "" {
		cte, cteArgs := qMatchedCTE(f.Q)
		withClause = "WITH " + cte + "\n"
		args = append(args, cteArgs...)
	}
	args = append(args, condArgs...)
	args = append(args, dateArgs...)
	args = append(args, limit+1, offset)

	query := fmt.Sprintf(`%s
		SELECT s.id, s.camera, s.seq, s.start_time, s.trip_id, t.trip_key, s.unreadable_reason
		FROM segments s
		LEFT JOIN trips t ON t.id = s.trip_id
		WHERE EXISTS (SELECT 1 FROM detections d WHERE d.segment_id = s.id AND (%s))%s
		ORDER BY s.start_time DESC, s.id DESC
		LIMIT ? OFFSET ?
	`, withClause, cond, dateClause)

	return d.querySegRows(ctx, query, args)
}

func (d *DB) querySegRows(ctx context.Context, query string, args []any) ([]segRow, error) {
	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: search: %w", err)
	}
	defer rows.Close()

	var segs []segRow
	for rows.Next() {
		var sr segRow
		if err := rows.Scan(&sr.id, &sr.camera, &sr.seq, &sr.startTime, &sr.tripID, &sr.tripKey, &sr.unreadableReason); err != nil {
			return nil, fmt.Errorf("db: search: %w", err)
		}
		segs = append(segs, sr)
	}
	return segs, rows.Err()
}

// searchStructuralHits resolves f's structural (class/plate/make/type)
// hits for every segment in segIDs in a single batched query — replacing
// the old per-segment searchHitsForSegment loop, which paid matchPredicate's
// CTE setup cost once per result-page segment (up to 30 round trips) for
// what should be a single indexed pass, the same reasoning that motivated
// batching q's hits in searchQHits.
//
// Task 2: multiple frames of the same followed object/plate/vehicle are
// collapsed into a single "chip" per det_tracks track (LEFT JOIN det_tracks
// on track_id) instead of one hit per matching frame — GROUP BY a key that
// is the track id when track_id is set, or the raw det_objects/det_plates/
// det_vehicles row id when it's NULL (pre-tracking-backfill data, where
// each frame still stands alone). The chip's pts_offset/confidence/text
// prefer the track's first_pts/best_conf/best_text over the individual
// row's own columns; SearchHit.Count carries how many raw frames the chip
// represents (the track's n_frames, or 1 when ungrouped).
//
// Capped at maxStructuralHitsPerSegment distinct chips per segment (a
// window function over the whole result page, not a per-segment query);
// truncated[segID] is true when a segment had more than that.
func (d *DB) searchStructuralHits(ctx context.Context, f SearchFilter, kinds []string, segIDs []int64) (hits map[int64][]SearchHit, truncated map[int64]bool, err error) {
	if len(segIDs) == 0 {
		return nil, nil, nil
	}

	kindSet := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}
	if !kindSet["class"] && !kindSet["plate"] && !kindSet["make"] && !kindSet["type"] {
		return nil, nil, nil
	}

	// f.Q is deliberately excluded from the predicate used here — its own
	// hits are fetched separately and batched across the whole result page
	// (see searchQHits); folding Q back in here would force q_matched's
	// CTE to re-run q's FTS MATCH scan as part of this query too, on top
	// of searchQHits already doing it once.
	fStructural := f
	fStructural.Q = ""
	cond, condArgs := matchPredicate(fStructural)

	idPlaceholders := strings.TrimRight(strings.Repeat("?,", len(segIDs)), ",")
	var args []any
	for _, id := range segIDs {
		args = append(args, id)
	}
	args = append(args, condArgs...)

	// Fix round 1: when MinConf is set, a chip must not advertise a
	// det_tracks track's overall best_conf/first_pts — that track-wide
	// "best ever seen" value can come from a frame that never itself
	// cleared MinConf (matchPredicate's confidence check is per-row, not
	// per-track), which would show a filtered search as if it found a
	// higher-confidence/earlier-time hit than it actually matched. In that
	// case, fall back to the matched row's own confidence/pts_offset —
	// grouping by track still collapses the chip, but the two attributes
	// only ever come from rows that actually satisfied the filter.
	useTrackAttrs := f.MinConf <= 0
	classAttrs := "COALESCE(dt.best_conf, o.confidence, 0) AS confidence, COALESCE(dt.first_pts, m.pts_offset) AS pts_offset"
	plateAttrs := "COALESCE(dt.best_conf, p.confidence, 0) AS confidence, COALESCE(dt.first_pts, m.pts_offset) AS pts_offset"
	vehicleAttrs := "COALESCE(dt.best_conf, v.confidence, 0) AS confidence, COALESCE(dt.first_pts, m.pts_offset) AS pts_offset"
	if !useTrackAttrs {
		classAttrs = "COALESCE(o.confidence, 0) AS confidence, m.pts_offset AS pts_offset"
		plateAttrs = "COALESCE(p.confidence, 0) AS confidence, m.pts_offset AS pts_offset"
		vehicleAttrs = "COALESCE(v.confidence, 0) AS confidence, m.pts_offset AS pts_offset"
	}

	var branches []string
	if kindSet["class"] {
		branch := fmt.Sprintf(`SELECT m.segment_id AS segment_id, 'class' AS kind, o.class AS text,
				%s,
				CASE WHEN o.track_id IS NOT NULL THEN 'ot' || o.track_id ELSE 'oi' || o.id END AS group_key
			FROM matched m JOIN det_objects o ON o.det_id = m.det_id
			LEFT JOIN det_tracks dt ON dt.id = o.track_id`, classAttrs)
		if len(f.Classes) > 0 {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(f.Classes)), ",")
			branch += fmt.Sprintf(" WHERE o.class IN (%s)", placeholders)
			for _, c := range f.Classes {
				args = append(args, c)
			}
		}
		branches = append(branches, branch)
	}
	if kindSet["plate"] {
		branch := fmt.Sprintf(`SELECT m.segment_id AS segment_id, 'plate' AS kind, COALESCE(dt.best_text, p.text) AS text,
				%s,
				CASE WHEN p.track_id IS NOT NULL THEN 'pt' || p.track_id ELSE 'pi' || p.id END AS group_key
			FROM matched m JOIN det_plates p ON p.det_id = m.det_id
			LEFT JOIN det_tracks dt ON dt.id = p.track_id`, plateAttrs)
		if f.Plate != "" {
			branch += ` WHERE p.text LIKE ? ESCAPE '\'`
			args = append(args, "%"+escapeLike(f.Plate)+"%")
		}
		branches = append(branches, branch)
	}
	if kindSet["make"] {
		branch := fmt.Sprintf(`SELECT m.segment_id AS segment_id, 'make' AS kind, v.make AS text,
				%s,
				CASE WHEN v.track_id IS NOT NULL THEN 'mt' || v.track_id ELSE 'mi' || v.id END AS group_key
			FROM matched m JOIN det_vehicles v ON v.det_id = m.det_id
			LEFT JOIN det_tracks dt ON dt.id = v.track_id
			WHERE v.make IS NOT NULL AND v.make != ''`, vehicleAttrs)
		if f.Make != "" {
			branch += ` AND v.make = ?`
			args = append(args, f.Make)
		}
		branches = append(branches, branch)
	}
	if kindSet["type"] {
		branch := fmt.Sprintf(`SELECT m.segment_id AS segment_id, 'type' AS kind, v.type AS text,
				%s,
				CASE WHEN v.track_id IS NOT NULL THEN 'vt' || v.track_id ELSE 'vi' || v.id END AS group_key
			FROM matched m JOIN det_vehicles v ON v.det_id = m.det_id
			LEFT JOIN det_tracks dt ON dt.id = v.track_id
			WHERE v.type IS NOT NULL AND v.type != ''`, vehicleAttrs)
		if f.Type != "" {
			branch += ` AND v.type = ?`
			args = append(args, f.Type)
		}
		branches = append(branches, branch)
	}

	args = append(args, maxStructuralHitsPerSegment)

	query := fmt.Sprintf(`
		WITH matched AS (
			SELECT d.id AS det_id, d.segment_id AS segment_id, d.pts_offset AS pts_offset
			FROM detections d
			WHERE d.segment_id IN (%s) AND (%s)
		),
		grouped AS (
			SELECT segment_id, kind, text, group_key,
				MAX(confidence) AS confidence,
				MIN(pts_offset) AS pts_offset,
				COUNT(*) AS frame_count
			FROM (%s)
			GROUP BY segment_id, kind, text, group_key
		),
		ranked AS (
			SELECT segment_id, kind, text, confidence, pts_offset, frame_count, group_key,
				ROW_NUMBER() OVER (PARTITION BY segment_id ORDER BY pts_offset, group_key) AS rn,
				COUNT(*) OVER (PARTITION BY segment_id) AS total
			FROM grouped
		)
		SELECT segment_id, pts_offset, kind, text, confidence, frame_count, total
		FROM ranked
		WHERE rn <= ?
		ORDER BY segment_id, pts_offset, group_key
	`, idPlaceholders, cond, strings.Join(branches, "\nUNION ALL\n"))

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("db: search structural hits: %w", err)
	}
	defer rows.Close()

	hits = make(map[int64][]SearchHit)
	truncated = make(map[int64]bool)
	for rows.Next() {
		var segID int64
		var h SearchHit
		var total int
		if err := rows.Scan(&segID, &h.PtsOffset, &h.Kind, &h.Text, &h.Confidence, &h.Count, &total); err != nil {
			return nil, nil, fmt.Errorf("db: search structural hits: %w", err)
		}
		hits[segID] = append(hits[segID], h)
		if total > maxStructuralHitsPerSegment {
			truncated[segID] = true
		}
	}
	return hits, truncated, rows.Err()
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
