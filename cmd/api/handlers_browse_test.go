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
	"testing"

	"dashcam/internal/config"
	"dashcam/internal/db"
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
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, context.Background())
	return mux, store
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
