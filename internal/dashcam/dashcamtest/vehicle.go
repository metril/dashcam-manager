package dashcamtest

import (
	"net/http"
	"sort"
	"strconv"
)

// VehicleTrip and VehicleEvent are hand-built response fixtures for
// /api/ingest/vehicle/{trips,events} — deliberately not the same type as
// internal/dashcam's client structs (see package doc comment at the top
// of fake.go for why). Tests populate Fake.VehicleTrips/VehicleEvents
// directly; the fake sorts/pages/filters them at request time.
type VehicleTrip struct {
	ID           int64
	StartTime    string
	EndTime      string
	StartT       *float64
	EndT         *float64
	DistanceM    *float64
	FuelL        *float64
	FuelPctStart *float64
	FuelPctEnd   *float64
	MaxSpeedKph  *float64
	AvgSpeedKph  *float64
	BattMinV     *float64
	BattMaxV     *float64
	CoolantMaxC  *float64
	SampleCount  int
	DTCs         []string
	// MIL and the flag counters below are sqlite INTEGER columns the
	// recorder emits as JSON numbers (0/1), never JSON bools — the fake
	// mirrors that on the wire in toJSON below.
	MIL           *int
	HardBrakes    *int
	HardAccels    *int
	Speeding      int
	Idling        int
	Overheat      int
	ChargingFault int
	ColdHard      int
	Cornering     int
	Stall         int
}

type VehicleEvent struct {
	ID          int64
	EventType   string
	WallTime    string
	T           *float64
	DurationSec *float64
	Peak        *float64
	SpeedKph    *float64
	RPM         *float64
	SegmentPath string
	GPSLat      *float64
	GPSLon      *float64
	TripID      *int64
	// RoadName/LimitKph are newer-recorder-branch-only fields; pointers so
	// a fixture can omit them to simulate an older recorder response.
	RoadName *string
	LimitKph *float64
}

func (t VehicleTrip) toJSON() map[string]any {
	dtcs := t.DTCs
	if dtcs == nil {
		dtcs = []string{}
	}
	return map[string]any{
		"id": t.ID, "start_time": t.StartTime, "end_time": t.EndTime,
		"start_t": t.StartT, "end_t": t.EndT,
		"distance_m": t.DistanceM, "fuel_l": t.FuelL,
		"fuel_pct_start": t.FuelPctStart, "fuel_pct_end": t.FuelPctEnd,
		"max_speed_kph": t.MaxSpeedKph, "avg_speed_kph": t.AvgSpeedKph,
		"batt_min_v": t.BattMinV, "batt_max_v": t.BattMaxV,
		"coolant_max_c": t.CoolantMaxC, "sample_count": t.SampleCount,
		"dtcs": dtcs, "mil": t.MIL,
		"hard_brakes": t.HardBrakes, "hard_accels": t.HardAccels,
		"speeding": t.Speeding, "idling": t.Idling, "overheat": t.Overheat,
		"charging_fault": t.ChargingFault, "cold_hard": t.ColdHard,
		"cornering": t.Cornering, "stall": t.Stall,
	}
}

func (e VehicleEvent) toJSON() map[string]any {
	return map[string]any{
		"id": e.ID, "event_type": e.EventType, "wall_time": e.WallTime,
		"t": e.T, "duration_sec": e.DurationSec, "peak": e.Peak,
		"speed_kph": e.SpeedKph, "rpm": e.RPM, "segment_path": e.SegmentPath,
		"gps_lat": e.GPSLat, "gps_lon": e.GPSLon, "trip_id": e.TripID,
		"road_name": e.RoadName, "limit_kph": e.LimitKph,
	}
}

// vehiclePageParams parses since_id/limit the way the recorder does:
// since_id exclusive lower bound (default 0, 400 invalid_since_id if not
// an integer), limit default 200, 400 invalid_limit if not an integer or
// less than 1 — only the upper bound (>500) clamps rather than erroring.
func (f *Fake) vehiclePageParams(w http.ResponseWriter, r *http.Request) (sinceID int64, limit int, ok bool) {
	q := r.URL.Query()
	sinceID = 0
	if raw := q.Get("since_id"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			f.writeError(w, 400, "invalid_since_id")
			return 0, 0, false
		}
		sinceID = n
	}
	limit = 200
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			f.writeError(w, 400, "invalid_limit")
			return 0, 0, false
		}
		limit = n
	}
	if limit > 500 {
		limit = 500
	}
	return sinceID, limit, true
}

// writeNotFound answers with aiohttp's plain 404 (no JSON body, no
// "error" code) — the shape an older recorder without the vehicle-
// telemetry routes actually returns for an unregistered path, as opposed
// to the ingest API's {"error":code} shape for a recognized-but-rejected
// request.
func writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404: Not Found"))
}

func (f *Fake) handleVehicleTrips(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	disabled := f.VehicleRoutesDisabled
	all := append([]VehicleTrip(nil), f.VehicleTrips...)
	f.mu.Unlock()
	if disabled {
		writeNotFound(w)
		return
	}

	sinceID, limit, ok := f.vehiclePageParams(w, r)
	if !ok {
		return
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	var filtered []VehicleTrip
	for _, t := range all {
		if t.ID > sinceID {
			filtered = append(filtered, t)
		}
	}
	full := len(filtered) >= limit && len(filtered) > 0
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	trips := make([]map[string]any, 0, len(filtered))
	for _, t := range filtered {
		trips = append(trips, t.toJSON())
	}
	var nextSinceID any
	if full && len(filtered) > 0 {
		nextSinceID = filtered[len(filtered)-1].ID
	}
	writeJSON(w, 200, map[string]any{
		"trips": trips, "count": len(trips), "next_since_id": nextSinceID,
	})
}

func (f *Fake) handleVehicleEvents(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	disabled := f.VehicleRoutesDisabled
	all := append([]VehicleEvent(nil), f.VehicleEvents...)
	f.mu.Unlock()
	if disabled {
		writeNotFound(w)
		return
	}

	sinceID, limit, ok := f.vehiclePageParams(w, r)
	if !ok {
		return
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	var filtered []VehicleEvent
	for _, e := range all {
		if e.ID > sinceID {
			filtered = append(filtered, e)
		}
	}
	full := len(filtered) >= limit && len(filtered) > 0
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	events := make([]map[string]any, 0, len(filtered))
	for _, e := range filtered {
		events = append(events, e.toJSON())
	}
	var nextSinceID any
	if full && len(filtered) > 0 {
		nextSinceID = filtered[len(filtered)-1].ID
	}
	writeJSON(w, 200, map[string]any{
		"events": events, "count": len(events), "next_since_id": nextSinceID,
	})
}

func (f *Fake) handleVehicleStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	disabled := f.VehicleRoutesDisabled
	status := f.VehicleStatus
	f.mu.Unlock()
	if disabled {
		writeNotFound(w)
		return
	}
	if status == nil {
		status = map[string]any{"obd": map[string]any{"enabled": false}, "trip": nil, "last_trip": nil}
	}
	writeJSON(w, 200, status)
}
