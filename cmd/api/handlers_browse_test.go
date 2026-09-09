package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// mustGzipBrowse gzips raw for use as a detections_raw.gz fixture value.
func mustGzipBrowse(t *testing.T, raw []byte) []byte {
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

// newTestMuxWithStore is like newTestMux but also returns the underlying
// store so tests can seed fixture rows directly.
func newTestMuxWithStore(t *testing.T) (*http.ServeMux, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dashcam.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	// registerJobHandlers/registerRoutes each independently call
	// config.Load(), which otherwise defaults DASHCAM_DB to
	// "/data/dashcam.db" — nothing else in cfg.DB's path ever needs to
	// match the store opened above, but the db_verify/db_prune job
	// handlers (cmd/api/jobs_maintenance.go) stat the real sqlite file by
	// that path, so it must agree with what db.Open used here.
	t.Setenv("DASHCAM_DB", dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	b := newBroker()
	queue.OnEvent(b.publishJob)
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, queue, b, ctx)
	return mux, store
}

// newTestMuxWithStoreAndQueueNoWorkers is like newTestMuxWithStore but
// doesn't start any queue workers and also returns the queue itself, for
// tests that need to seed a job and assert on it while it's still
// queued — starting workers risks the job racing to completion (or
// failure) before the test gets to look at it.
func newTestMuxWithStoreAndQueueNoWorkers(t *testing.T) (*http.ServeMux, *db.DB, *jobs.Queue) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dashcam.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Setenv("DASHCAM_DB", dbPath) // see newTestMuxWithStore's comment above

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	b := newBroker()
	queue.OnEvent(b.publishJob)
	registerJobHandlers(queue, store)

	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, queue, b, ctx)
	return mux, store, queue
}

// seedTrip inserts one source, one trip with a front segment (with
// detections + summary) and a rear segment (no detections), and returns
// (tripID, frontSegID, rearSegID).
func seedTrip(t *testing.T, store *db.DB, startedAt string) (tripID, frontSegID, rearSegID int64) {
	t.Helper()
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := store.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	tripID = must(`INSERT INTO trips (source_id, trip_key, started_at, ended_at, duration_s, front_dir, rear_dir, segment_count, bytes)
		VALUES (?, 'recording_test', ?, ?, 60, 'recording_test_f', 'recording_test_r', 2, 2000)`,
		sourceID, startedAt, startedAt)
	frontSegID = must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/x/segment_00000.mp4', 0, ?, ?, 30, 1000, 1, 0)`,
		sourceID, tripID, startedAt, startedAt)
	rearSegID = must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'rear', 'rear/x/segment_00000.mp4', 0, ?, ?, 30, 1000, 0, 0)`,
		sourceID, tripID, startedAt, startedAt)
	must(`INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates, top_classes)
		VALUES (?, 2, 0, 0, 1, 0, 0, 0, '["car"]')`, frontSegID)

	raw := []byte(`{"segment":"segment_00000.mp4","camera":"front","frame_width":640,"frame_height":640,
		"detections":[{"timestamp":0.5,"frame_index":0,"pts_offset":0.5,
			"objects":[{"class":"car","confidence":0.9,"bbox":[64,64,320,320]}]}]}`)
	must(`INSERT INTO detections_raw (segment_id, gz) VALUES (?, ?)`, frontSegID, mustGzipBrowse(t, raw))

	return tripID, frontSegID, rearSegID
}

func getJSON(t *testing.T, mux *http.ServeMux, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s response: %v, body=%s", path, err, rec.Body)
		}
	}
	return rec
}

func TestCalendarGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedTrip(t, store, "2026-03-01T10:00:00Z")

	var days []db.CalendarDay
	rec := getJSON(t, mux, "/api/calendar?from=2026-03-01&to=2026-03-01", &days)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(days) != 1 || days[0].Trips != 1 || days[0].Segments != 2 {
		t.Fatalf("days = %+v", days)
	}
}

func TestCalendarGetRejectsBadDate(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/calendar?from=not-a-date", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestTripsGetByDate(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	tripID, _, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	var body struct {
		Trips      []db.TripCard `json:"trips"`
		NextCursor string        `json:"next_cursor"`
	}
	rec := getJSON(t, mux, "/api/trips?date=2026-03-01", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.Trips) != 1 || body.Trips[0].ID != tripID {
		t.Fatalf("trips = %+v", body.Trips)
	}
	if !body.Trips[0].Front || !body.Trips[0].Rear {
		t.Fatalf("front/rear = %v/%v", body.Trips[0].Front, body.Trips[0].Rear)
	}
}

func TestTripGetEndpoint(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	tripID, _, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	var trip db.TripDetail
	rec := getJSON(t, mux, "/api/trips/"+itoa(tripID), &trip)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(trip.Segments) != 2 {
		t.Fatalf("segments = %+v", trip.Segments)
	}

	rec = getJSON(t, mux, "/api/trips/999999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing trip status = %d, want 404", rec.Code)
	}

	rec = getJSON(t, mux, "/api/trips/not-a-number", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id status = %d, want 400", rec.Code)
	}
}

func TestSegmentGetEndpoint(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	var meta db.SegmentMeta
	rec := getJSON(t, mux, "/api/segments/"+itoa(frontSegID), &meta)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if meta.Camera != "front" {
		t.Fatalf("camera = %q, want front", meta.Camera)
	}
}

func TestSegmentDetectionsGetNormalizesBBoxes(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, rearSegID := seedTrip(t, store, "2026-03-01T10:00:00Z")

	var view detectionsView
	rec := getJSON(t, mux, "/api/segments/"+itoa(frontSegID)+"/detections", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(view.Detections) != 1 || len(view.Detections[0].Objects) != 1 {
		t.Fatalf("detections = %+v", view.Detections)
	}
	bbox := view.Detections[0].Objects[0].BBox
	// raw bbox [64,64,320,320] over a 640x640 frame -> normalized [0.1,0.1,0.5,0.5]
	want := bboxView{0.1, 0.1, 0.5, 0.5}
	if bbox != want {
		t.Fatalf("bbox = %v, want %v", bbox, want)
	}

	rec = getJSON(t, mux, "/api/segments/"+itoa(rearSegID)+"/detections", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rear (no detections) status = %d, want 404", rec.Code)
	}
}

func TestStatsGetEndpoint(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedTrip(t, store, "2026-03-01T10:00:00Z")

	var stats db.Stats
	rec := getJSON(t, mux, "/api/stats", &stats)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if stats.Segments != 2 || stats.Trips != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.ByClass["car"] != 2 {
		t.Fatalf("by_class = %+v", stats.ByClass)
	}
}

func TestTripsGetRejectsInvalidCursor(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedTrip(t, store, "2026-03-01T10:00:00Z")

	for _, cursor := range []string{"not-a-number", "-1", "1.5"} {
		rec := getJSON(t, mux, "/api/trips?date=2026-03-01&cursor="+cursor, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cursor=%q status = %d, want 400, body=%s", cursor, rec.Code, rec.Body)
		}
	}
}

func TestTripGetEndpointEmptySegmentsIsEmptyArrayNotNull(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	sourceID, _ := res.LastInsertId()
	res, err = store.Write().ExecContext(ctx,
		`INSERT INTO trips (source_id, trip_key, started_at, segment_count, bytes)
		 VALUES (?, 'recording_empty', '2026-03-01T09:00:00Z', 0, 0)`, sourceID)
	if err != nil {
		t.Fatalf("seed trip: %v", err)
	}
	tripID, _ := res.LastInsertId()

	rec := getJSON(t, mux, "/api/trips/"+itoa(tripID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"segments":null`) {
		t.Fatalf("segments serialized as null, want []: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"segments":[]`) {
		t.Fatalf("segments not serialized as [], body=%s", rec.Body.String())
	}
}

func TestSegmentDetectionsGetObjectsNeverNull(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")
	ctx := context.Background()

	// Replace the seeded detections_raw with a frame that has zero objects,
	// to exercise the frame's Objects field (no `omitempty`, so it must
	// serialize as [] rather than null).
	raw := []byte(`{"segment":"segment_00000.mp4","camera":"front","frame_width":640,"frame_height":640,
		"detections":[{"timestamp":0.5,"frame_index":0,"pts_offset":0.5,"objects":[]}]}`)
	if _, err := store.Write().ExecContext(ctx,
		`UPDATE detections_raw SET gz = ? WHERE segment_id = ?`, mustGzipBrowse(t, raw), frontSegID); err != nil {
		t.Fatalf("update detections_raw: %v", err)
	}

	rec := getJSON(t, mux, "/api/segments/"+itoa(frontSegID)+"/detections", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"objects":null`) {
		t.Fatalf("frame objects serialized as null, want []: %s", body)
	}
	if !strings.Contains(body, `"objects":[]`) {
		t.Fatalf("frame objects not serialized as [], body=%s", body)
	}
}

func TestStatsGetByMonthNeverNull(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)

	// No trips/segments seeded at all: by_month must still be [] not null.
	rec := getJSON(t, mux, "/api/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"by_month":null`) {
		t.Fatalf("by_month serialized as null, want []: %s", body)
	}
	if !strings.Contains(body, `"by_month":[]`) {
		t.Fatalf("by_month not serialized as [], body=%s", body)
	}
}

// TestSegmentDetectionsGetIncludesTracks covers Task 3's `tracks` array on
// GET /api/segments/{id}/detections: unfiltered det_tracks rows, and an
// empty (not null) array when the segment has none.
func TestSegmentDetectionsGetIncludesTracks(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, rearSegID := seedTrip(t, store, "2026-03-01T10:00:00Z")

	if _, err := store.Write().ExecContext(context.Background(),
		`INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf, best_text, x1, y1, x2, y2)
		 VALUES (?, 'object', 'car', 0.5, 1.5, 3, 0.9, NULL, 0.1, 0.1, 0.5, 0.5)`, frontSegID); err != nil {
		t.Fatalf("seed det_tracks: %v", err)
	}

	var view detectionsView
	rec := getJSON(t, mux, "/api/segments/"+itoa(frontSegID)+"/detections", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(view.Tracks) != 1 {
		t.Fatalf("tracks = %+v, want 1 row", view.Tracks)
	}
	tr := view.Tracks[0]
	if tr.Kind != "object" || tr.Class != "car" || tr.NFrames != 3 {
		t.Fatalf("track = %+v, want object/car/3 frames", tr)
	}
	if tr.BestConf == nil || *tr.BestConf != 0.9 {
		t.Fatalf("track.BestConf = %v, want 0.9", tr.BestConf)
	}
	if tr.BestText != nil {
		t.Fatalf("track.BestText = %v, want nil", *tr.BestText)
	}

	// rearSegID has no detections_raw at all -> 404, never reaches the
	// tracks lookup.
	rec = getJSON(t, mux, "/api/segments/"+itoa(rearSegID)+"/detections", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rear status = %d, want 404", rec.Code)
	}
}
