package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"dashcam/internal/db"
)

// registerDashcamKey generates a signing key via POST
// /api/dashcams/key/generate and registers its public half with f.fake,
// so a subsequent dashcamClientFor-backed call (e.g. GET /api/vehicle/
// status) can actually sign and be accepted.
func registerDashcamKey(t *testing.T, f *dashcamHandlerFixture) {
	t.Helper()
	w := f.do(t, "POST", "/api/dashcams/key/generate", nil)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("key/generate status = %d, body=%s", w.Code, w.Body.String())
	}
	gen := decodeJSON[dashcamKeyView](t, w)
	pub, err := base64.StdEncoding.DecodeString(gen.PublicKeyB64)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	f.fake.AddKey(gen.KeyID, pub)
}

// -- GET /api/segments/{id}/obd -----------------------------------------------

func TestHandleSegmentOBDGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	shareID, shareSrcID := mustInsertShareSourceCmd(t, store, ctx)
	_ = shareID
	segID := mustInsertVehicleSegmentCmd(t, store, ctx, shareSrcID, nil, "front", "front/x.mp4",
		"2026-01-01T10:00:00Z", "2026-01-01T10:01:00Z")
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO segment_obd (segment_id, distance_m, fuel_used_l, max_speed_kph, point_count) VALUES (?, 100, 0.1, 60, 2)`,
		segID); err != nil {
		t.Fatalf("insert segment_obd: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO segment_obd_points (segment_id, t, speed_kph) VALUES (?, '2026-01-01T10:00:30Z', 55)`, segID); err != nil {
		t.Fatalf("insert point: %v", err)
	}

	var obd db.SegmentOBD
	rec := getJSON(t, mux, "/api/segments/"+itoa(segID)+"/obd", &obd)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if obd.Summary == nil || obd.Summary.DistanceM == nil || *obd.Summary.DistanceM != 100 {
		t.Fatalf("summary = %+v, want distance_m 100", obd.Summary)
	}
	if len(obd.Points) != 1 || obd.Points[0].OffsetS < 29.9 || obd.Points[0].OffsetS > 30.1 {
		t.Fatalf("points = %+v, want 1 point at offset ~30", obd.Points)
	}
}

func TestHandleSegmentOBDGetNoOBDIs404(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	_, shareSrcID := mustInsertShareSourceCmd(t, store, ctx)
	segID := mustInsertVehicleSegmentCmd(t, store, ctx, shareSrcID, nil, "front", "front/x.mp4",
		"2026-01-01T10:00:00Z", "2026-01-01T10:01:00Z")

	rec := getJSON(t, mux, "/api/segments/"+itoa(segID)+"/obd", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body)
	}
}

func TestHandleSegmentOBDGetUnknownSegmentIs404(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/segments/999999/obd", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// -- GET /api/trips/{id}/vehicle -----------------------------------------------

func TestHandleTripVehicleGet(t *testing.T) {
	t.Setenv("TZ", "UTC")
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	shareID, shareSrcID := mustInsertShareSourceCmd(t, store, ctx)
	dashcamSrcID, err := store.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	tripID := mustInsertVehicleTripCmd(t, store, ctx, shareSrcID, "trip", "2026-01-01T10:00:00Z", "2026-01-01T10:10:00Z")
	segID := mustInsertVehicleSegmentCmd(t, store, ctx, shareSrcID, &tripID, "front", "front/x.mp4",
		"2026-01-01T10:00:00Z", "2026-01-01T10:10:00Z")
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO segment_obd (segment_id, distance_m) VALUES (?, 4200)`, segID); err != nil {
		t.Fatalf("insert segment_obd: %v", err)
	}
	if _, err := store.VehicleTripsUpsert(ctx, dashcamSrcID, db.VehicleTrip{
		RemoteTripID: "vt-1", StartTime: "2026-01-01T10:00:00", EndTime: "2026-01-01T10:10:00", MIL: 1,
	}); err != nil {
		t.Fatalf("VehicleTripsUpsert: %v", err)
	}

	var resp db.TripVehicleResponse
	rec := getJSON(t, mux, "/api/trips/"+itoa(tripID)+"/vehicle", &resp)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(resp.RecorderTrips) != 1 {
		t.Fatalf("recorder_trips = %+v, want 1", resp.RecorderTrips)
	}
	if resp.Totals.DistanceM == nil || *resp.Totals.DistanceM != 4200 {
		t.Errorf("totals.distance_m = %v, want 4200", resp.Totals.DistanceM)
	}
	if !resp.Totals.MIL {
		t.Errorf("totals.mil = false, want true")
	}
}

func TestHandleTripVehicleGetUnknownTripIs404(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/trips/999999/vehicle", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// -- GET /api/vehicle/events ----------------------------------------------------

func TestHandleVehicleEventsGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	shareID, _ := mustInsertShareSourceCmd(t, store, ctx)
	dashcamSrcID, err := store.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	if _, err := store.VehicleEventsUpsert(ctx, dashcamSrcID, db.VehicleEvent{
		RemoteEventID: "e1", EventType: "hard_brake", WallTime: "2026-01-01T10:00:00",
	}); err != nil {
		t.Fatalf("upsert e1: %v", err)
	}
	if _, err := store.VehicleEventsUpsert(ctx, dashcamSrcID, db.VehicleEvent{
		RemoteEventID: "e2", EventType: "stall", WallTime: "2026-01-02T10:00:00",
	}); err != nil {
		t.Fatalf("upsert e2: %v", err)
	}

	var body struct {
		Events     []db.VehicleEventRow `json:"events"`
		NextCursor string               `json:"next_cursor"`
	}
	rec := getJSON(t, mux, "/api/vehicle/events?type=stall", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.Events) != 1 || body.Events[0].EventType != "stall" {
		t.Fatalf("events = %+v, want just the stall event", body.Events)
	}

	// Paging: limit=1 should return one event plus a next_cursor.
	rec = getJSON(t, mux, "/api/vehicle/events?limit=1", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.Events) != 1 || body.NextCursor == "" {
		t.Fatalf("page1 = %+v, want 1 event and a next_cursor", body)
	}
	rec = getJSON(t, mux, "/api/vehicle/events?limit=1&cursor="+body.NextCursor, &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.Events) != 1 {
		t.Fatalf("page2 = %+v, want 1 event", body)
	}
}

func TestHandleVehicleEventsGetInvalidLimit(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/vehicle/events?limit=0", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// -- GET /api/vehicle/trips ----------------------------------------------------

func TestHandleVehicleTripsGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	shareID, _ := mustInsertShareSourceCmd(t, store, ctx)
	dashcamSrcID, err := store.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	if _, err := store.VehicleTripsUpsert(ctx, dashcamSrcID, db.VehicleTrip{
		RemoteTripID: "t1", StartTime: "2026-01-01T10:00:00",
	}); err != nil {
		t.Fatalf("upsert t1: %v", err)
	}
	if _, err := store.VehicleTripsUpsert(ctx, dashcamSrcID, db.VehicleTrip{
		RemoteTripID: "t2", StartTime: "2026-01-05T10:00:00",
	}); err != nil {
		t.Fatalf("upsert t2: %v", err)
	}

	var body struct {
		Trips      []db.VehicleTripRow `json:"trips"`
		NextCursor string              `json:"next_cursor"`
	}
	rec := getJSON(t, mux, "/api/vehicle/trips?from=2026-01-04&to=2026-01-06", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.Trips) != 1 {
		t.Fatalf("trips = %+v, want 1 (t2 only)", body.Trips)
	}
}

// -- GET /api/vehicle/status ----------------------------------------------------

func TestHandleVehicleStatusGetSuccess(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	f.createDashcam(t)
	registerDashcamKey(t, f)
	f.fake.VehicleStatus = map[string]any{"obd": map[string]any{"enabled": true}}

	w := f.do(t, "GET", "/api/vehicle/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["obd"]; !ok {
		t.Fatalf("body = %v, want passthrough obd key", body)
	}
	if _, ok := body["error_code"]; ok {
		t.Fatalf("body = %v, want no error_code on success", body)
	}
}

func TestHandleVehicleStatusGetUnsupportedFirmware(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	f.createDashcam(t)
	registerDashcamKey(t, f)
	f.fake.VehicleRoutesDisabled = true

	w := f.do(t, "GET", "/api/vehicle/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade), body=%s", w.Code, w.Body.String())
	}
	body := decodeJSON[map[string]any](t, w)
	if body["error_code"] != "unsupported_firmware" {
		t.Fatalf("error_code = %v, want unsupported_firmware", body["error_code"])
	}
}

func TestHandleVehicleStatusGetNoDashcamConfigured(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	w := f.do(t, "GET", "/api/vehicle/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade), body=%s", w.Code, w.Body.String())
	}
	body := decodeJSON[map[string]any](t, w)
	if body["error_code"] != "no_dashcam" {
		t.Fatalf("error_code = %v, want no_dashcam", body["error_code"])
	}
}

// -- GET /api/vehicle/dtcs / /api/vehicle/trends --------------------------

func TestHandleVehicleDTCsGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	shareID, _ := mustInsertShareSourceCmd(t, store, ctx)
	dashcamSrcID, err := store.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	if _, err := store.VehicleTripsUpsert(ctx, dashcamSrcID, db.VehicleTrip{
		RemoteTripID: "t1", StartTime: "2026-01-01T10:00:00", DTCs: []string{"P0171"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var body struct {
		DTCs []db.VehicleDTC `json:"dtcs"`
	}
	rec := getJSON(t, mux, "/api/vehicle/dtcs", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.DTCs) != 1 || body.DTCs[0].Code != "P0171" {
		t.Fatalf("dtcs = %+v, want [P0171]", body.DTCs)
	}
}

func TestHandleVehicleTrendsGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	shareID, _ := mustInsertShareSourceCmd(t, store, ctx)
	dashcamSrcID, err := store.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	if _, err := store.VehicleTripsUpsert(ctx, dashcamSrcID, db.VehicleTrip{
		RemoteTripID: "t1", StartTime: "2026-01-01T10:00:00", BattMinV: floatpCmd(11.9),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var body struct {
		Trips []db.VehicleTrend `json:"trips"`
	}
	rec := getJSON(t, mux, "/api/vehicle/trends?days=3650", &body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(body.Trips) != 1 || body.Trips[0].BattMinV == nil || *body.Trips[0].BattMinV != 11.9 {
		t.Fatalf("trips = %+v, want 1 trend with batt_min_v 11.9", body.Trips)
	}
}

func TestHandleVehicleTrendsGetInvalidDays(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/vehicle/trends?days=0", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// -- shared fixture helpers (cmd/api package doesn't import internal/db's
// test-only helpers, so these are minimal local equivalents) -------------

func mustInsertShareSourceCmd(t *testing.T, store *db.DB, ctx context.Context) (shareID, shareSrcID int64) {
	t.Helper()
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('target', 'srv', 'footage') RETURNING id`,
	).Scan(&shareID); err != nil {
		t.Fatalf("insert share: %v", err)
	}
	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, share_id, root_path, label) VALUES ('share', ?, '/mnt/nas', 'nas')`, shareID)
	if err != nil {
		t.Fatalf("insert share source: %v", err)
	}
	shareSrcID, _ = res.LastInsertId()
	return shareID, shareSrcID
}

func mustInsertVehicleTripCmd(t *testing.T, store *db.DB, ctx context.Context, shareSrcID int64, tripKey, startedAt, endedAt string) int64 {
	t.Helper()
	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO trips (source_id, trip_key, started_at, ended_at) VALUES (?, ?, ?, ?)`,
		shareSrcID, tripKey, startedAt, endedAt)
	if err != nil {
		t.Fatalf("insert trip: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func mustInsertVehicleSegmentCmd(t *testing.T, store *db.DB, ctx context.Context, shareSrcID int64, tripID *int64, camera, relPath, startTime, endTime string) int64 {
	t.Helper()
	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, trip_id, camera, rel_path, start_time, end_time) VALUES (?, ?, ?, ?, ?, ?)`,
		shareSrcID, tripID, camera, relPath, startTime, endTime)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func floatpCmd(v float64) *float64 { return &v }
