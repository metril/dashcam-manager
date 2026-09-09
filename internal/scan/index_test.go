package scan

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// subTripKey mirrors rebuildTripsForDate's sub-trip key derivation
// (recordingKey + "+" + sub-trip's own local start HHMMSS) so tests can
// compute the expected trip_key without duplicating that logic.
func subTripKey(recordingKey string, subStart time.Time) string {
	return recordingKey + "+" + subStart.Format("150405")
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
	wantFrontSub := subTripKey(frontKey, frontStart)
	wantRearSub := subTripKey(rearKey, rearStart)
	if tripKey.String != wantFrontSub {
		t.Errorf("trip_key = %q, want %q (front dir)", tripKey.String, wantFrontSub)
	}
	if frontDir.String != wantFrontSub || rearDir.String != wantRearSub {
		t.Errorf("front_dir/rear_dir = %q/%q, want %q/%q", frontDir.String, rearDir.String, wantFrontSub, wantRearSub)
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
	wantSub := subTripKey(rearKey, rearStart)
	if tripKey != wantSub {
		t.Errorf("trip_key = %q, want %q (rear dir, no front)", tripKey, wantSub)
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
		`SELECT segment_count FROM trips WHERE source_id = ? AND trip_key = ?`, sourceID, subTripKey(key, start),
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
		`SELECT segment_count FROM trips WHERE source_id = ? AND trip_key = ?`, sourceID, subTripKey(key, start),
	).Scan(&segCount); err != nil {
		t.Fatalf("query trip: %v", err)
	}
	if segCount != 2 {
		t.Errorf("segment_count after removal = %d, want 2", segCount)
	}
}

func TestScanRefusesToWipeIndexWhenRootLooksEmpty(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 10, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), "", "")
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	// Simulate an unmounted/empty root: point the scan at a fresh empty
	// directory while the source's existing rows (from root above) remain.
	emptyRoot := t.TempDir()
	_, err = Scan(ctx, store, sourceID, emptyRoot, Opts{})
	if err == nil {
		t.Fatal("Scan on empty root with existing index: want error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to delete index") {
		t.Errorf("Scan error = %q, want mention of refusing to delete index", err.Error())
	}

	var count int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE source_id = ?`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if count != 1 {
		t.Errorf("segments remaining = %d, want 1 (index must not have been wiped)", count)
	}
}

// TestScanCleanedSSDAllArchivedRescansCleanly covers residual-findings item
// 1: a source that's been fully ingested to a share and then cleaned off
// the SSD has zero files left on disk and every one of its rows already
// archived — that's not "root looks empty/unmounted", it's the expected
// steady state, and the empty-root guard must let it rescan (finding 0
// files, changing nothing) instead of refusing forever.
func TestScanCleanedSSDAllArchivedRescansCleanly(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 10, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	mp4 := mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), "", "")
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	var segID int64
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id FROM segments WHERE source_id = ?`, sourceID).Scan(&segID); err != nil {
		t.Fatalf("query segment id: %v", err)
	}
	var shareID int64
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('backup-nas', '10.0.0.5', 'dashcam') RETURNING id`,
	).Scan(&shareID); err != nil {
		t.Fatalf("insert share: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'front/x/segment_00000.mp4', 5, 'deadbeef', '2026-02-27T10:05:00Z')`,
		segID, shareID); err != nil {
		t.Fatalf("insert ingests row: %v", err)
	}

	// Clean the file off the SSD and archive the row (mirrors what a
	// second scan would do on its own, done directly here for a
	// deterministic single-segment fixture — see
	// TestScanArchivesSegmentWithVerifiedIngestInsteadOfDeleting for the
	// scan-driven path).
	base := mp4[:len(mp4)-len(".mp4")]
	for _, ext := range []string{".mp4", ".timestamp.json"} {
		if err := os.Remove(base + ext); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Write().ExecContext(ctx,
		`UPDATE segments SET archived_at = '2026-02-27T10:06:00Z' WHERE id = ?`, segID); err != nil {
		t.Fatalf("archive segment: %v", err)
	}

	// Now the SSD root is genuinely empty (the one segment's file is
	// gone) and the only existing row is archived — a rescan must succeed
	// cleanly rather than tripping the empty-root guard.
	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("rescan of a fully-archived, now-empty source: %v", err)
	}
	if stats.Removed != 0 {
		t.Errorf("Removed = %d, want 0 (the archived row must not be deleted)", stats.Removed)
	}

	var count int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE source_id = ?`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if count != 1 {
		t.Errorf("segments remaining = %d, want 1 (archived row must survive)", count)
	}
}

// TestScanForceBypassesEmptyRootGuard covers residual-findings item 1's
// other half: Opts.Force lets a caller proceed anyway when the guard would
// otherwise refuse (a live, non-archived row with a walk that found
// nothing) — the rare case an operator really did delete everything.
func TestScanForceBypassesEmptyRootGuard(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 10, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), "", "")
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	emptyRoot := t.TempDir()
	stats, err := Scan(ctx, store, sourceID, emptyRoot, Opts{Force: true})
	if err != nil {
		t.Fatalf("forced Scan on empty root: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("Removed = %d, want 1 (force should have deleted the live row)", stats.Removed)
	}

	var count int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE source_id = ?`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if count != 0 {
		t.Errorf("segments remaining = %d, want 0", count)
	}
}

// TestScanArchivesSegmentWithVerifiedIngestInsteadOfDeleting covers
// final-fix-wave item 7: ingests must survive segment deletion. When a
// segment's file disappears from the source but a verified ingests row
// backs it (a share already has a confirmed copy), the segment row must be
// archived (archived_at stamped) rather than deleted — deleting it would
// cascade away its ingests row and lose all provenance of where the
// footage actually is.
func TestScanArchivesSegmentWithVerifiedIngestInsteadOfDeleting(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 10, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	toArchiveMP4 := mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), "", "")
	// A second segment stays on disk throughout, so the root doesn't look
	// empty/unmounted to the item-1 guard when the first one's file is
	// removed below.
	mkSegment(t, root, "front", testDate, key, 1, start.Add(30*time.Second), start.Add(60*time.Second), "", "")
	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	var segID int64
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id FROM segments WHERE source_id = ? AND seq = 0`, sourceID).Scan(&segID); err != nil {
		t.Fatalf("query segment id: %v", err)
	}

	// Simulate a verified ingest to some share for this segment.
	var shareID int64
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('backup-nas', '10.0.0.5', 'dashcam') RETURNING id`,
	).Scan(&shareID); err != nil {
		t.Fatalf("insert share: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'front/x/segment_00000.mp4', 5, 'deadbeef', '2026-02-27T10:05:00Z')`,
		segID, shareID); err != nil {
		t.Fatalf("insert ingests row: %v", err)
	}

	base := toArchiveMP4[:len(toArchiveMP4)-len(".mp4")]
	for _, ext := range []string{".mp4", ".timestamp.json"} {
		if err := os.Remove(base + ext); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if stats.Removed != 0 {
		t.Errorf("Removed = %d, want 0 (should be archived, not deleted)", stats.Removed)
	}
	if stats.Archived != 1 {
		t.Errorf("Archived = %d, want 1", stats.Archived)
	}

	var archivedAt sql.NullString
	if err := store.Read().QueryRowContext(ctx,
		`SELECT archived_at FROM segments WHERE id = ?`, segID).Scan(&archivedAt); err != nil {
		t.Fatalf("query archived_at: %v", err)
	}
	if !archivedAt.Valid || archivedAt.String == "" {
		t.Error("expected archived_at to be set")
	}

	// The ingests row (provenance) must still exist.
	var ingestCount int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ingests WHERE segment_id = ?`, segID).Scan(&ingestCount); err != nil {
		t.Fatalf("query ingests: %v", err)
	}
	if ingestCount != 1 {
		t.Errorf("ingests rows for segment = %d, want 1 (must survive)", ingestCount)
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
	var termCount int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM search_terms WHERE segment_id = ?`, segID).Scan(&termCount); err != nil {
		t.Fatal(err)
	}
	if termCount != 3 { // 2 class terms (car, person) + 1 plate term
		t.Errorf("search_terms count = %d, want 3", termCount)
	}

	// Re-indexing must not duplicate rows, including search_terms/search_fts
	// (a prior bug left search_terms uncleared across re-index runs, since
	// it's FK'd to segments, not detections, so deleting `detections` alone
	// doesn't cascade to it).
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
	var termCount2 int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM search_terms WHERE segment_id = ?`, segID).Scan(&termCount2); err != nil {
		t.Fatal(err)
	}
	if termCount2 != 3 {
		t.Errorf("search_terms count after re-index = %d, want 3 (no duplicates)", termCount2)
	}
	var ftsCount2 int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM search_fts WHERE search_fts MATCH 'ABC123' AND kind = 'plate'`,
	).Scan(&ftsCount2); err != nil {
		t.Fatalf("fts query after re-index: %v", err)
	}
	if ftsCount2 != 1 {
		t.Errorf("fts plate match count after re-index = %d, want 1 (no duplicates)", ftsCount2)
	}
}

// TestIndexDetectionsAssignsTracks builds a two-frame sidecar with a car
// drifting slightly frame to frame (well within the IoU tracker's iouMin)
// and asserts IndexDetections creates one det_tracks row spanning both
// frames, with both det_objects rows' track_id pointing at it — and that
// re-indexing doesn't duplicate the det_tracks row either.
func TestIndexDetectionsAssignsTracks(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 12, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	dets := `{"segment":"segment_00000.mp4","camera":"front","frame_width":640,"frame_height":640,` +
		`"frames_analyzed":2,"start_time":"2026-02-27T00:16:54.000","end_time":"2026-02-27T00:17:24.000",` +
		`"pts_start":0,"pts_end":30,"total_objects":2,"total_plates":0,"total_vehicles_classified":0,"detections":[` +
		`{"timestamp":0.0,"wall_time":"2026-02-27T00:16:54.000","frame_index":0,"pts_offset":0.0,"objects":[{"class":"car","confidence":0.9,"bbox":[10,10,110,110]}]},` +
		`{"timestamp":0.1,"wall_time":"2026-02-27T00:16:54.100","frame_index":1,"pts_offset":0.1,"objects":[{"class":"car","confidence":0.95,"bbox":[15,10,115,110]}]}` +
		`]}`
	mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), dets, "")

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var segID int64
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id FROM segments WHERE source_id = ? AND seq = 0`, sourceID,
	).Scan(&segID); err != nil {
		t.Fatalf("query segment: %v", err)
	}

	if err := IndexDetections(ctx, store, segID); err != nil {
		t.Fatalf("IndexDetections: %v", err)
	}

	var trackCount int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM det_tracks WHERE segment_id = ?`, segID).Scan(&trackCount); err != nil {
		t.Fatal(err)
	}
	if trackCount != 1 {
		t.Fatalf("det_tracks count = %d, want 1", trackCount)
	}
	var trackID int64
	var kind, class string
	var nFrames int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id, kind, class, n_frames FROM det_tracks WHERE segment_id = ?`, segID,
	).Scan(&trackID, &kind, &class, &nFrames); err != nil {
		t.Fatal(err)
	}
	if kind != "object" || class != "car" || nFrames != 2 {
		t.Errorf("track = kind=%q class=%q n_frames=%d, want object/car/2", kind, class, nFrames)
	}

	var objTrackCount int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM det_objects o JOIN detections d ON d.id = o.det_id WHERE d.segment_id = ? AND o.track_id = ?`,
		segID, trackID,
	).Scan(&objTrackCount); err != nil {
		t.Fatal(err)
	}
	if objTrackCount != 2 {
		t.Errorf("det_objects with track_id = %d, want 2", objTrackCount)
	}

	// Re-indexing must not duplicate det_tracks.
	if err := IndexDetections(ctx, store, segID); err != nil {
		t.Fatalf("IndexDetections (re-run): %v", err)
	}
	var trackCount2 int
	if err := store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM det_tracks WHERE segment_id = ?`, segID).Scan(&trackCount2); err != nil {
		t.Fatal(err)
	}
	if trackCount2 != 1 {
		t.Errorf("det_tracks count after re-index = %d, want 1 (no duplicates)", trackCount2)
	}
}

// TestScanTripGapSplit builds one front recording directory whose segments
// have a 20-minute gap partway through and asserts it's split into two
// separate trips, per TripGapSplit.
func TestScanTripGapSplit(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 14, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	// First 2 segments back-to-back, then a 20-minute gap, then 2 more.
	firstStarts := []time.Time{start, start.Add(30 * time.Second)}
	secondStart := start.Add(20 * time.Minute)
	secondStarts := []time.Time{secondStart, secondStart.Add(30 * time.Second)}
	for i, s := range firstStarts {
		mkSegment(t, root, "front", testDate, key, i, s, s.Add(30*time.Second), "", "")
	}
	for i, s := range secondStarts {
		mkSegment(t, root, "front", testDate, key, i+2, s, s.Add(30*time.Second), "", "")
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.TripsUpserted != 2 {
		t.Fatalf("TripsUpserted = %d, want 2 (20-minute gap should split into 2 trips)", stats.TripsUpserted)
	}

	rows, err := store.Read().QueryContext(ctx,
		`SELECT trip_key, segment_count FROM trips WHERE source_id = ? ORDER BY started_at`, sourceID)
	if err != nil {
		t.Fatalf("query trips: %v", err)
	}
	defer rows.Close()
	var gotKeys []string
	var gotCounts []int
	for rows.Next() {
		var k string
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			t.Fatal(err)
		}
		gotKeys = append(gotKeys, k)
		gotCounts = append(gotCounts, c)
	}
	if len(gotKeys) != 2 {
		t.Fatalf("got %d trips, want 2: %v", len(gotKeys), gotKeys)
	}
	wantFirst := subTripKey(key, firstStarts[0])
	wantSecond := subTripKey(key, secondStart)
	if gotKeys[0] != wantFirst || gotKeys[1] != wantSecond {
		t.Errorf("trip_keys = %v, want [%q %q]", gotKeys, wantFirst, wantSecond)
	}
	if gotCounts[0] != 2 || gotCounts[1] != 2 {
		t.Errorf("segment_counts = %v, want [2 2]", gotCounts)
	}
}

// TestScanSidecarMtimeTriggersReindex covers finding #2: the unchanged-skip
// check must notice a sidecar added/changed after the mp4, not just the
// mp4's own mtime.
func TestScanSidecarMtimeTriggersReindex(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 15, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	mp4 := mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), "", "")

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	var hasDetBefore int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT has_detections FROM segments WHERE source_id = ? AND seq = 0`, sourceID).Scan(&hasDetBefore); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if hasDetBefore != 0 {
		t.Fatalf("has_detections before sidecar added = %d, want 0", hasDetBefore)
	}

	// The recorder writes .detections.json some time after the .mp4 itself
	// (once analysis finishes) — simulate that by adding it well after the
	// first scan, with a newer mtime than anything seen so far, but the mp4
	// itself is untouched.
	base := mp4[:len(mp4)-len(".mp4")]
	dets := detsFixture([][2]string{{"car", ""}}, nil)
	if err := os.WriteFile(base+".detections.json", []byte(dets), 0o644); err != nil {
		t.Fatal(err)
	}
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(base+".detections.json", newTime, newTime); err != nil {
		t.Fatal(err)
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if stats.Indexed != 1 || stats.Skipped != 0 {
		t.Errorf("second scan stats = %+v, want Indexed=1 Skipped=0 (new sidecar should force reprocessing)", stats)
	}
	var hasDetAfter int
	if err := store.Read().QueryRowContext(ctx,
		`SELECT has_detections FROM segments WHERE source_id = ? AND seq = 0`, sourceID).Scan(&hasDetAfter); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if hasDetAfter != 1 {
		t.Errorf("has_detections after sidecar added = %d, want 1", hasDetAfter)
	}
}

// TestScanDetectionsSidecarRemoved covers finding #4: if a segment's
// .detections.json disappears (or becomes unparsable) between scans, the
// stale segment_summary/detections_raw rows from when it existed must be
// cleared, not left describing detections that no longer exist.
func TestScanDetectionsSidecarRemoved(t *testing.T) {
	root := t.TempDir()
	store := openTestDB(t)
	ctx := context.Background()
	sourceID, err := store.SourceCreate(ctx, "ssd", root, "test")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	start := time.Date(2026, 2, 27, 16, 0, 0, 0, time.Local)
	key := recKey(testDate, start)
	dets := detsFixture([][2]string{{"car", ""}}, []string{"XYZ999"})
	mp4 := mkSegment(t, root, "front", testDate, key, 0, start, start.Add(30*time.Second), dets, "")
	base := mp4[:len(mp4)-len(".mp4")]
	// Give .detections.json a strictly later mtime than the mp4/timestamp
	// sidecar (realistic: analysis finishes well after the recorder writes
	// the segment), so that removing it below actually lowers the combined
	// mtime Scan compares against — mkSegment writes all fixture files back
	// to back, which can otherwise land in the same mtime second.
	detTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(base+".detections.json", detTime, detTime); err != nil {
		t.Fatal(err)
	}

	if _, err := Scan(ctx, store, sourceID, root, Opts{}); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	var segID int64
	if err := store.Read().QueryRowContext(ctx,
		`SELECT id FROM segments WHERE source_id = ? AND seq = 0`, sourceID).Scan(&segID); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	var summaryCount, rawCount int
	store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segment_summary WHERE segment_id = ?`, segID).Scan(&summaryCount)
	store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM detections_raw WHERE segment_id = ?`, segID).Scan(&rawCount)
	if summaryCount != 1 || rawCount != 1 {
		t.Fatalf("before removal: summaryCount=%d rawCount=%d, want 1/1", summaryCount, rawCount)
	}

	if err := os.Remove(base + ".detections.json"); err != nil {
		t.Fatal(err)
	}

	stats, err := Scan(ctx, store, sourceID, root, Opts{})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if stats.Indexed != 1 {
		t.Fatalf("second scan Indexed = %d, want 1 (missing sidecar should force reprocessing)", stats.Indexed)
	}

	var hasDet int
	store.Read().QueryRowContext(ctx, `SELECT has_detections FROM segments WHERE id = ?`, segID).Scan(&hasDet)
	if hasDet != 0 {
		t.Errorf("has_detections after sidecar removed = %d, want 0", hasDet)
	}
	store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segment_summary WHERE segment_id = ?`, segID).Scan(&summaryCount)
	store.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM detections_raw WHERE segment_id = ?`, segID).Scan(&rawCount)
	if summaryCount != 0 || rawCount != 0 {
		t.Errorf("after removal: summaryCount=%d rawCount=%d, want 0/0 (stale rows must be cleared)", summaryCount, rawCount)
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
