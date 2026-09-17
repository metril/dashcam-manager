package dashcam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// VehicleTrip mirrors one entry of GET /api/ingest/vehicle/trips' trips
// array (see resources/ingest.py's vehicle-trip endpoint, Task 1). Numeric
// fields the recorder can leave unset (e.g. a trip with no fuel telemetry)
// decode as nil pointers rather than zero values.
type VehicleTrip struct {
	ID            int64    `json:"id"`
	StartTime     string   `json:"start_time"`
	EndTime       string   `json:"end_time"`
	StartT        *float64 `json:"start_t"`
	EndT          *float64 `json:"end_t"`
	DistanceM     *float64 `json:"distance_m"`
	FuelL         *float64 `json:"fuel_l"`
	FuelPctStart  *float64 `json:"fuel_pct_start"`
	FuelPctEnd    *float64 `json:"fuel_pct_end"`
	MaxSpeedKph   *float64 `json:"max_speed_kph"`
	AvgSpeedKph   *float64 `json:"avg_speed_kph"`
	BattMinV      *float64 `json:"batt_min_v"`
	BattMaxV      *float64 `json:"batt_max_v"`
	CoolantMaxC   *float64 `json:"coolant_max_c"`
	SampleCount   int      `json:"sample_count"`
	DTCs          []string `json:"dtcs"`
	MIL           bool     `json:"mil"`
	HardBrakes    *int     `json:"hard_brakes"`
	HardAccels    *int     `json:"hard_accels"`
	Speeding      bool     `json:"speeding"`
	Idling        bool     `json:"idling"`
	Overheat      bool     `json:"overheat"`
	ChargingFault bool     `json:"charging_fault"`
	ColdHard      bool     `json:"cold_hard"`
	Cornering     bool     `json:"cornering"`
	Stall         bool     `json:"stall"`
}

// VehicleEvent mirrors one entry of GET /api/ingest/vehicle/events' events
// array.
type VehicleEvent struct {
	ID          int64    `json:"id"`
	EventType   string   `json:"event_type"`
	WallTime    string   `json:"wall_time"`
	T           *float64 `json:"t"`
	DurationSec *float64 `json:"duration_sec"`
	Peak        *float64 `json:"peak"`
	SpeedKph    *float64 `json:"speed_kph"`
	RPM         *float64 `json:"rpm"`
	SegmentPath string   `json:"segment_path"`
	GPSLat      *float64 `json:"gps_lat"`
	GPSLon      *float64 `json:"gps_lon"`
	TripID      *int64   `json:"trip_id"`
	RoadName    *string  `json:"road_name"`
	LimitKph    *float64 `json:"limit_kph"`
}

// VehicleTripsPage is one page of GET /api/ingest/vehicle/trips.
// NextSinceID is nil when there is no further cursor to page with (the
// server sent next_since_id: null, meaning this page was short/empty).
type VehicleTripsPage struct {
	Trips       []VehicleTrip `json:"trips"`
	Count       int           `json:"count"`
	NextSinceID *int64        `json:"next_since_id"`
}

// VehicleEventsPage is one page of GET /api/ingest/vehicle/events.
type VehicleEventsPage struct {
	Events      []VehicleEvent `json:"events"`
	Count       int            `json:"count"`
	NextSinceID *int64         `json:"next_since_id"`
}

// vehicleQuery builds the since_id/limit query params shared by the three
// vehicle endpoints. sinceID <= 0 and limit <= 0 both omit their param
// (server defaults apply), mirroring Manifest's since/limit handling.
func vehicleQuery(sinceID int64, limit int) string {
	v := url.Values{}
	if sinceID > 0 {
		v.Set("since_id", strconv.FormatInt(sinceID, 10))
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	return buildQuery(v)
}

// VehicleTrips fetches one page of OBD trip summaries. limit <= 0 omits
// the limit query param (server default applies).
func (c *Client) VehicleTrips(ctx context.Context, sinceID int64, limit int) (VehicleTripsPage, error) {
	pathQS := "/api/ingest/vehicle/trips" + vehicleQuery(sinceID, limit)
	resp, err := c.do(ctx, "GET", pathQS, nil)
	if err != nil {
		return VehicleTripsPage{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return VehicleTripsPage{}, fmt.Errorf("dashcam: read vehicle trips: %w", err)
	}
	var page VehicleTripsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return VehicleTripsPage{}, fmt.Errorf("dashcam: decode vehicle trips: %w", err)
	}
	return page, nil
}

// VehicleEvents fetches one page of OBD driving events.
func (c *Client) VehicleEvents(ctx context.Context, sinceID int64, limit int) (VehicleEventsPage, error) {
	pathQS := "/api/ingest/vehicle/events" + vehicleQuery(sinceID, limit)
	resp, err := c.do(ctx, "GET", pathQS, nil)
	if err != nil {
		return VehicleEventsPage{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return VehicleEventsPage{}, fmt.Errorf("dashcam: read vehicle events: %w", err)
	}
	var page VehicleEventsPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return VehicleEventsPage{}, fmt.Errorf("dashcam: decode vehicle events: %w", err)
	}
	return page, nil
}

// VehicleStatus fetches the recorder's live OBD status ({"obd":..,
// "trip":..,"last_trip":..}) as a raw JSON passthrough: the shape is
// recorder-version-dependent, so the manager forwards it rather than
// decoding it into a fixed struct.
func (c *Client) VehicleStatus(ctx context.Context) (json.RawMessage, error) {
	resp, err := c.do(ctx, "GET", "/api/ingest/vehicle/status", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dashcam: read vehicle status: %w", err)
	}
	return json.RawMessage(raw), nil
}

// EachVehicleTrip pages through /api/ingest/vehicle/trips via
// next_since_id, calling fn for every trip in id-ascending order, and
// returns the ID of the last trip delivered to fn (0 if none) — a cursor
// suitable for a future call's sinceID. It stops when a page comes back
// with zero trips or next_since_id comes back JSON null.
func (c *Client) EachVehicleTrip(ctx context.Context, sinceID int64, fn func(VehicleTrip) error) (int64, error) {
	cursor := sinceID
	for {
		page, err := c.VehicleTrips(ctx, cursor, 0)
		if err != nil {
			return cursor, err
		}
		if len(page.Trips) == 0 {
			return cursor, nil
		}
		for _, trip := range page.Trips {
			if err := fn(trip); err != nil {
				return cursor, err
			}
		}
		cursor = page.Trips[len(page.Trips)-1].ID
		if page.NextSinceID == nil {
			return cursor, nil
		}
	}
}

// EachVehicleEvent pages through /api/ingest/vehicle/events the same way
// EachVehicleTrip pages through trips.
func (c *Client) EachVehicleEvent(ctx context.Context, sinceID int64, fn func(VehicleEvent) error) (int64, error) {
	cursor := sinceID
	for {
		page, err := c.VehicleEvents(ctx, cursor, 0)
		if err != nil {
			return cursor, err
		}
		if len(page.Events) == 0 {
			return cursor, nil
		}
		for _, ev := range page.Events {
			if err := fn(ev); err != nil {
				return cursor, err
			}
		}
		cursor = page.Events[len(page.Events)-1].ID
		if page.NextSinceID == nil {
			return cursor, nil
		}
	}
}
