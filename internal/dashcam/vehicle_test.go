package dashcam_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/dashcam/dashcamtest"
)

// signAndGet signs a GET to pathQS the same way dashcam.Client does (using
// c's own key material) and returns the decoded {error,server_time} body's
// status/code — used to exercise 400 paths the typed client API can never
// construct on its own (VehicleTrips always sends a valid int64).
func signAndGet(t *testing.T, c *dashcam.Client, pathQS string) (status int, code string) {
	t.Helper()
	nonceB := make([]byte, 16)
	if _, err := rand.Read(nonceB); err != nil {
		t.Fatal(err)
	}
	nonce := base64.StdEncoding.EncodeToString(nonceB)
	ts := time.Now().Unix()
	sum := sha256.Sum256(nil)
	msg := "GET\n" + pathQS + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
	sig := ed25519.Sign(c.Priv, []byte(msg))

	req, err := http.NewRequest("GET", c.BaseURL+pathQS, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Ingest-Key-Id", c.KeyID)
	req.Header.Set("X-Ingest-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Ingest-Nonce", nonce)
	req.Header.Set("X-Ingest-Signature", base64.StdEncoding.EncodeToString(sig))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error
}

func floatp(v float64) *float64 { return &v }
func intp(v int) *int           { return &v }
func int64p(v int64) *int64     { return &v }
func strp(v string) *string     { return &v }

// -- VehicleTrips / EachVehicleTrip ----------------------------------------

func TestVehicleTrips_PagesAcrossMultiplePages(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	for i := int64(1); i <= 5; i++ {
		fake.VehicleTrips = append(fake.VehicleTrips, dashcamtest.VehicleTrip{
			ID: i, StartTime: "2026-09-11T00:00:00", EndTime: "2026-09-11T00:10:00",
			SampleCount: int(i), DTCs: []string{},
		})
	}

	page, err := c.VehicleTrips(ctx, 0, 2)
	if err != nil {
		t.Fatalf("VehicleTrips: %v", err)
	}
	if len(page.Trips) != 2 || page.Trips[0].ID != 1 || page.Trips[1].ID != 2 {
		t.Fatalf("page1 trips = %+v", page.Trips)
	}
	if page.NextSinceID == nil || *page.NextSinceID != 2 {
		t.Fatalf("page1 NextSinceID = %v, want 2", page.NextSinceID)
	}

	var got []int64
	cursor, err := c.EachVehicleTrip(ctx, 0, func(tr dashcam.VehicleTrip) error {
		got = append(got, tr.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("EachVehicleTrip: %v", err)
	}
	want := []int64{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("EachVehicleTrip visited %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EachVehicleTrip visited %v, want %v", got, want)
		}
	}
	if cursor != 5 {
		t.Fatalf("EachVehicleTrip cursor = %d, want 5", cursor)
	}
}

func TestVehicleTrips_SinceIDExclusive(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleTrips = []dashcamtest.VehicleTrip{
		{ID: 1, DTCs: []string{}}, {ID: 2, DTCs: []string{}}, {ID: 3, DTCs: []string{}},
	}
	page, err := c.VehicleTrips(ctx, 1, 0)
	if err != nil {
		t.Fatalf("VehicleTrips: %v", err)
	}
	if len(page.Trips) != 2 || page.Trips[0].ID != 2 || page.Trips[1].ID != 3 {
		t.Fatalf("trips = %+v, want IDs [2 3]", page.Trips)
	}
	if page.NextSinceID != nil {
		t.Fatalf("NextSinceID = %v, want nil (short page)", page.NextSinceID)
	}
}

func TestVehicleTrips_NullableFieldsAndDTCsDecode(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleTrips = []dashcamtest.VehicleTrip{{
		ID: 1, StartTime: "2026-09-11T00:00:00", EndTime: "2026-09-11T00:10:00",
		DistanceM: floatp(1234.5), FuelL: nil, MaxSpeedKph: floatp(88.2),
		SampleCount: 42, DTCs: []string{"P0171", "P0300"},
		MIL: intp(1), HardBrakes: intp(2), HardAccels: nil,
		Speeding: 1, Idling: 0, Overheat: 0,
		ChargingFault: 0, ColdHard: 0, Cornering: 1, Stall: 0,
	}}

	page, err := c.VehicleTrips(ctx, 0, 0)
	if err != nil {
		t.Fatalf("VehicleTrips: %v", err)
	}
	if len(page.Trips) != 1 {
		t.Fatalf("got %d trips, want 1", len(page.Trips))
	}
	tr := page.Trips[0]
	if tr.DistanceM == nil || *tr.DistanceM != 1234.5 {
		t.Fatalf("DistanceM = %v, want 1234.5", tr.DistanceM)
	}
	if tr.FuelL != nil {
		t.Fatalf("FuelL = %v, want nil", tr.FuelL)
	}
	if tr.HardAccels != nil {
		t.Fatalf("HardAccels = %v, want nil", tr.HardAccels)
	}
	if tr.HardBrakes == nil || *tr.HardBrakes != 2 {
		t.Fatalf("HardBrakes = %v, want 2", tr.HardBrakes)
	}
	if len(tr.DTCs) != 2 || tr.DTCs[0] != "P0171" || tr.DTCs[1] != "P0300" {
		t.Fatalf("DTCs = %v", tr.DTCs)
	}
	if tr.MIL == nil || *tr.MIL != 1 {
		t.Fatalf("MIL = %v, want 1", tr.MIL)
	}
	if tr.Speeding != 1 || tr.Cornering != 1 || tr.Idling != 0 || tr.Stall != 0 {
		t.Fatalf("flags decoded wrong: %+v", tr)
	}
}

// TestVehicleTrips_DecodesRecorderShapeDirectly feeds a raw JSON body
// shaped exactly like the recorder's actual response (see review round 1:
// mil/speeding/etc. are sqlite INTEGER columns json-encoded as 0/1, never
// JSON bools) rather than round-tripping through the fake's own Go
// structs, so a regression back to `"mil": bool` in either vehicle.go or
// dashcamtest/vehicle.go's toJSON would be caught even if both sides
// changed together.
func TestVehicleTrips_DecodesRecorderShapeDirectly(t *testing.T) {
	raw := []byte(`{
		"trips": [{
			"id": 1, "start_time": "2026-09-11T00:00:00", "end_time": "2026-09-11T00:10:00",
			"start_t": 100.0, "end_t": 700.0, "distance_m": 5000.0, "fuel_l": null,
			"fuel_pct_start": 80.0, "fuel_pct_end": 78.5, "max_speed_kph": 95.0,
			"avg_speed_kph": 40.0, "batt_min_v": 12.1, "batt_max_v": 14.2,
			"coolant_max_c": 92.0, "sample_count": 120, "dtcs": ["C0300"],
			"mil": 0, "hard_brakes": 1, "hard_accels": 0,
			"speeding": 1, "idling": 0, "overheat": 0, "charging_fault": 0,
			"cold_hard": 0, "cornering": 0, "stall": 0
		}],
		"count": 1, "next_since_id": null
	}`)
	var page dashcam.VehicleTripsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode recorder-shaped trips page: %v", err)
	}
	if len(page.Trips) != 1 {
		t.Fatalf("got %d trips, want 1", len(page.Trips))
	}
	tr := page.Trips[0]
	if tr.MIL == nil || *tr.MIL != 0 {
		t.Fatalf("MIL = %v, want 0", tr.MIL)
	}
	if tr.Speeding != 1 {
		t.Fatalf("Speeding = %d, want 1", tr.Speeding)
	}
	if len(tr.DTCs) != 1 || tr.DTCs[0] != "C0300" {
		t.Fatalf("DTCs = %v, want [C0300]", tr.DTCs)
	}
	if page.NextSinceID != nil {
		t.Fatalf("NextSinceID = %v, want nil", page.NextSinceID)
	}
}

func TestVehicleTrips_EmptyStops(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	var calls int
	cursor, err := c.EachVehicleTrip(ctx, 0, func(dashcam.VehicleTrip) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("EachVehicleTrip: %v", err)
	}
	if calls != 0 || cursor != 0 {
		t.Fatalf("calls=%d cursor=%d, want 0/0", calls, cursor)
	}
}

// -- VehicleEvents / EachVehicleEvent --------------------------------------

func TestVehicleEvents_PagesAndDecodesNullableFields(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleEvents = []dashcamtest.VehicleEvent{
		{ID: 10, EventType: "hard_brake", WallTime: "2026-09-11T00:01:00",
			Peak: floatp(0.9), SpeedKph: floatp(60), RoadName: strp("Main St"),
			TripID: int64p(5)},
		{ID: 11, EventType: "overheat", WallTime: "2026-09-11T00:05:00",
			Peak: nil, SpeedKph: nil, RoadName: nil, TripID: nil},
		{ID: 12, EventType: "speeding", WallTime: "2026-09-11T00:07:00"},
	}

	var got []dashcam.VehicleEvent
	cursor, err := c.EachVehicleEvent(ctx, 0, func(ev dashcam.VehicleEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("EachVehicleEvent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Peak == nil || *got[0].Peak != 0.9 {
		t.Fatalf("event0 Peak = %v, want 0.9", got[0].Peak)
	}
	if got[0].RoadName == nil || *got[0].RoadName != "Main St" {
		t.Fatalf("event0 RoadName = %v", got[0].RoadName)
	}
	if got[0].TripID == nil || *got[0].TripID != 5 {
		t.Fatalf("event0 TripID = %v, want 5", got[0].TripID)
	}
	if got[1].Peak != nil || got[1].RoadName != nil || got[1].TripID != nil {
		t.Fatalf("event1 nullable fields not nil: %+v", got[1])
	}
	if cursor != 12 {
		t.Fatalf("cursor = %d, want 12", cursor)
	}

	// Page directly too, with a small limit to force pagination.
	page, err := c.VehicleEvents(ctx, 0, 2)
	if err != nil {
		t.Fatalf("VehicleEvents: %v", err)
	}
	if len(page.Events) != 2 || page.NextSinceID == nil || *page.NextSinceID != 11 {
		t.Fatalf("page = %+v", page)
	}
}

// -- VehicleStatus ----------------------------------------------------------

func TestVehicleStatus_PassthroughJSON(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleStatus = map[string]any{
		"obd":       map[string]any{"enabled": true, "port": "/dev/ttyUSB0"},
		"trip":      map[string]any{"id": 7, "start_time": "2026-09-11T00:00:00"},
		"last_trip": nil,
	}

	raw, err := c.VehicleStatus(ctx)
	if err != nil {
		t.Fatalf("VehicleStatus: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	obd, ok := decoded["obd"].(map[string]any)
	if !ok || obd["enabled"] != true {
		t.Fatalf("obd = %+v", decoded["obd"])
	}
	if decoded["last_trip"] != nil {
		t.Fatalf("last_trip = %v, want nil", decoded["last_trip"])
	}
}

// -- errors -----------------------------------------------------------------

// TestVehicleTrips_SignedInvalidParamsAre400APIError signs raw requests
// itself (the typed client can never construct a malformed since_id/limit)
// to exercise the fake's 400 paths, and confirms an *dashcam.APIError with
// the right code comes back through c.do's normal error mapping — reached
// here via a minimal client pointed at the same signed headers.
func TestVehicleTrips_SignedInvalidParamsAre400APIError(t *testing.T) {
	c, _ := newTestClient(t)

	for _, tc := range []struct {
		name, qs, wantCode string
	}{
		{"bad since_id", "/api/ingest/vehicle/trips?since_id=notanumber", "invalid_since_id"},
		{"bad limit", "/api/ingest/vehicle/trips?limit=notanumber", "invalid_limit"},
		{"zero limit", "/api/ingest/vehicle/trips?limit=0", "invalid_limit"},
		{"negative limit", "/api/ingest/vehicle/trips?limit=-1", "invalid_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, code := signAndGet(t, c, tc.qs)
			if status != 400 || code != tc.wantCode {
				t.Fatalf("status/code = %d/%s, want 400/%s", status, code, tc.wantCode)
			}
		})
	}
}

func TestVehicleTrips_HugeLimitClampsInsteadOfErroring(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleTrips = []dashcamtest.VehicleTrip{{ID: 1, DTCs: []string{}}}

	page, err := c.VehicleTrips(ctx, 0, 10000)
	if err != nil {
		t.Fatalf("VehicleTrips with huge limit: %v", err)
	}
	if len(page.Trips) != 1 {
		t.Fatalf("got %d trips, want 1", len(page.Trips))
	}
}

func TestVehicleEvents_Unsigned401(t *testing.T) {
	_, fake := newTestClient(t)
	resp, err := http.Get(fake.URL() + "/api/ingest/vehicle/events")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestVehicleStatus_Unsigned401(t *testing.T) {
	_, fake := newTestClient(t)
	resp, err := http.Get(fake.URL() + "/api/ingest/vehicle/status")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestVehicleTrips_RoutesDisabled404(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleRoutesDisabled = true
	_, err := c.VehicleTrips(ctx, 0, 0)
	if err == nil {
		t.Fatal("expected error for disabled vehicle routes")
	}
	if !dashcam.IsNotFound(err) {
		t.Fatalf("err = %v, want APIError 404", err)
	}
}

func TestVehicleEvents_RoutesDisabled404(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleRoutesDisabled = true
	_, err := c.VehicleEvents(ctx, 0, 0)
	if !dashcam.IsNotFound(err) {
		t.Fatalf("err = %v, want APIError 404", err)
	}
}

// -- .obd.json sidecar (manifest) --------------------------------------

func TestManifest_OBDSidecarAppears(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	rel := "front/2026/09/11/rec1/segment_00001.mp4"
	if err := fake.AddSegment("front", rel, []byte("hello"),
		"2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false); err != nil {
		t.Fatal(err)
	}
	if err := fake.WriteOBDSidecar(rel, []byte(`{"trip_id":1}`)); err != nil {
		t.Fatal(err)
	}

	page, err := c.Manifest(ctx, "", "", 0)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(page.Segments) != 1 {
		t.Fatalf("got %d segments, want 1", len(page.Segments))
	}
	if _, ok := page.Segments[0].Sidecars["obd"]; !ok {
		t.Fatalf("sidecars = %+v, want an obd entry", page.Segments[0].Sidecars)
	}
}

func TestVehicleStatus_RoutesDisabled404(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	fake.VehicleRoutesDisabled = true
	_, err := c.VehicleStatus(ctx)
	if !dashcam.IsNotFound(err) {
		t.Fatalf("err = %v, want APIError 404", err)
	}
}
