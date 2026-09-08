package scan

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dashcam/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "scan-test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return d
}

// mkSegment writes a fake segment_NNNNN.mp4 plus its .timestamp.json (and
// optionally .detections.json / .gps.json) under root, matching the
// recorder's real layout so ParseSegmentPath/walkRoot accept it.
func mkSegment(t *testing.T, root, camera string, date time.Time, recKey string, seq int, start, end time.Time, detsJSON, gpsJSON string) string {
	t.Helper()
	dir := filepath.Join(root, camera, date.Format("2006/01/02"), recKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, fmt.Sprintf("segment_%05d", seq))
	if err := os.WriteFile(base+".mp4", []byte("dummy-mp4-bytes-not-real-video"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := fmt.Sprintf(`{"segment":"segment_%05d.mp4","camera":%q,"start_time":%q,"end_time":%q}`,
		seq, camera, start.Format("2006-01-02T15:04:05.000"), end.Format("2006-01-02T15:04:05.000"))
	if err := os.WriteFile(base+".timestamp.json", []byte(ts), 0o644); err != nil {
		t.Fatal(err)
	}
	if detsJSON != "" {
		if err := os.WriteFile(base+".detections.json", []byte(detsJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if gpsJSON != "" {
		if err := os.WriteFile(base+".gps.json", []byte(gpsJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base + ".mp4"
}

func recKey(date time.Time, start time.Time) string {
	return "recording_" + date.Format("20060102") + "_" + start.Format("150405")
}

var testDate = time.Date(2026, 2, 27, 0, 0, 0, 0, time.Local)

func detsFixture(objects [][2]string, plates []string) string {
	// objects: [][class, confidence-ignored]; keep it simple with fixed conf.
	var objs []string
	for _, o := range objects {
		objs = append(objs, fmt.Sprintf(`{"class":%q,"confidence":0.9,"bbox":[10,10,100,100]}`, o[0]))
	}
	var pls []string
	for _, p := range plates {
		pls = append(pls, fmt.Sprintf(`{"text":%q,"confidence":0.8,"bbox":[5,5,50,20],"vehicle_bbox":[0,0,200,200]}`, p))
	}
	frame := fmt.Sprintf(`{"timestamp":0.5,"wall_time":"2026-02-27T00:16:55.000","frame_index":1,"pts_offset":0.5,"objects":[%s],"plates":[%s]}`,
		joinStrs(objs), joinStrs(pls))
	return fmt.Sprintf(`{"segment":"segment_00000.mp4","camera":"front","frame_width":640,"frame_height":640,"frames_analyzed":1,"start_time":"2026-02-27T00:16:54.000","end_time":"2026-02-27T00:17:24.000","pts_start":0,"pts_end":30,"total_objects":%d,"total_plates":%d,"total_vehicles_classified":0,"detections":[%s]}`,
		len(objects), len(plates), frame)
}

func joinStrs(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func gpsFixture(startLat, startLon float64) string {
	var pts []string
	base := time.Date(2026, 2, 27, 0, 16, 54, 0, time.Local)
	for i := 0; i < 5; i++ {
		t := base.Add(time.Duration(i) * time.Second)
		pts = append(pts, fmt.Sprintf(`{"t":%q,"lat":%f,"lon":%f,"speed_mps":10.5,"heading":90,"alt_m":10,"sats":8,"fix":3}`,
			t.Format("2006-01-02T15:04:05.000"), startLat+float64(i)*0.0001, startLon))
	}
	return fmt.Sprintf(`{"segment":"segment_00000.mp4","camera":"front","start_time":"2026-02-27T00:16:54.000","end_time":"2026-02-27T00:16:59.000","start_lat":%f,"start_lon":%f,"end_lat":%f,"end_lon":%f,"distance_m":50,"max_speed_mps":11,"points":[%s]}`,
		startLat, startLon, startLat+0.0004, startLon, joinStrs(pts))
}

func TestScanFrontRearPairing(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	frontStart := time.Date(2026, 2, 27, 0, 16, 54, 0, time.Local)
	rearStart := frontStart.Add(2 * time.Second) // 2s offset, per the brief
	frontKey := recKey(testDate, frontStart)
	rearKey := recKey(testDate, rearStart)

	for i := 0; i < 3; i++ {
		s := frontStart.Add(time.Duration(i) * 30 * time.Second)
		e := s.Add(30 * time.Second)
		gps := ""
		if i == 0 {
			gps = gpsFixture(40.9, -73.7)
		}
		mkSegment(t, root, "front", testDate, frontKey, i, s, e, "", gps)
	}
	for i := 0; i < 3; i++ {
		s := rearStart.Add(time.Duration(i) * 30 * time.Second)
		e := s.Add(30 * time.Second)
		mkSegment(t, root, "rear", testDate, rearKey, i, s, e, "", "")
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Found != 6 || stats.Indexed != 6 {
		t.Fatalf("stats = %+v, want Found=6 Indexed=6", stats)
	}
	if stats.TripsUpserted != 1 {
		t.Fatalf("TripsUpserted = %d, want 1", stats.TripsUpserted)
	}

	var tripKey, frontDir, rearDir sql.NullString
	var pairOffset sql.NullFloat64
	var segCount int
	var distanceM sql.NullFloat64
	err = store.Read().QueryRowContext(ctx,
		`SELECT trip_key, front_dir, rear_dir, pair_offset_s, segment_count, distance_m FROM trips WHERE source_id = ?`, sourceID,
	).Scan(&tripKey, &frontDir, &rearDir, &pairOffset, &segCount, &distanceM)
	if err != nil {
		t.Fatalf("query trip: %v", err)
	}
	if tripKey.String != frontKey {
		t.Errorf("trip_key = %q, want %q (front dir)", tripKey.String, frontKey)
	}
	if frontDir.String != frontKey || rearDir.String != rearKey {
		t.Errorf("front_dir/rear_dir = %q/%q, want %q/%q", frontDir.String, rearDir.String, frontKey, rearKey)
	}
	if !pairOffset.Valid || pairOffset.Float64 < 1.9 || pairOffset.Float64 > 2.1 {
		t.Errorf("pair_offset_s = %v, want ~2.0", pairOffset)
	}
	if segCount != 6 {
		t.Errorf("segment_count = %d, want 6", segCount)
	}
	if !distanceM.Valid || distanceM.Float64 <= 0 {
		t.Errorf("distance_m = %v, want > 0 (front had gps)", distanceM)
	}

	var linkedTrip sql.NullInt64
	if err := store.Read().QueryRowContext(ctx,
		`SELECT trip_id FROM segments WHERE source_id = ? AND camera = 'front' AND seq = 0`, sourceID,
	).Scan(&linkedTrip); err != nil {
		t.Fatalf("query segment trip_id: %v", err)
	}
	if !linkedTrip.Valid {
		t.Error("front segment 0 has no trip_id set")
	}
}

func TestScanRearOnly(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	rearStart := time.Date(2026, 2, 27, 5, 0, 0, 0, time.Local)
	rearKey := recKey(testDate, rearStart)
	mkSegment(t, root, "rear", testDate, rearKey, 0, rearStart, rearStart.Add(30*time.Second), "", "")

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Found != 1 || stats.TripsUpserted != 1 {
		t.Fatalf("stats = %+v, want Found=1 TripsUpserted=1", stats)
	}

	var tripKey string
	var frontDir sql.NullString
	if err := store.Read().QueryRowContext(ctx,
		`SELECT trip_key, front_dir FROM trips WHERE source_id = ?`, sourceID,
	).Scan(&tripKey, &frontDir); err != nil {
		t.Fatalf("query trip: %v", err)
	}
	if tripKey != rearKey {
		t.Errorf("trip_key = %q, want %q (rear dir, no front)", tripKey, rearKey)
	}
	if frontDir.Valid {
		t.Errorf("front_dir = %q, want NULL", frontDir.String)
	}
}

func TestScanDroppedSegment(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 8, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	// seq 0, 1, 3 present; seq 2 dropped.
	for _, seq := range []int{0, 1, 3} {
		s := start.Add(time.Duration(seq) * 30 * time.Second)
		mkSegment(t, root, "front", testDate, key, seq, s, s.Add(30*time.Second), "", "")
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Found != 3 {
		t.Fatalf("Found = %d, want 3", stats.Found)
	}
	var segCount int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT segment_count FROM trips WHERE source_id = ? AND trip_key = ?`, sourceID, key,
	).Scan(&segCount); err != nil {
		t.Fatalf("query trip: %v", err)
	}
	if segCount != 3 {
		t.Errorf("segment_count = %d, want 3 (dropped seq 2 tolerated)", segCount)
	}
}

func TestScanRescanNoOp(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 9, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	for i := 0; i < 3; i++ {
		s := start.Add(time.Duration(i) * 30 * time.Second)
		mkSegment(t, root, "front", testDate, key, i, s, s.Add(30*time.Second), "", "")
	}

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if stats.Indexed != 0 || stats.Skipped != 3 {
		t.Errorf("rescan stats = %+v, want Indexed=0 Skipped=3", stats)
	}
}

func TestScanDeletedFileRemoval(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 10, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	var lastMP4 string
	for i := 0; i < 3; i++ {
		s := start.Add(time.Duration(i) * 30 * time.Second)
		lastMP4 = mkSegment(t, root, "front", testDate, key, i, s, s.Add(30*time.Second), "", "")
	}
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	base := lastMP4[:len(lastMP4)-len(".mp4")]
	for _, ext := range []string{".mp4", ".timestamp.json"} {
		if err := os.Remove(base + ext); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if stats.Removed != 1 {
		t.Fatalf("Removed = %d, want 1", stats.Removed)
	}

	var count int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE source_id = ?`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if count != 2 {
		t.Errorf("segments remaining = %d, want 2", count)
	}
	var segCount int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT segment_count FROM trips WHERE source_id = ? AND trip_key = ?`, sourceID, key,
	).Scan(&segCount); err != nil {
		t.Fatalf("query trip: %v", err)
	}
	if segCount != 2 {
		t.Errorf("segment_count after removal = %d, want 2", segCount)
	}
}

func TestIndexDetections(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 12, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	dets := detsFixture([][2]string{{"car", ""}, {"person", ""}}, []string{"ABC123"})
	mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), dets, "")

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var segID int64
	var hasDet int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id, has_detections FROM segments WHERE source_id = ? AND seq = 0`, sourceID,
	).Scan(&segID, &hasDet); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if hasDet != 1 {
		t.Fatalf("has_detections = %d, want 1", hasDet)
	}

	// Before IndexDetections, no per-frame rows exist (deferred to M7).
	var preCount int
	store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM det_objects`).Scan(&preCount)
	if preCount != 0 {
		t.Fatalf("det_objects before IndexDetections = %d, want 0", preCount)
	}

	if err := IndexDetections(ctx, store, segID); err != nil {
		t.Fatalf("IndexDetections: %v", err)
	}

	var objCount, plateCount int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM det_objects`).Scan(&objCount); err != nil {
		t.Fatal(err)
	}
	if objCount != 2 {
		t.Errorf("det_objects count = %d, want 2", objCount)
	}
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM det_plates`).Scan(&plateCount); err != nil {
		t.Fatal(err)
	}
	if plateCount != 1 {
		t.Errorf("det_plates count = %d, want 1", plateCount)
	}

	var ftsCount int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'ABC123' AND kind = 'plate'`,
	).Scan(&ftsCount); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if ftsCount != 1 {
		t.Errorf("fts plate match count = %d, want 1", ftsCount)
	}

	// Re-indexing must not duplicate rows.
	if err := IndexDetections(ctx, store, segID); err != nil {
		t.Fatalf("IndexDetections (re-run): %v", err)
	}
	var objCount2 int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM det_objects`).Scan(&objCount2); err != nil {
		t.Fatal(err)
	}
	if objCount2 != 2 {
		t.Errorf("det_objects count after re-index = %d, want 2 (no duplicates)", objCount2)
	}
}

func TestPairRecordingsNoOverlap(t *testing.T) {
	f := Recording{Camera: "front", Key: "recording_20260227_000000",
		Start: time.Date(2026, 2, 27, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 2, 27, 0, 1, 0, 0, time.UTC), SegmentIDs: []int64{1}}
	r := Recording{Camera: "rear", Key: "recording_20260227_010000",
		Start: time.Date(2026, 2, 27, 1, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 2, 27, 1, 1, 0, 0, time.UTC), SegmentIDs: []int64{2}}
	pairs := PairRecordings([]Recording{f}, []Recording{r})
	if len(pairs) != 2 {
		t.Fatalf("len(pairs) = %d, want 2 (no overlap -> two singletons)", len(pairs))
	}
	for _, p := range pairs {
		if p.FrontDir != "" && p.RearDir != "" {
			t.Errorf("expected no full pair for non-overlapping recordings, got %+v", p)
		}
	}
}

func TestSourcesCRUD(t *testing.T) {
	store := openTestDB(t)
	ctx := context.Background()

	id, err := store.SourceEnsure(ctx, "ssd", "/mnt/dashcam", "SSD")
	if err != nil {
		t.Fatalf("SourceEnsure: %v", err)
	}
	id2, err := store.SourceEnsure(ctx, "ssd", "/mnt/dashcam", "SSD (renamed elsewhere)")
	if err != nil {
		t.Fatalf("SourceEnsure (again): %v", err)
	}
	if id != id2 {
		t.Errorf("SourceEnsure returned different ids for same root_path: %d != %d", id, id2)
	}

	sources, err := store.SourcesList(ctx)
	if err != nil {
		t.Fatalf("SourcesList: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("len(sources) = %d, want 1", len(sources))
	}

	if err := store.SourceSetLastScanAt(ctx, id, "2026-09-08T00:00:00Z", "idle"); err != nil {
		t.Fatalf("SourceSetLastScanAt: %v", err)
	}
	s, err := store.SourceGet(ctx, id)
	if err != nil {
		t.Fatalf("SourceGet: %v", err)
	}
	if s.Status != "idle" || s.LastScanAt == nil || *s.LastScanAt != "2026-09-08T00:00:00Z" {
		t.Errorf("source after SetLastScanAt = %+v", s)
	}
}

func TestEnrichFromLegacyDBNoFile(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}
	n, err := EnrichFromLegacyDB(ctx, store, sourceID, root)
	if err != nil {
		t.Fatalf("EnrichFromLegacyDB: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 (no legacy db present)", n)
	}
}
