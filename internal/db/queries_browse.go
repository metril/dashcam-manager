package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dateOnlyLayout is the YYYY-MM-DD format used throughout the browse
// queries for date-range filters.
const dateOnlyLayout = "2006-01-02"

// dayRange turns a YYYY-MM-DD date string into a half-open [start, end)
// pair of YYYY-MM-DD strings (end being the following day) suitable for a
// sargable `started_at >= start AND started_at < end` filter: started_at
// values are ISO8601 timestamps ("2026-03-01T05:16:54.947Z"), and since 'T'
// sorts after every digit, a bare date string compares as less than any
// timestamp on that same date under ordinary TEXT (byte-value) comparison —
// so this avoids wrapping the indexed column in date(...), which would
// otherwise force a full scan instead of using idx_trips_started_at /
// idx_segments_start_time.
func dayRange(date string) (start, end string, err error) {
	t, err := time.Parse(dateOnlyLayout, date)
	if err != nil {
		return "", "", fmt.Errorf("invalid date %q: %w", date, err)
	}
	return t.Format(dateOnlyLayout), t.AddDate(0, 0, 1).Format(dateOnlyLayout), nil
}

// CalendarDay is one day's aggregate stats for GET /api/calendar.
type CalendarDay struct {
	Date     string `json:"date"` // YYYY-MM-DD
	Trips    int    `json:"trips"`
	Segments int    `json:"segments"`
	Bytes    int64  `json:"bytes"`
	Objects  int    `json:"objects"`
}

// CalendarRange returns one CalendarDay per calendar date (UTC, taken from
// trips.started_at) in [from, to] that has at least one trip. from/to are
// YYYY-MM-DD strings. "objects" excludes plate detections (car/truck/bus/
// person/bicycle/motorcycle only), matching the frontend's StatsStrip.
func (d *DB) CalendarRange(ctx context.Context, from, to string) ([]CalendarDay, error) {
	start, _, err := dayRange(from)
	if err != nil {
		return nil, fmt.Errorf("db: calendar range: %w", err)
	}
	_, end, err := dayRange(to)
	if err != nil {
		return nil, fmt.Errorf("db: calendar range: %w", err)
	}

	// Single LEFT JOIN against a per-trip object-count subquery (already
	// grouped by trip_id, so it's a 1:1 join with no fan-out), then one
	// GROUP BY date(...) pass over that — not a per-row correlated
	// subquery. The started_at bound stays sargable (no date(...) wrapping
	// the indexed column); GROUP BY still needs date() to bucket by day,
	// but that no longer prevents the range scan from using
	// idx_trips_started_at.
	rows, err := d.read.QueryContext(ctx, `
		SELECT date(t.started_at) AS day,
			COUNT(DISTINCT t.id) AS trips,
			COALESCE(SUM(t.segment_count), 0) AS segments,
			COALESCE(SUM(t.bytes), 0) AS bytes,
			COALESCE(SUM(agg.objects), 0) AS objects
		FROM trips t
		LEFT JOIN (
			SELECT s.trip_id AS trip_id,
				SUM(ss.n_car + ss.n_truck + ss.n_bus + ss.n_person + ss.n_bicycle + ss.n_motorcycle) AS objects
			FROM segments s
			JOIN segment_summary ss ON ss.segment_id = s.id
			GROUP BY s.trip_id
		) agg ON agg.trip_id = t.id
		WHERE t.started_at >= ? AND t.started_at < ?
		GROUP BY day
		ORDER BY day
	`, start, end)
	if err != nil {
		return nil, fmt.Errorf("db: calendar range: %w", err)
	}
	defer rows.Close()

	var out []CalendarDay
	for rows.Next() {
		var c CalendarDay
		if err := rows.Scan(&c.Date, &c.Trips, &c.Segments, &c.Bytes, &c.Objects); err != nil {
			return nil, fmt.Errorf("db: calendar range: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TripCard is one trip's summary for GET /api/trips.
type TripCard struct {
	ID           int64          `json:"id"`
	TripKey      string         `json:"trip_key"`
	StartedAt    string         `json:"started_at"`
	EndedAt      *string        `json:"ended_at"`
	DurationS    *float64       `json:"duration_s"`
	SegmentCount int            `json:"segment_count"`
	Bytes        int64          `json:"bytes"`
	DistanceM    *float64       `json:"distance_m"`
	MaxSpeedMps  *float64       `json:"max_speed_mps"`
	Front        bool           `json:"front"`
	Rear         bool           `json:"rear"`
	Summary      map[string]int `json:"summary"`
	CoverThumb   *string        `json:"cover_thumb"`
}

// TripsFilter selects trips either by a single date (Date) or an inclusive
// [From, To] date range; Date takes precedence when non-empty. Cursor is an
// opaque offset token from a previous page's NextCursor (empty for the first
// page); Limit caps the page size (callers should apply their own default).
type TripsFilter struct {
	Date   string
	From   string
	To     string
	Cursor string
	Limit  int
}

// TripsList returns a page of trips (most recent first) matching filter,
// plus a cursor for the next page (empty when there are no more).
func (d *DB) TripsList(ctx context.Context, filter TripsFilter) ([]TripCard, string, error) {
	offset := 0
	if filter.Cursor != "" {
		n, err := strconv.Atoi(filter.Cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("db: trips list: invalid cursor %q", filter.Cursor)
		}
		offset = n
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	var where string
	var args []any
	switch {
	case filter.Date != "":
		start, end, err := dayRange(filter.Date)
		if err != nil {
			return nil, "", fmt.Errorf("db: trips list: %w", err)
		}
		where = "WHERE t.started_at >= ? AND t.started_at < ?"
		args = append(args, start, end)
	case filter.From != "" && filter.To != "":
		start, _, err := dayRange(filter.From)
		if err != nil {
			return nil, "", fmt.Errorf("db: trips list: %w", err)
		}
		_, end, err := dayRange(filter.To)
		if err != nil {
			return nil, "", fmt.Errorf("db: trips list: %w", err)
		}
		where = "WHERE t.started_at >= ? AND t.started_at < ?"
		args = append(args, start, end)
	}

	query := fmt.Sprintf(`
		SELECT t.id, t.trip_key, t.started_at, t.ended_at, t.duration_s, t.segment_count,
			t.bytes, t.distance_m, t.max_speed_mps, t.front_dir, t.rear_dir, t.cover_thumb
		FROM trips t
		%s
		ORDER BY t.started_at DESC, t.id DESC
		LIMIT ? OFFSET ?
	`, where)
	args = append(args, limit+1, offset)

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("db: trips list: %w", err)
	}
	defer rows.Close()

	var cards []TripCard
	var frontDir, rearDir sql.NullString
	for rows.Next() {
		var c TripCard
		if err := rows.Scan(&c.ID, &c.TripKey, &c.StartedAt, &c.EndedAt, &c.DurationS, &c.SegmentCount,
			&c.Bytes, &c.DistanceM, &c.MaxSpeedMps, &frontDir, &rearDir, &c.CoverThumb); err != nil {
			return nil, "", fmt.Errorf("db: trips list: %w", err)
		}
		c.Front = frontDir.Valid && frontDir.String != ""
		c.Rear = rearDir.Valid && rearDir.String != ""
		cards = append(cards, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("db: trips list: %w", err)
	}

	nextCursor := ""
	if len(cards) > limit {
		cards = cards[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}

	if err := d.attachTripSummaries(ctx, cards); err != nil {
		return nil, "", err
	}

	return cards, nextCursor, nil
}

// attachTripSummaries batch-loads per-class object counts for the given
// trip cards (mutating Summary in place) in a single query, keyed by
// segments.trip_id joined to segment_summary.
func (d *DB) attachTripSummaries(ctx context.Context, cards []TripCard) error {
	if len(cards) == 0 {
		return nil
	}
	ids := make([]any, len(cards))
	placeholders := make([]string, len(cards))
	byID := make(map[int64]*TripCard, len(cards))
	for i := range cards {
		ids[i] = cards[i].ID
		placeholders[i] = "?"
		byID[cards[i].ID] = &cards[i]
		cards[i].Summary = map[string]int{}
	}

	query := fmt.Sprintf(`
		SELECT s.trip_id,
			COALESCE(SUM(ss.n_car), 0), COALESCE(SUM(ss.n_truck), 0), COALESCE(SUM(ss.n_bus), 0),
			COALESCE(SUM(ss.n_person), 0), COALESCE(SUM(ss.n_bicycle), 0), COALESCE(SUM(ss.n_motorcycle), 0),
			COALESCE(SUM(ss.n_plates), 0)
		FROM segments s
		JOIN segment_summary ss ON ss.segment_id = s.id
		WHERE s.trip_id IN (%s)
		GROUP BY s.trip_id
	`, strings.Join(placeholders, ","))

	rows, err := d.read.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("db: trip summaries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var tripID int64
		var car, truck, bus, person, bicycle, motorcycle, plates int
		if err := rows.Scan(&tripID, &car, &truck, &bus, &person, &bicycle, &motorcycle, &plates); err != nil {
			return fmt.Errorf("db: trip summaries: %w", err)
		}
		c, ok := byID[tripID]
		if !ok {
			continue
		}
		setIfPositive(c.Summary, "car", car)
		setIfPositive(c.Summary, "truck", truck)
		setIfPositive(c.Summary, "bus", bus)
		setIfPositive(c.Summary, "person", person)
		setIfPositive(c.Summary, "bicycle", bicycle)
		setIfPositive(c.Summary, "motorcycle", motorcycle)
		setIfPositive(c.Summary, "plate", plates)
	}
	return rows.Err()
}

func setIfPositive(m map[string]int, key string, v int) {
	if v > 0 {
		m[key] = v
	}
}

// TripSegment is one segment row within a trip, for GET /api/trips/{id}.
type TripSegment struct {
	ID            int64          `json:"id"`
	Camera        string         `json:"camera"`
	Seq           int            `json:"seq"`
	StartTime     string         `json:"start_time"`
	EndTime       *string        `json:"end_time"`
	DurationS     *float64       `json:"duration_s"`
	Bytes         int64          `json:"bytes"`
	HasDetections bool           `json:"has_detections"`
	HasGPS        bool           `json:"has_gps"`
	Summary       map[string]int `json:"summary,omitempty"`
	// ArchivedAt/ArchivedToShare mirror db.SegmentMedia's fields (see
	// queries_media.go) — non-nil once a re-scan found this segment's file
	// gone from its source with a verified ingests row backing it. Lets
	// the browse UI show an "archived" badge and the player show
	// "archived to <share>" up front instead of only discovering it via a
	// 404 from GET /api/segments/{id}/media.
	ArchivedAt      *string `json:"archived_at,omitempty"`
	ArchivedToShare *string `json:"archived_to_share,omitempty"`
}

// TripDetail is the full trip + its segments for GET /api/trips/{id}.
type TripDetail struct {
	TripCard
	PairOffsetS *float64      `json:"pair_offset_s"`
	Segments    []TripSegment `json:"segments"`
}

// TripGet returns the trip with the given id, its segments (ordered by
// camera then seq), and its pair_offset_s, or ErrNotFound.
func (d *DB) TripGet(ctx context.Context, id int64) (TripDetail, error) {
	var t TripDetail
	var frontDir, rearDir sql.NullString
	err := d.read.QueryRowContext(ctx, `
		SELECT t.id, t.trip_key, t.started_at, t.ended_at, t.duration_s, t.segment_count,
			t.bytes, t.distance_m, t.max_speed_mps, t.front_dir, t.rear_dir, t.cover_thumb, t.pair_offset_s
		FROM trips t WHERE t.id = ?
	`, id).Scan(&t.ID, &t.TripKey, &t.StartedAt, &t.EndedAt, &t.DurationS, &t.SegmentCount,
		&t.Bytes, &t.DistanceM, &t.MaxSpeedMps, &frontDir, &rearDir, &t.CoverThumb, &t.PairOffsetS)
	if errors.Is(err, sql.ErrNoRows) {
		return TripDetail{}, ErrNotFound
	}
	if err != nil {
		return TripDetail{}, fmt.Errorf("db: trip get %d: %w", id, err)
	}
	t.Front = frontDir.Valid && frontDir.String != ""
	t.Rear = rearDir.Valid && rearDir.String != ""

	cards := []TripCard{t.TripCard}
	if err := d.attachTripSummaries(ctx, cards); err != nil {
		return TripDetail{}, err
	}
	t.TripCard = cards[0]

	rows, err := d.read.QueryContext(ctx, `
		SELECT s.id, s.camera, s.seq, s.start_time, s.end_time, s.duration_s, s.bytes,
			s.has_detections, s.has_gps, s.archived_at,
			(SELECT sh.name FROM ingests i JOIN shares sh ON sh.id = i.share_id
			 WHERE i.segment_id = s.id AND i.verified_at IS NOT NULL
			 ORDER BY i.verified_at LIMIT 1),
			ss.n_car, ss.n_truck, ss.n_bus, ss.n_person, ss.n_bicycle, ss.n_motorcycle, ss.n_plates
		FROM segments s
		LEFT JOIN segment_summary ss ON ss.segment_id = s.id
		WHERE s.trip_id = ?
		ORDER BY s.camera, s.seq
	`, id)
	if err != nil {
		return TripDetail{}, fmt.Errorf("db: trip segments %d: %w", id, err)
	}
	defer rows.Close()

	for rows.Next() {
		var seg TripSegment
		var hasDet, hasGPS int
		var car, truck, bus, person, bicycle, motorcycle, plates sql.NullInt64
		if err := rows.Scan(&seg.ID, &seg.Camera, &seg.Seq, &seg.StartTime, &seg.EndTime, &seg.DurationS, &seg.Bytes,
			&hasDet, &hasGPS, &seg.ArchivedAt, &seg.ArchivedToShare,
			&car, &truck, &bus, &person, &bicycle, &motorcycle, &plates); err != nil {
			return TripDetail{}, fmt.Errorf("db: trip segments %d: %w", id, err)
		}
		seg.HasDetections = hasDet != 0
		seg.HasGPS = hasGPS != 0
		summary := map[string]int{}
		setIfPositive(summary, "car", int(car.Int64))
		setIfPositive(summary, "truck", int(truck.Int64))
		setIfPositive(summary, "bus", int(bus.Int64))
		setIfPositive(summary, "person", int(person.Int64))
		setIfPositive(summary, "bicycle", int(bicycle.Int64))
		setIfPositive(summary, "motorcycle", int(motorcycle.Int64))
		setIfPositive(summary, "plate", int(plates.Int64))
		if len(summary) > 0 {
			seg.Summary = summary
		}
		t.Segments = append(t.Segments, seg)
	}
	return t, rows.Err()
}

// SegmentMeta is a single segment's metadata for GET /api/segments/{id}.
type SegmentMeta struct {
	ID            int64          `json:"id"`
	TripID        *int64         `json:"trip_id"`
	Camera        string         `json:"camera"`
	RelPath       string         `json:"rel_path"`
	Seq           int            `json:"seq"`
	StartTime     string         `json:"start_time"`
	EndTime       *string        `json:"end_time"`
	DurationS     *float64       `json:"duration_s"`
	Bytes         int64          `json:"bytes"`
	Width         *int           `json:"width"`
	Height        *int           `json:"height"`
	HasDetections bool           `json:"has_detections"`
	HasGPS        bool           `json:"has_gps"`
	Summary       map[string]int `json:"summary,omitempty"`
	// ArchivedAt/ArchivedToShare: see TripSegment's identically-named
	// fields above.
	ArchivedAt      *string `json:"archived_at,omitempty"`
	ArchivedToShare *string `json:"archived_to_share,omitempty"`
}

// SegmentGet returns metadata for the segment with the given id, or
// ErrNotFound.
func (d *DB) SegmentGet(ctx context.Context, id int64) (SegmentMeta, error) {
	var s SegmentMeta
	var hasDet, hasGPS int
	var car, truck, bus, person, bicycle, motorcycle, plates sql.NullInt64
	err := d.read.QueryRowContext(ctx, `
		SELECT s.id, s.trip_id, s.camera, s.rel_path, s.seq, s.start_time, s.end_time, s.duration_s, s.bytes,
			s.width, s.height, s.has_detections, s.has_gps, s.archived_at,
			(SELECT sh.name FROM ingests i JOIN shares sh ON sh.id = i.share_id
			 WHERE i.segment_id = s.id AND i.verified_at IS NOT NULL
			 ORDER BY i.verified_at LIMIT 1),
			ss.n_car, ss.n_truck, ss.n_bus, ss.n_person, ss.n_bicycle, ss.n_motorcycle, ss.n_plates
		FROM segments s
		LEFT JOIN segment_summary ss ON ss.segment_id = s.id
		WHERE s.id = ?
	`, id).Scan(&s.ID, &s.TripID, &s.Camera, &s.RelPath, &s.Seq, &s.StartTime, &s.EndTime, &s.DurationS, &s.Bytes,
		&s.Width, &s.Height, &hasDet, &hasGPS, &s.ArchivedAt, &s.ArchivedToShare,
		&car, &truck, &bus, &person, &bicycle, &motorcycle, &plates)
	if errors.Is(err, sql.ErrNoRows) {
		return SegmentMeta{}, ErrNotFound
	}
	if err != nil {
		return SegmentMeta{}, fmt.Errorf("db: segment get %d: %w", id, err)
	}
	s.HasDetections = hasDet != 0
	s.HasGPS = hasGPS != 0
	summary := map[string]int{}
	setIfPositive(summary, "car", int(car.Int64))
	setIfPositive(summary, "truck", int(truck.Int64))
	setIfPositive(summary, "bus", int(bus.Int64))
	setIfPositive(summary, "person", int(person.Int64))
	setIfPositive(summary, "bicycle", int(bicycle.Int64))
	setIfPositive(summary, "motorcycle", int(motorcycle.Int64))
	setIfPositive(summary, "plate", int(plates.Int64))
	if len(summary) > 0 {
		s.Summary = summary
	}
	return s, nil
}

// SegmentDetectionsRaw returns the gzipped raw .detections.json bytes for a
// segment, or ErrNotFound if the segment has no detections_raw row.
func (d *DB) SegmentDetectionsRaw(ctx context.Context, id int64) ([]byte, error) {
	var gz []byte
	err := d.read.QueryRowContext(ctx,
		`SELECT gz FROM detections_raw WHERE segment_id = ?`, id).Scan(&gz)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: segment detections raw %d: %w", id, err)
	}
	return gz, nil
}

// Stats is the aggregate library summary for GET /api/stats.
type Stats struct {
	Segments        int64            `json:"segments"`
	Trips           int64            `json:"trips"`
	Bytes           int64            `json:"bytes"`
	ByMonth         []StatsByMonth   `json:"by_month"`
	ByCamera        map[string]int64 `json:"by_camera"`
	ByClass         map[string]int64 `json:"by_class"`
	BySource        []StatsBySource  `json:"by_source"`
	IngestedPercent float64          `json:"ingested_percent"`
}

// StatsBySource is one source's (SSD or a mounted share) segment/byte
// totals, for the storage-per-source/share panel.
type StatsBySource struct {
	SourceID int64  `json:"source_id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Segments int64  `json:"segments"`
	Bytes    int64  `json:"bytes"`
}

// StatsByMonth is one calendar month's segment/byte totals.
type StatsByMonth struct {
	Month    string `json:"month"` // YYYY-MM
	Bytes    int64  `json:"bytes"`
	Segments int64  `json:"segments"`
}

// Stats computes library-wide aggregate counters. It runs a handful of
// simple aggregate queries rather than one large join, since segments,
// trips and segment_summary have very different cardinalities.
func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	if err := d.read.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM segments`).
		Scan(&s.Segments, &s.Bytes); err != nil {
		return Stats{}, fmt.Errorf("db: stats segments: %w", err)
	}
	if err := d.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM trips`).Scan(&s.Trips); err != nil {
		return Stats{}, fmt.Errorf("db: stats trips: %w", err)
	}

	monthRows, err := d.read.QueryContext(ctx, `
		SELECT strftime('%Y-%m', start_time) AS month, COALESCE(SUM(bytes), 0), COUNT(*)
		FROM segments
		GROUP BY month
		ORDER BY month
	`)
	if err != nil {
		return Stats{}, fmt.Errorf("db: stats by month: %w", err)
	}
	defer monthRows.Close()
	for monthRows.Next() {
		var m StatsByMonth
		if err := monthRows.Scan(&m.Month, &m.Bytes, &m.Segments); err != nil {
			return Stats{}, fmt.Errorf("db: stats by month: %w", err)
		}
		s.ByMonth = append(s.ByMonth, m)
	}
	if err := monthRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("db: stats by month: %w", err)
	}
	if s.ByMonth == nil {
		s.ByMonth = []StatsByMonth{}
	}

	s.ByCamera = map[string]int64{}
	camRows, err := d.read.QueryContext(ctx, `SELECT camera, COUNT(*) FROM segments GROUP BY camera`)
	if err != nil {
		return Stats{}, fmt.Errorf("db: stats by camera: %w", err)
	}
	defer camRows.Close()
	for camRows.Next() {
		var cam string
		var n int64
		if err := camRows.Scan(&cam, &n); err != nil {
			return Stats{}, fmt.Errorf("db: stats by camera: %w", err)
		}
		s.ByCamera[cam] = n
	}
	if err := camRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("db: stats by camera: %w", err)
	}

	var car, truck, bus, person, bicycle, motorcycle, plates int64
	if err := d.read.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(n_car),0), COALESCE(SUM(n_truck),0), COALESCE(SUM(n_bus),0),
			COALESCE(SUM(n_person),0), COALESCE(SUM(n_bicycle),0), COALESCE(SUM(n_motorcycle),0),
			COALESCE(SUM(n_plates),0)
		FROM segment_summary
	`).Scan(&car, &truck, &bus, &person, &bicycle, &motorcycle, &plates); err != nil {
		return Stats{}, fmt.Errorf("db: stats by class: %w", err)
	}
	s.ByClass = map[string]int64{}
	setIfPositive64(s.ByClass, "car", car)
	setIfPositive64(s.ByClass, "truck", truck)
	setIfPositive64(s.ByClass, "bus", bus)
	setIfPositive64(s.ByClass, "person", person)
	setIfPositive64(s.ByClass, "bicycle", bicycle)
	setIfPositive64(s.ByClass, "motorcycle", motorcycle)
	setIfPositive64(s.ByClass, "plate", plates)

	srcRows, err := d.read.QueryContext(ctx, `
		SELECT src.id, src.label, src.kind, COUNT(s.id), COALESCE(SUM(s.bytes), 0)
		FROM sources src
		LEFT JOIN segments s ON s.source_id = src.id
		GROUP BY src.id
		ORDER BY src.id
	`)
	if err != nil {
		return Stats{}, fmt.Errorf("db: stats by source: %w", err)
	}
	defer srcRows.Close()
	s.BySource = []StatsBySource{}
	for srcRows.Next() {
		var b StatsBySource
		if err := srcRows.Scan(&b.SourceID, &b.Label, &b.Kind, &b.Segments, &b.Bytes); err != nil {
			return Stats{}, fmt.Errorf("db: stats by source: %w", err)
		}
		s.BySource = append(s.BySource, b)
	}
	if err := srcRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("db: stats by source: %w", err)
	}

	// Ingest coverage: fraction of segments on SSD sources that have a
	// verified ingests row (i.e. have a confirmed copy on some share).
	// Segments already on a share source don't need "ingesting" so they're
	// excluded from the denominator.
	var ssdTotal, ssdIngested int64
	if err := d.read.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN EXISTS (
				SELECT 1 FROM ingests i WHERE i.segment_id = s.id AND i.verified_at IS NOT NULL
			) THEN 1 ELSE 0 END), 0)
		FROM segments s
		JOIN sources src ON src.id = s.source_id AND src.kind = 'ssd'
	`).Scan(&ssdTotal, &ssdIngested); err != nil {
		return Stats{}, fmt.Errorf("db: stats ingest coverage: %w", err)
	}
	if ssdTotal > 0 {
		s.IngestedPercent = float64(ssdIngested) / float64(ssdTotal) * 100
	}

	return s, nil
}

func setIfPositive64(m map[string]int64, key string, v int64) {
	if v > 0 {
		m[key] = v
	}
}
