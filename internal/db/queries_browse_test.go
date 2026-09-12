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

// TestTripGetAndSegmentGetExposeArchivedAt covers residual-findings item 2:
// TripGet's segments and SegmentGet must surface archived_at and the
// archive-destination share name, so the browse UI can show an "archived"
// badge and the player can show "archived to <share>" up front instead of
// only discovering it via a 404 from GET /api/segments/{id}/media.
func TestTripGetAndSegmentGetExposeArchivedAt(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	trip, err := d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet: %v", err)
	}
	frontSegID := trip.Segments[0].ID

	// Not archived yet: both fields nil.
	if trip.Segments[0].ArchivedAt != nil {
		t.Fatalf("ArchivedAt = %v, want nil before archiving", *trip.Segments[0].ArchivedAt)
	}
	meta, err := d.SegmentGet(ctx, frontSegID)
	if err != nil {
		t.Fatalf("SegmentGet: %v", err)
	}
	if meta.ArchivedAt != nil {
		t.Fatalf("SegmentGet ArchivedAt = %v, want nil before archiving", *meta.ArchivedAt)
	}

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('backup-nas', '10.0.0.5', 'dashcam')`); err != nil {
		t.Fatalf("insert share: %v", err)
	}
	var shareID int64
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM shares WHERE name = 'backup-nas'`).Scan(&shareID); err != nil {
		t.Fatalf("query share id: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'front/x/segment_00000.mp4', 5, 'deadbeef', '2026-03-01T10:05:00Z')`,
		frontSegID, shareID); err != nil {
		t.Fatalf("insert ingests row: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`UPDATE segments SET archived_at = '2026-03-01T10:06:00Z' WHERE id = ?`, frontSegID); err != nil {
		t.Fatalf("archive segment: %v", err)
	}

	trip, err = d.TripGet(ctx, trip1)
	if err != nil {
		t.Fatalf("TripGet after archive: %v", err)
	}
	if trip.Segments[0].ArchivedAt == nil || *trip.Segments[0].ArchivedAt == "" {
		t.Fatal("expected TripGet's front segment to report ArchivedAt")
	}
	if trip.Segments[0].ArchivedToShare == nil || *trip.Segments[0].ArchivedToShare != "backup-nas" {
		t.Fatalf("ArchivedToShare = %v, want \"backup-nas\"", trip.Segments[0].ArchivedToShare)
	}
	// The rear segment (never archived) must still report nil.
	if trip.Segments[1].ArchivedAt != nil {
		t.Fatalf("rear segment ArchivedAt = %v, want nil (never archived)", *trip.Segments[1].ArchivedAt)
	}

	meta, err = d.SegmentGet(ctx, frontSegID)
	if err != nil {
		t.Fatalf("SegmentGet after archive: %v", err)
	}
	if meta.ArchivedAt == nil || *meta.ArchivedAt == "" {
		t.Fatal("expected SegmentGet to report ArchivedAt")
	}
	if meta.ArchivedToShare == nil || *meta.ArchivedToShare != "backup-nas" {
		t.Fatalf("SegmentGet ArchivedToShare = %v, want \"backup-nas\"", meta.ArchivedToShare)
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

// TestStatsTracksByKind pins the fix-round-1 Tracks/TracksByKind aggregate:
// GET /api/stats' "Objects (tracked)" tile counts det_tracks rows (one per
// followed object/plate/vehicle), not raw per-frame detections — a track
// spanning many frames still counts once, unlike ByClass above.
func TestStatsTracksByKind(t *testing.T) {
	d := openTest(t)
	_, trip1, _ := seedBrowseFixture(t, d)
	ctx := context.Background()

	// Stats' det_tracks GROUP BY doesn't care which segment a track
	// belongs to, only that the segment exists to satisfy the foreign
	// key — reuse trip1's first (front) segment.
	var segID int64
	if err := d.read.QueryRowContext(ctx, `SELECT id FROM segments WHERE trip_id = ? AND camera = 'front'`, trip1).
		Scan(&segID); err != nil {
		t.Fatalf("find seeded front segment: %v", err)
	}

	must := func(query string, args ...any) {
		if _, err := d.Write().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
	}
	// Two "object" tracks (a car followed across many frames still counts
	// once), one "plate" track, no "vehicle" tracks — the per-kind map
	// should only carry the kinds actually present.
	must(`INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf)
		VALUES (?, 'object', 'car', 0, 5, 30, 0.9)`, segID)
	must(`INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf)
		VALUES (?, 'object', 'person', 1, 4, 20, 0.8)`, segID)
	must(`INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf, best_text)
		VALUES (?, 'plate', '', 2, 3, 10, 0.7, 'ABC123')`, segID)

	stats, err := d.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Tracks != 3 {
		t.Fatalf("Tracks = %d, want 3", stats.Tracks)
	}
	if stats.TracksByKind["object"] != 2 {
		t.Fatalf("TracksByKind[object] = %d, want 2", stats.TracksByKind["object"])
	}
	if stats.TracksByKind["plate"] != 1 {
		t.Fatalf("TracksByKind[plate] = %d, want 1", stats.TracksByKind["plate"])
	}
	if _, ok := stats.TracksByKind["vehicle"]; ok {
		t.Fatalf("TracksByKind = %+v, want no 'vehicle' key (none seeded)", stats.TracksByKind)
	}
}

// TestStatsTracksZeroOnNoTracks confirms a database with no det_tracks
// rows at all (pre-schema-v5, or never backfilled) reports Tracks: 0 and
// an empty (non-nil) TracksByKind rather than erroring — the frontend
// treats 0 as "not available," not "confirmed empty."
func TestStatsTracksZeroOnNoTracks(t *testing.T) {
	d := openTest(t)
	seedBrowseFixture(t, d)
	ctx := context.Background()

	stats, err := d.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Tracks != 0 {
		t.Fatalf("Tracks = %d, want 0", stats.Tracks)
	}
	if stats.TracksByKind == nil || len(stats.TracksByKind) != 0 {
		t.Fatalf("TracksByKind = %+v, want empty non-nil map", stats.TracksByKind)
	}
}

// TestStatsArchivedSSDSegmentWithShareCopyCountsOnce pins task 10's fix for
// the plan's "SSD is not a storage" double-count: an archived SSD segment
// (file gone, but a verified ingests row proves a persistent copy exists)
// must contribute to logical Bytes exactly once, via its copy, not via the
// SSD row too.
func TestStatsArchivedSSDSegmentWithShareCopyCountsOnce(t *testing.T) {
	f := newDupFixture(t)
	segID := f.segment(t, f.ssdSource, "front/clip.mp4", 1000, strp("deadbeef"))
	f.exec(t, `UPDATE segments SET archived_at = '2026-01-02T00:00:00Z' WHERE id = ?`, segID)
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at)
		VALUES (?, ?, 'front/clip.mp4', 1000, 'deadbeef', '2026-01-02T00:00:00Z')`, segID, f.shareA)

	stats, err := f.d.Stats(f.ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Bytes != 1000 {
		t.Fatalf("Bytes = %d, want 1000 (counted once via the copy)", stats.Bytes)
	}
	if stats.BytesOnSSDOnly != 0 {
		t.Fatalf("BytesOnSSDOnly = %d, want 0 (has a persistent copy)", stats.BytesOnSSDOnly)
	}
	if stats.BytesPhysical != 1000 {
		t.Fatalf("BytesPhysical = %d, want 1000 (one segments row)", stats.BytesPhysical)
	}
}

// TestStatsSSDSegmentNoCopyCountsInBothBytesAndSSDOnly: footage still only
// on the SSD (no ingest/pull) is real, uncounted-elsewhere footage, so it
// counts toward both the logical Bytes total and BytesOnSSDOnly ("not
// ingested").
func TestStatsSSDSegmentNoCopyCountsInBothBytesAndSSDOnly(t *testing.T) {
	f := newDupFixture(t)
	f.segment(t, f.ssdSource, "front/clip.mp4", 500, nil)

	stats, err := f.d.Stats(f.ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Bytes != 500 {
		t.Fatalf("Bytes = %d, want 500", stats.Bytes)
	}
	if stats.BytesOnSSDOnly != 500 {
		t.Fatalf("BytesOnSSDOnly = %d, want 500", stats.BytesOnSSDOnly)
	}
}

// TestStatsTwoLocalStoragesVerifiedDuplicateCountsOnceInBytesTwiceInPhysical:
// the same file recorded/scanned as its own segments row on two distinct
// local storages (a verified sha256 duplicate group, task 7/8) is one
// logical footage item — Bytes counts it once — but BySource/BytesPhysical
// (unchanged, physical) still shows it on both storages.
func TestStatsTwoLocalStoragesVerifiedDuplicateCountsOnceInBytesTwiceInPhysical(t *testing.T) {
	f := newDupFixture(t)
	f.segment(t, f.sourceA, "front/clip.mp4", 800, strp("deadbeef"))
	f.segment(t, f.sourceB, "front/clip.mp4", 800, strp("deadbeef"))
	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	stats, err := f.d.Stats(f.ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Bytes != 800 {
		t.Fatalf("Bytes = %d, want 800 (counted once)", stats.Bytes)
	}
	if stats.BytesPhysical != 1600 {
		t.Fatalf("BytesPhysical = %d, want 1600 (two segments rows)", stats.BytesPhysical)
	}
}

// TestStatsSegmentAndIngestSameStoragePathCountsOnce pins fix round 1's
// dedup: an SSD segment ingested to share A, where share A's source has
// also been scanned (producing its own 'segment' row for the ingested
// file), yields two file_copies rows -- 'segment' and 'ingest' -- at the
// SAME storage_key+rel_path. That's a single-storage overlap (no
// duplicate_groups row exists to correct it, since grouping requires >=2
// distinct storages), so file_copies' raw SUM would double it; Bytes must
// still count it once.
func TestStatsSegmentAndIngestSameStoragePathCountsOnce(t *testing.T) {
	f := newDupFixture(t)
	ssdSeg := f.segment(t, f.ssdSource, "front/clip.mp4", 700, strp("deadbeef"))
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at)
		VALUES (?, ?, 'front/clip.mp4', 700, 'deadbeef', '2026-01-02T00:00:00Z')`, ssdSeg, f.shareA)
	// share A's own source (kind='share') also indexed the ingested file
	// as its own segments row -- same share, same rel_path.
	f.segment(t, f.sourceA, "front/clip.mp4", 700, strp("deadbeef"))

	stats, err := f.d.Stats(f.ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Bytes != 700 {
		t.Fatalf("Bytes = %d, want 700 (counted once despite segment+ingest overlap on the same storage)", stats.Bytes)
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
