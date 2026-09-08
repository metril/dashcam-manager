package db

import (
	"bytes"
	"compress/gzip"
	"context"
	"testing"
)

// mustGzip gzips raw for use as a detections_raw.gz fixture value.
func mustGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// seedBrowseFixture inserts one source, two trips (one with front+rear
// segments and detections, one front-only) on different dates, and returns
// their ids.
func seedBrowseFixture(t *testing.T, d *DB) (sourceID, trip1, trip2 int64) {
	t.Helper()
	ctx := context.Background()

	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	sourceID = must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)

	trip1 = must(`INSERT INTO trips (source_id, trip_key, started_at, ended_at, duration_s, front_dir, rear_dir, segment_count, bytes)
		VALUES (?, 'recording_20260301_100000', '2026-03-01T10:00:00Z', '2026-03-01T10:05:00Z', 300, 'recording_20260301_100000', 'recording_20260301_100002', 2, 2000)`, sourceID)
	seg1 := must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/01/recording_20260301_100000/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', '2026-03-01T10:00:30Z', 30, 1000, 1, 0)`, sourceID, trip1)
	must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'rear', 'rear/2026/03/01/recording_20260301_100002/segment_00000.mp4', 0, '2026-03-01T10:00:02Z', '2026-03-01T10:00:32Z', 30, 1000, 0, 0)`, sourceID, trip1)
	must(`INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates, top_classes)
		VALUES (?, 3, 1, 0, 2, 0, 0, 1, '["car","person"]')`, seg1)
	must(`INSERT INTO detections_raw (segment_id, gz) VALUES (?, ?)`, seg1, gzipFixture(t))

	trip2 = must(`INSERT INTO trips (source_id, trip_key, started_at, ended_at, duration_s, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260302_080000', '2026-03-02T08:00:00Z', '2026-03-02T08:01:00Z', 60, 'recording_20260302_080000', 1, 900)`, sourceID)
	must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/02/recording_20260302_080000/segment_00000.mp4', 0, '2026-03-02T08:00:00Z', '2026-03-02T08:00:30Z', 30, 900, 0, 0)`, sourceID, trip2)

	return sourceID, trip1, trip2
}

// gzipFixture returns a small gzipped .detections.json body.
func gzipFixture(t *testing.T) []byte {
	t.Helper()
	raw := []byte(`{"segment":"segment_00000.mp4","camera":"front","frame_width":640,"frame_height":640,
		"detections":[{"timestamp":0.5,"frame_index":0,"pts_offset":0.5,
			"objects":[{"class":"car","confidence":0.9,"bbox":[64,64,320,320]}]}]}`)
	return mustGzip(t, raw)
}

func TestCalendarRange(t *testing.T) {
	d := openTest(t)
	seedBrowseFixture(t, d)
	ctx := context.Background()

	days, err := d.CalendarRange(ctx, "2026-03-01", "2026-03-02")
	if err != nil {
		t.Fatalf("CalendarRange: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2: %+v", len(days), days)
	}
	if days[0].Date != "2026-03-01" || days[0].Trips != 1 || days[0].Segments != 2 || days[0].Bytes != 2000 {
		t.Fatalf("day0 = %+v", days[0])
	}
	if days[0].Objects != 6 { // 3 car + 1 truck + 2 person
		t.Fatalf("day0 objects = %d, want 6", days[0].Objects)
	}
	if days[1].Date != "2026-03-02" || days[1].Trips != 1 || days[1].Segments != 1 {
		t.Fatalf("day1 = %+v", days[1])
	}

	empty, err := d.CalendarRange(ctx, "2026-01-01", "2026-01-02")
	if err != nil {
		t.Fatalf("CalendarRange empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no days, got %+v", empty)
	}
}

func TestTripsListByDate(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	cards, cursor, err := d.TripsList(ctx, TripsFilter{Date: "2026-03-01"})
	if err != nil {
		t.Fatalf("TripsList: %v", err)
	}
	if cursor != "" {
		t.Fatalf("expected no next cursor, got %q", cursor)
	}
	if len(cards) != 1 {
		t.Fatalf("got %d trips, want 1: %+v", len(cards), cards)
	}
	c := cards[0]
	if c.ID != trip1 {
		t.Fatalf("id = %d, want %d", c.ID, trip1)
	}
	if !c.Front || !c.Rear {
		t.Fatalf("front/rear = %v/%v, want true/true", c.Front, c.Rear)
	}
	if c.Summary["car"] != 3 || c.Summary["person"] != 2 || c.Summary["truck"] != 1 {
		t.Fatalf("summary = %+v", c.Summary)
	}
	if _, ok := c.Summary["bus"]; ok {
		t.Fatalf("summary should omit zero-count classes: %+v", c.Summary)
	}
}

func TestTripsListByRangeAndPaging(t *testing.T) {
	d := openTest(t)
	seedBrowseFixture(t, d)
	ctx := context.Background()

	cards, cursor, err := d.TripsList(ctx, TripsFilter{From: "2026-03-01", To: "2026-03-02", Limit: 1})
	if err != nil {
		t.Fatalf("TripsList: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("got %d trips, want 1 (page size)", len(cards))
	}
	if cursor == "" {
		t.Fatalf("expected a next cursor for a partial page")
	}

	cards2, cursor2, err := d.TripsList(ctx, TripsFilter{From: "2026-03-01", To: "2026-03-02", Limit: 1, Cursor: cursor})
	if err != nil {
		t.Fatalf("TripsList page 2: %v", err)
	}
	if len(cards2) != 1 {
		t.Fatalf("got %d trips on page 2, want 1", len(cards2))
	}
	if cursor2 != "" {
		t.Fatalf("expected no next cursor after last page, got %q", cursor2)
	}
	if cards[0].ID == cards2[0].ID {
		t.Fatalf("pages returned the same trip twice: %d", cards[0].ID)
	}
}

func TestTripGet(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	trip, err := d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	if trip.PairOffsetS != nil {
		t.Fatalf("pair_offset_s = %v, want nil (not seeded)", *trip.PairOffsetS)
	}
	if len(trip.Segments) != 2 {
		t.Fatalf("got %d segments, want 2: %+v", len(trip.Segments), trip.Segments)
	}
	if trip.Segments[0].Camera != "front" || trip.Segments[1].Camera != "rear" {
		t.Fatalf("segment order/camera = %+v", trip.Segments)
	}
	if !trip.Segments[0].HasDetections {
		t.Fatalf("front segment should have has_detections=true")
	}
	if trip.Segments[0].Summary["car"] != 3 {
		t.Fatalf("front segment summary = %+v", trip.Segments[0].Summary)
	}

	_, err = d.TripGet(ctx, 999999)
	if err != ErrNotFound {
		t.Fatalf("TripGet(missing) err = %v, want ErrNotFound", err)
	}
}

func TestSegmentGetAndDetectionsRaw(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	trip, err := d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	frontSegID := trip.Segments[0].ID

	meta, err := d.SegmentGet(ctx, frontSegID)
	if err != nil {
		t.Fatalf("SegmentGet: %v", err)
	}
	if meta.Camera != "front" || meta.Bytes != 1000 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Summary["car"] != 3 {
		t.Fatalf("meta summary = %+v", meta.Summary)
	}

	gz, err := d.SegmentDetectionsRaw(ctx, frontSegID)
	if err != nil {
		t.Fatalf("SegmentDetectionsRaw: %v", err)
	}
	if len(gz) == 0 {
		t.Fatalf("expected non-empty gz")
	}

	rearSegID := trip.Segments[1].ID
	_, err = d.SegmentDetectionsRaw(ctx, rearSegID)
	if err != ErrNotFound {
		t.Fatalf("SegmentDetectionsRaw(no detections) err = %v, want ErrNotFound", err)
	}

	_, err = d.SegmentGet(ctx, 999999)
	if err != ErrNotFound {
		t.Fatalf("SegmentGet(missing) err = %v, want ErrNotFound", err)
	}
}

func TestStats(t *testing.T) {
	d := openTest(t)
	seedBrowseFixture(t, d)
	ctx := context.Background()

	stats, err := d.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Segments != 3 {
		t.Fatalf("segments = %d, want 3", stats.Segments)
	}
	if stats.Trips != 2 {
		t.Fatalf("trips = %d, want 2", stats.Trips)
	}
	if stats.Bytes != 2900 {
		t.Fatalf("bytes = %d, want 2900", stats.Bytes)
	}
	if stats.ByCamera["front"] != 2 || stats.ByCamera["rear"] != 1 {
		t.Fatalf("by_camera = %+v", stats.ByCamera)
	}
	if stats.ByClass["car"] != 3 || stats.ByClass["person"] != 2 {
		t.Fatalf("by_class = %+v", stats.ByClass)
	}
	if len(stats.ByMonth) != 1 || stats.ByMonth[0].Month != "2026-03" || stats.ByMonth[0].Segments != 3 {
		t.Fatalf("by_month = %+v", stats.ByMonth)
	}
}

// TestCalendarRangeIsHalfOpenAtBoundaries pins the sargable started_at
// range rewrite: a trip started right at 23:59:59 on the last day of range
// must still be included, and one on the day after must be excluded.
func TestCalendarRangeIsHalfOpenAtBoundaries(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	must(`INSERT INTO trips (source_id, trip_key, started_at, segment_count, bytes)
		VALUES (?, 'edge_in', '2026-03-05T23:59:59.999Z', 1, 100)`, sourceID)
	must(`INSERT INTO trips (source_id, trip_key, started_at, segment_count, bytes)
		VALUES (?, 'edge_out', '2026-03-06T00:00:00.000Z', 1, 100)`, sourceID)

	days, err := d.CalendarRange(ctx, "2026-03-05", "2026-03-05")
	if err != nil {
		t.Fatalf("CalendarRange: %v", err)
	}
	if len(days) != 1 || days[0].Date != "2026-03-05" || days[0].Trips != 1 {
		t.Fatalf("days = %+v, want exactly 2026-03-05 with 1 trip", days)
	}
}

// TestTripsListDateFilterIsHalfOpen mirrors the calendar boundary check for
// the ?date= filter's sargable started_at range.
func TestTripsListDateFilterIsHalfOpen(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	inID := must(`INSERT INTO trips (source_id, trip_key, started_at, segment_count, bytes)
		VALUES (?, 'edge_in', '2026-03-05T23:59:59.999Z', 1, 100)`, sourceID)
	must(`INSERT INTO trips (source_id, trip_key, started_at, segment_count, bytes)
		VALUES (?, 'edge_out', '2026-03-06T00:00:00.000Z', 1, 100)`, sourceID)

	cards, _, err := d.TripsList(ctx, TripsFilter{Date: "2026-03-05"})
	if err != nil {
		t.Fatalf("TripsList: %v", err)
	}
	if len(cards) != 1 || cards[0].ID != inID {
		t.Fatalf("cards = %+v, want only trip %d", cards, inID)
	}
}
