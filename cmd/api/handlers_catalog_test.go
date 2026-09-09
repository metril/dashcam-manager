package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dashcam/internal/db"
)

// seedCatalogHandlerFixture inserts one segment with two Toyota sedan
// detections (make="Toyota" count 2) and a second segment with one Ford
// truck, so min_count/kind filtering has something real to check.
func seedCatalogHandlerFixture(t *testing.T, store *db.DB) {
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
	trip1 := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260301_100000', '2026-03-01T10:00:00Z', 'recording_20260301_100000', 1, 1000)`, sourceID)
	seg1 := must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/01/recording_20260301_100000/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', '2026-03-01T10:00:30Z', 30, 1000, 1, 0)`, sourceID, trip1)
	det1a := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, seg1)
	det1b := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 2.0)`, seg1)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.9)`, det1a)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.8)`, det1b)

	trip2 := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260305_090000', '2026-03-05T09:00:00Z', 'recording_20260305_090000', 1, 1000)`, sourceID)
	seg2 := must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/05/recording_20260305_090000/segment_00000.mp4', 0, '2026-03-05T09:00:00Z', '2026-03-05T09:00:30Z', 30, 1000, 1, 0)`, sourceID, trip2)
	det2 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, seg2)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'truck', 'Ford', 0.6)`, det2)

	// GET /api/catalog reads from the precomputed catalog_terms cache (see
	// internal/db/queries_catalog.go), not live det_vehicles rows.
	if err := store.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}
}

func TestHandleCatalogGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedCatalogHandlerFixture(t, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/catalog?kind=make", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Kind        string           `json:"kind"`
		Items       []db.CatalogItem `json:"items"`
		NextCursor  string           `json:"next_cursor"`
		RefreshedAt string           `json:"refreshed_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Kind != "make" {
		t.Errorf("kind = %q, want make", body.Kind)
	}
	if len(body.Items) != 2 || body.Items[0].Value != "Toyota" || body.Items[0].Count != 2 {
		t.Fatalf("items = %+v, want Toyota(2) first", body.Items)
	}
	if body.RefreshedAt == "" {
		t.Error("refreshed_at should be set after RefreshCatalog ran")
	}
}

func TestHandleCatalogGetMinCount(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedCatalogHandlerFixture(t, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/catalog?kind=make&min_count=2", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []db.CatalogItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Value != "Toyota" {
		t.Fatalf("items = %+v, want just Toyota", body.Items)
	}
}

func TestHandleCatalogGetInvalidKind(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/catalog?kind=bogus", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleCatalogGetMissingKind(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
