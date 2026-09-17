// queries_vehicle.go is the data-access layer for schema v10's OBD vehicle
// telemetry: `vehicle_trips`/`vehicle_events` (the recorder's own trip/event
// summaries, pulled via internal/dashcam's VehicleTrips/VehicleEvents and
// upserted here), the vehicle_trips_cursor/vehicle_events_cursor resume
// tokens on a kind='dashcam' source, and the linking pass that ties an
// event to the manager's own segments/trips (VehicleEventsLink, shaped
// after PullsLinkSegments in queries_dashcam.go).
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// vehicleTimeLayout/vehicleTZOnce/vehicleTZLoc/vehicleLocalLocation/
// parseVehicleLocalTime duplicate internal/scan/sidecar.go's unexported
// parseLocalTime (deliberately, not imported: internal/scan already
// imports internal/db to write scan results, so internal/db importing
// internal/scan back would be a cycle — see scan.ParseLocalTime's doc
// comment). Any drift between the two must be fixed in both places.
const vehicleTimeLayout = "2006-01-02T15:04:05.999999999"

var (
	vehicleTZOnce sync.Once
	vehicleTZLoc  *time.Location
)

func vehicleLocalLocation() *time.Location {
	vehicleTZOnce.Do(func() {
		vehicleTZLoc = time.Local
		if tz := os.Getenv("TZ"); tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				vehicleTZLoc = loc
			}
		}
	})
	return vehicleTZLoc
}

func parseVehicleLocalTime(s string) (time.Time, error) {
	return time.ParseInLocation(vehicleTimeLayout, s, vehicleLocalLocation())
}

// VehicleTrip is the manager-side shape of one recorder OBD trip summary,
// passed to VehicleTripsUpsert. StartTime/EndTime are the recorder's raw
// naive local ISO timestamps exactly as the client hands them back (e.g.
// dashcam.VehicleTrip.StartTime) — VehicleTripsUpsert parses them with
// scan.ParseLocalTime and stores RFC3339 UTC, matching every other
// wall-clock column in this schema (segments.start_time, trips.started_at,
// ...) so cross-table time comparisons stay clock-consistent regardless of
// the container's TZ. MIL/HardBrakes/HardAccels are plain ints (the wire's
// nullable pointers, coalesced to 0 by the caller) since the DB columns are
// NOT NULL DEFAULT 0.
type VehicleTrip struct {
	RemoteTripID  string
	StartTime     string // raw naive local, "" if absent
	EndTime       string
	StartT        *float64
	EndT          *float64
	DistanceM     *float64
	FuelL         *float64
	FuelPctStart  *float64
	FuelPctEnd    *float64
	MaxSpeedKph   *float64
	AvgSpeedKph   *float64
	BattMinV      *float64
	BattMaxV      *float64
	CoolantMaxC   *float64
	SampleCount   int
	DTCs          []string
	MIL           int
	HardBrakes    int
	HardAccels    int
	Speeding      int
	Idling        int
	Overheat      int
	ChargingFault int
	ColdHard      int
	Cornering     int
	Stall         int
}

// VehicleEvent is the manager-side shape of one recorder OBD driving event,
// passed to VehicleEventsUpsert. WallTime is the recorder's raw naive local
// ISO timestamp (dashcam.VehicleEvent.WallTime) — parsed/stored the same
// way VehicleTrip's StartTime/EndTime are. RemoteTripID is the recorder-
// side trip id as a string ("" if the event's trip_id was absent), used to
// resolve this row's vehicle_trip_id FK against a same-source
// vehicle_trips row — which may not exist yet at first upsert (the
// recorder can emit an event before its trip closes) and is re-resolved on
// every upsert so a later re-pull backfills it.
type VehicleEvent struct {
	RemoteEventID string
	EventType     string
	WallTime      string // raw naive local, "" if absent
	T             *float64
	DurationSec   *float64
	Peak          *float64
	SpeedKph      *float64
	RPM           *float64
	RoadName      *string
	LimitKph      *float64
	RemoteTripID  string
}

// localTimeToRFC3339UTC parses raw (a naive local ISO timestamp, as the
// recorder emits) with scan.ParseLocalTime and formats it RFC3339 UTC for
// storage. Returns (nil, nil) for an empty raw string.
func localTimeToRFC3339UTC(raw string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := parseVehicleLocalTime(raw)
	if err != nil {
		return nil, fmt.Errorf("db: parse local time %q: %w", raw, err)
	}
	s := t.UTC().Format(time.RFC3339)
	return &s, nil
}

// dtcsJSON encodes dtcs as a JSON array for storage, or nil (SQL NULL) for
// an empty/absent list.
func dtcsJSON(dtcs []string) (*string, error) {
	if len(dtcs) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(dtcs)
	if err != nil {
		return nil, fmt.Errorf("db: encode dtcs: %w", err)
	}
	s := string(b)
	return &s, nil
}

// VehicleTripsUpsert inserts or updates one vehicle_trips row, keyed on
// (dashcam_source_id, remote_trip_id) — a re-pull (e.g. the tail re-fetch
// dashcam_pull always does) upserts rather than duplicates, and every
// mutable field is refreshed so a trip that was still open (no end_time)
// on an earlier pull picks up its final summary on a later one.
func (d *DB) VehicleTripsUpsert(ctx context.Context, dashcamSourceID int64, t VehicleTrip) (int64, error) {
	startTime, err := localTimeToRFC3339UTC(t.StartTime)
	if err != nil {
		return 0, err
	}
	endTime, err := localTimeToRFC3339UTC(t.EndTime)
	if err != nil {
		return 0, err
	}
	dtcs, err := dtcsJSON(t.DTCs)
	if err != nil {
		return 0, err
	}

	var id int64
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO vehicle_trips
				(dashcam_source_id, remote_trip_id, start_time, end_time, start_t, end_t,
				 distance_m, fuel_l, fuel_pct_start, fuel_pct_end, max_speed_kph, avg_speed_kph,
				 batt_min_v, batt_max_v, coolant_max_c, sample_count, dtcs, mil,
				 hard_brakes, hard_accels, speeding, idling, overheat, charging_fault,
				 cold_hard, cornering, stall)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(dashcam_source_id, remote_trip_id) DO UPDATE SET
				start_time = excluded.start_time,
				end_time = excluded.end_time,
				start_t = excluded.start_t,
				end_t = excluded.end_t,
				distance_m = excluded.distance_m,
				fuel_l = excluded.fuel_l,
				fuel_pct_start = excluded.fuel_pct_start,
				fuel_pct_end = excluded.fuel_pct_end,
				max_speed_kph = excluded.max_speed_kph,
				avg_speed_kph = excluded.avg_speed_kph,
				batt_min_v = excluded.batt_min_v,
				batt_max_v = excluded.batt_max_v,
				coolant_max_c = excluded.coolant_max_c,
				sample_count = excluded.sample_count,
				dtcs = excluded.dtcs,
				mil = excluded.mil,
				hard_brakes = excluded.hard_brakes,
				hard_accels = excluded.hard_accels,
				speeding = excluded.speeding,
				idling = excluded.idling,
				overheat = excluded.overheat,
				charging_fault = excluded.charging_fault,
				cold_hard = excluded.cold_hard,
				cornering = excluded.cornering,
				stall = excluded.stall
			RETURNING id`,
			dashcamSourceID, t.RemoteTripID, startTime, endTime, t.StartT, t.EndT,
			t.DistanceM, t.FuelL, t.FuelPctStart, t.FuelPctEnd, t.MaxSpeedKph, t.AvgSpeedKph,
			t.BattMinV, t.BattMaxV, t.CoolantMaxC, t.SampleCount, dtcs, t.MIL,
			t.HardBrakes, t.HardAccels, t.Speeding, t.Idling, t.Overheat, t.ChargingFault,
			t.ColdHard, t.Cornering, t.Stall,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: vehicle trips upsert: %w", err)
	}
	return id, nil
}

// VehicleEventsUpsert inserts or updates one vehicle_events row, keyed on
// (dashcam_source_id, remote_event_id). remote_trip_id and the resolved
// vehicle_trip_id FK are refreshed on every upsert (not just insert) since
// the recorder can emit an event before its trip closes — a later re-pull
// (dashcam_pull's tail re-fetch, since_id = cursor-20 for trips) backfills
// remote_trip_id, and this re-resolves vehicle_trip_id against
// vehicle_trips at that point too.
func (d *DB) VehicleEventsUpsert(ctx context.Context, dashcamSourceID int64, e VehicleEvent) (int64, error) {
	wallTime, err := localTimeToRFC3339UTC(e.WallTime)
	if err != nil {
		return 0, err
	}
	var remoteTripID any
	if e.RemoteTripID != "" {
		remoteTripID = e.RemoteTripID
	}

	var id int64
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO vehicle_events
				(dashcam_source_id, remote_event_id, event_type, wall_time, t, duration_sec,
				 peak, speed_kph, rpm, road_name, limit_kph, remote_trip_id, vehicle_trip_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				(SELECT id FROM vehicle_trips WHERE dashcam_source_id = ? AND remote_trip_id = ?))
			ON CONFLICT(dashcam_source_id, remote_event_id) DO UPDATE SET
				event_type = excluded.event_type,
				wall_time = excluded.wall_time,
				t = excluded.t,
				duration_sec = excluded.duration_sec,
				peak = excluded.peak,
				speed_kph = excluded.speed_kph,
				rpm = excluded.rpm,
				road_name = excluded.road_name,
				limit_kph = excluded.limit_kph,
				remote_trip_id = excluded.remote_trip_id,
				vehicle_trip_id = (SELECT id FROM vehicle_trips WHERE dashcam_source_id = ? AND remote_trip_id = ?)
			RETURNING id`,
			dashcamSourceID, e.RemoteEventID, e.EventType, wallTime, e.T, e.DurationSec,
			e.Peak, e.SpeedKph, e.RPM, e.RoadName, e.LimitKph, remoteTripID,
			dashcamSourceID, remoteTripID,
			dashcamSourceID, remoteTripID,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: vehicle events upsert: %w", err)
	}
	return id, nil
}

// DashcamGetVehicleCursors returns id's vehicle_trips_cursor/
// vehicle_events_cursor — opaque "last id seen" resume tokens for the pull
// job's vehicle-trips/vehicle-events sub-step, nil when nothing has been
// pulled yet.
func (d *DB) DashcamGetVehicleCursors(ctx context.Context, id int64) (tripsCursor, eventsCursor *int64, err error) {
	err = d.read.QueryRowContext(ctx,
		`SELECT vehicle_trips_cursor, vehicle_events_cursor FROM sources WHERE id = ? AND kind = 'dashcam'`,
		id).Scan(&tripsCursor, &eventsCursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("db: dashcam get vehicle cursors %d: %w", id, err)
	}
	return tripsCursor, eventsCursor, nil
}

// DashcamSetVehicleCursors persists id's vehicle_trips_cursor/
// vehicle_events_cursor.
func (d *DB) DashcamSetVehicleCursors(ctx context.Context, id int64, tripsCursor, eventsCursor *int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sources SET vehicle_trips_cursor = ?, vehicle_events_cursor = ? WHERE id = ? AND kind = 'dashcam'`,
			tripsCursor, eventsCursor, id)
		if err != nil {
			return fmt.Errorf("db: dashcam set vehicle cursors %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: dashcam set vehicle cursors %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DashcamSourceIDsForTargetShare returns the ids of every kind='dashcam'
// source whose target_share_id is shareID — used both by the scan job (to
// decide, cheaply, whether a just-scanned share is any dashcam's pull
// target and so worth a VehicleEventsLink pass) and by VehicleTripSummary
// (to find which vehicle_trips rows could possibly overlap a manager
// trip's span).
func (d *DB) DashcamSourceIDsForTargetShare(ctx context.Context, shareID int64) ([]int64, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id FROM sources WHERE kind = 'dashcam' AND target_share_id = ?`, shareID)
	if err != nil {
		return nil, fmt.Errorf("db: dashcam source ids for target share %d: %w", shareID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: dashcam source ids for target share %d: %w", shareID, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// pendingVehicleEvent is one segment_id IS NULL / trip_id IS NULL
// vehicle_events row VehicleEventsLink is trying to link.
type pendingVehicleEvent struct {
	id       int64
	wallTime string
}

// VehicleEventsLink links every not-yet-linked (segment_id IS NULL AND
// trip_id IS NULL) vehicle_events row under dashcamSourceID to the
// manager's own data, in two passes:
//
//  1. Exact: the front segment (under shareSourceID, the share the
//     dashcam's files land on) whose [start_time, end_time) contains
//     wall_time. Sets segment_id, trip_id (the segment's own trip_id),
//     offset_s (wall_time - segment start, seconds), and lat/lon from the
//     segment's nearest segment_gps point to wall_time.
//  2. Fallback (no segment matched — live data: engine-off stalls and
//     gaps at trip edges): the manager trip (shareSourceID's trips row)
//     whose [started_at-120s, ended_at+120s] window contains wall_time.
//     Sets trip_id and offset_s (clamped to the trip's own span), leaves
//     segment_id/lat/lon NULL.
//
// An event matching neither stays fully NULL — still listed, just
// unlinked — and is retried by a later call (e.g. once a scan has indexed
// the segment its wall_time falls in). Returns how many rows were linked.
func (d *DB) VehicleEventsLink(ctx context.Context, dashcamSourceID, shareSourceID int64) (int, error) {
	var n int
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, wall_time FROM vehicle_events
			 WHERE dashcam_source_id = ? AND segment_id IS NULL AND trip_id IS NULL
			   AND wall_time IS NOT NULL AND wall_time != ''`,
			dashcamSourceID)
		if err != nil {
			return fmt.Errorf("db: vehicle events link: select pending: %w", err)
		}
		var pending []pendingVehicleEvent
		for rows.Next() {
			var p pendingVehicleEvent
			if err := rows.Scan(&p.id, &p.wallTime); err != nil {
				rows.Close()
				return fmt.Errorf("db: vehicle events link: scan pending: %w", err)
			}
			pending = append(pending, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, p := range pending {
			wt, err := time.Parse(time.RFC3339, p.wallTime)
			if err != nil {
				continue // unparseable wall_time: leave unlinked
			}

			var segID int64
			var segTripID sql.NullInt64
			var startStr string
			segErr := tx.QueryRowContext(ctx,
				`SELECT id, trip_id, start_time FROM segments
				 WHERE source_id = ? AND camera = 'front'
				   AND start_time <= ? AND end_time IS NOT NULL AND end_time > ?
				 ORDER BY start_time DESC LIMIT 1`,
				shareSourceID, p.wallTime, p.wallTime).Scan(&segID, &segTripID, &startStr)
			if segErr != nil && !errors.Is(segErr, sql.ErrNoRows) {
				return fmt.Errorf("db: vehicle events link: match segment: %w", segErr)
			}
			if segErr == nil {
				startT, perr := time.Parse(time.RFC3339, startStr)
				if perr != nil {
					continue
				}
				offset := wt.Sub(startT).Seconds()

				lat, lon, gerr := nearestSegmentGPS(ctx, tx, segID, wt)
				if gerr != nil {
					return gerr
				}

				var tripIDVal any
				if segTripID.Valid {
					tripIDVal = segTripID.Int64
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE vehicle_events SET segment_id = ?, trip_id = ?, offset_s = ?, lat = ?, lon = ? WHERE id = ?`,
					segID, tripIDVal, offset, lat, lon, p.id,
				); err != nil {
					return fmt.Errorf("db: vehicle events link: update (segment) %d: %w", p.id, err)
				}
				n++
				continue
			}

			// Fallback: nearest-window manager trip.
			tripID, offset, ok, ferr := fallbackTripWindow(ctx, tx, shareSourceID, wt)
			if ferr != nil {
				return ferr
			}
			if !ok {
				continue // truly unmatched — stays NULL
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE vehicle_events SET trip_id = ?, offset_s = ? WHERE id = ?`,
				tripID, offset, p.id,
			); err != nil {
				return fmt.Errorf("db: vehicle events link: update (fallback) %d: %w", p.id, err)
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// nearestSegmentGPS returns the lat/lon of segID's segment_gps point
// closest in time to wt, as (nil, nil) if segID has no gps points at all.
func nearestSegmentGPS(ctx context.Context, tx *sql.Tx, segID int64, wt time.Time) (lat, lon any, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT t, lat, lon FROM segment_gps WHERE segment_id = ?`, segID)
	if err != nil {
		return nil, nil, fmt.Errorf("db: vehicle events link: segment gps %d: %w", segID, err)
	}
	defer rows.Close()

	bestDiff := math.Inf(1)
	var bestLat, bestLon float64
	found := false
	for rows.Next() {
		var tStr string
		var la, lo float64
		if err := rows.Scan(&tStr, &la, &lo); err != nil {
			return nil, nil, fmt.Errorf("db: vehicle events link: scan segment gps %d: %w", segID, err)
		}
		gt, err := time.Parse(time.RFC3339, tStr)
		if err != nil {
			continue
		}
		diff := math.Abs(gt.Sub(wt).Seconds())
		if diff < bestDiff {
			bestDiff, bestLat, bestLon, found = diff, la, lo, true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("db: vehicle events link: segment gps %d: %w", segID, err)
	}
	if !found {
		return nil, nil, nil
	}
	return bestLat, bestLon, nil
}

// fallbackTripWindow finds the shareSourceID trip whose
// [started_at-120s, ended_at+120s] window contains wt, returning its id
// and an offset_s clamped to the trip's own [started_at, ended_at] span
// (not the widened window). ok is false when no trip's window contains wt.
func fallbackTripWindow(ctx context.Context, tx *sql.Tx, shareSourceID int64, wt time.Time) (tripID int64, offset float64, ok bool, err error) {
	const fallbackWindow = 120 * time.Second

	rows, err := tx.QueryContext(ctx,
		`SELECT id, started_at, ended_at FROM trips
		 WHERE source_id = ? AND started_at IS NOT NULL AND ended_at IS NOT NULL`,
		shareSourceID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("db: vehicle events link: fallback trips: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var tid int64
		var sa, ea string
		if err := rows.Scan(&tid, &sa, &ea); err != nil {
			return 0, 0, false, fmt.Errorf("db: vehicle events link: scan fallback trip: %w", err)
		}
		st, err1 := time.Parse(time.RFC3339, sa)
		et, err2 := time.Parse(time.RFC3339, ea)
		if err1 != nil || err2 != nil {
			continue
		}
		lo := st.Add(-fallbackWindow)
		hi := et.Add(fallbackWindow)
		if wt.Before(lo) || wt.After(hi) {
			continue
		}
		off := wt.Sub(st).Seconds()
		span := et.Sub(st).Seconds()
		if off < 0 {
			off = 0
		} else if off > span {
			off = span
		}
		return tid, off, true, rows.Err()
	}
	return 0, 0, false, rows.Err()
}

// VehicleTripSummary is the per-manager-trip vehicle telemetry rollup
// Task 6's handlers read: distance/fuel/max-speed summed exactly (no
// double counting) from segment_obd over the manager trip's own front
// segments; event counts by type from vehicle_events.trip_id; DTC/MIL/
// battery/coolant unioned from every vehicle_trips row (across whichever
// dashcam source(s) target this trip's share) whose span overlaps the
// manager trip's [started_at, ended_at] — recorder trips and manager
// trips split on different boundaries (engine-on/off/60s-gap vs
// TripGapSplit), so this is a read-time rollup rather than a stored link.
type VehicleTripSummary struct {
	TripID                    int64
	DistanceM                 float64
	FuelUsedL                 float64
	MaxSpeedKph               float64
	EventCounts               map[string]int
	DTCs                      []string
	MIL                       bool
	BattMinV                  *float64
	BattMaxV                  *float64
	CoolantMaxC               *float64
	OverlappingVehicleTripIDs []int64
}

// VehicleTripSummary computes tripID's VehicleTripSummary. Returns
// ErrNotFound if tripID doesn't exist.
func (d *DB) VehicleTripSummary(ctx context.Context, tripID int64) (VehicleTripSummary, error) {
	sum := VehicleTripSummary{TripID: tripID, EventCounts: map[string]int{}}

	var sourceID int64
	var startedAt string
	var endedAt sql.NullString
	err := d.read.QueryRowContext(ctx,
		`SELECT source_id, started_at, ended_at FROM trips WHERE id = ?`, tripID,
	).Scan(&sourceID, &startedAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return VehicleTripSummary{}, ErrNotFound
	}
	if err != nil {
		return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: %w", tripID, err)
	}

	var distance, fuel, maxSpeed sql.NullFloat64
	if err := d.read.QueryRowContext(ctx, `
		SELECT SUM(so.distance_m), SUM(so.fuel_used_l), MAX(so.max_speed_kph)
		FROM segment_obd so JOIN segments s ON s.id = so.segment_id
		WHERE s.trip_id = ? AND s.camera = 'front'`, tripID,
	).Scan(&distance, &fuel, &maxSpeed); err != nil {
		return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: obd sums: %w", tripID, err)
	}
	sum.DistanceM = distance.Float64
	sum.FuelUsedL = fuel.Float64
	sum.MaxSpeedKph = maxSpeed.Float64

	evRows, err := d.read.QueryContext(ctx,
		`SELECT event_type, COUNT(*) FROM vehicle_events WHERE trip_id = ? GROUP BY event_type`, tripID)
	if err != nil {
		return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: event counts: %w", tripID, err)
	}
	for evRows.Next() {
		var et string
		var c int
		if err := evRows.Scan(&et, &c); err != nil {
			evRows.Close()
			return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: scan event count: %w", tripID, err)
		}
		sum.EventCounts[et] = c
	}
	if err := evRows.Err(); err != nil {
		evRows.Close()
		return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: event counts: %w", tripID, err)
	}
	evRows.Close()

	dashcamIDs, err := d.dashcamSourceIDsForTripSource(ctx, sourceID)
	if err != nil {
		return VehicleTripSummary{}, err
	}
	if len(dashcamIDs) == 0 {
		return sum, nil
	}

	tripStart, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return sum, nil // can't compare; no overlapping vehicle_trips
	}
	tripEnd := time.Now().UTC()
	if endedAt.Valid {
		if t, err := time.Parse(time.RFC3339, endedAt.String); err == nil {
			tripEnd = t
		}
	}

	placeholders := make([]any, len(dashcamIDs))
	qMarks := ""
	for i, id := range dashcamIDs {
		placeholders[i] = id
		if i > 0 {
			qMarks += ","
		}
		qMarks += "?"
	}
	vtRows, err := d.read.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, start_time, end_time, dtcs, mil, batt_min_v, batt_max_v, coolant_max_c
		 FROM vehicle_trips
		 WHERE dashcam_source_id IN (%s) AND start_time IS NOT NULL AND end_time IS NOT NULL`, qMarks),
		placeholders...)
	if err != nil {
		return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: overlapping vehicle_trips: %w", tripID, err)
	}
	defer vtRows.Close()

	dtcSet := map[string]bool{}
	for vtRows.Next() {
		var vtID int64
		var vs, ve string
		var dtcs sql.NullString
		var mil int
		var battMin, battMax, coolant sql.NullFloat64
		if err := vtRows.Scan(&vtID, &vs, &ve, &dtcs, &mil, &battMin, &battMax, &coolant); err != nil {
			return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: scan vehicle_trips: %w", tripID, err)
		}
		vst, err1 := time.Parse(time.RFC3339, vs)
		vet, err2 := time.Parse(time.RFC3339, ve)
		if err1 != nil || err2 != nil {
			continue
		}
		if overlapSeconds(tripStart, tripEnd, vst, vet) <= 0 {
			continue
		}
		sum.OverlappingVehicleTripIDs = append(sum.OverlappingVehicleTripIDs, vtID)
		if mil != 0 {
			sum.MIL = true
		}
		if dtcs.Valid {
			var list []string
			if err := json.Unmarshal([]byte(dtcs.String), &list); err == nil {
				for _, code := range list {
					dtcSet[code] = true
				}
			}
		}
		if battMin.Valid && (sum.BattMinV == nil || battMin.Float64 < *sum.BattMinV) {
			v := battMin.Float64
			sum.BattMinV = &v
		}
		if battMax.Valid && (sum.BattMaxV == nil || battMax.Float64 > *sum.BattMaxV) {
			v := battMax.Float64
			sum.BattMaxV = &v
		}
		if coolant.Valid && (sum.CoolantMaxC == nil || coolant.Float64 > *sum.CoolantMaxC) {
			v := coolant.Float64
			sum.CoolantMaxC = &v
		}
	}
	if err := vtRows.Err(); err != nil {
		return VehicleTripSummary{}, fmt.Errorf("db: vehicle trip summary %d: overlapping vehicle_trips: %w", tripID, err)
	}
	for code := range dtcSet {
		sum.DTCs = append(sum.DTCs, code)
	}

	return sum, nil
}

// dashcamSourceIDsForTripSource resolves the kind='dashcam' source ids
// that pull onto the same share a manager trip's own (kind='share')
// source scans — i.e. the dashcam(s) whose footage this trip's segments
// could have come from, and so whose vehicle_trips could overlap it.
func (d *DB) dashcamSourceIDsForTripSource(ctx context.Context, tripSourceID int64) ([]int64, error) {
	src, err := d.SourceGet(ctx, tripSourceID)
	if err != nil {
		return nil, fmt.Errorf("db: dashcam source ids for trip source %d: %w", tripSourceID, err)
	}
	if src.ShareID == nil {
		return nil, nil
	}
	return d.DashcamSourceIDsForTargetShare(ctx, *src.ShareID)
}

// overlapSeconds returns the overlap, in seconds, between two [start,end)
// intervals — 0 if they don't overlap (or either interval is degenerate).
// Duplicated (deliberately, not imported) from internal/scan/pair.go's
// unexported helper of the same name/shape, since that package's version
// is unexported and this package already re-derives scan's local-time
// parsing the same way (see scan.ParseLocalTime).
func overlapSeconds(aStart, aEnd, bStart, bEnd time.Time) float64 {
	lo := aStart
	if bStart.After(lo) {
		lo = bStart
	}
	hi := aEnd
	if bEnd.Before(hi) {
		hi = bEnd
	}
	if !hi.After(lo) {
		return 0
	}
	return hi.Sub(lo).Seconds()
}
