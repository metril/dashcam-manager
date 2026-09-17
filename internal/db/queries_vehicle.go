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
	"strconv"
	"strings"
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
// recorder emits) with parseVehicleLocalTime and formats it RFC3339Nano UTC
// for storage — RFC3339Nano, not plain RFC3339, to exactly match how
// internal/scan/index.go formats segments.start_time/end_time and
// segment_gps.t (time.RFC3339Nano throughout). VehicleEventsLink compares
// these as plain SQL/Go text, and a fractional-second segment boundary
// formatted RFC3339Nano ("...T18:37:00.046Z") sorts lexically *before* a
// same-instant-class value formatted plain RFC3339 with no fraction
// ("...T18:37:00Z") — '.' (0x2E) sorts below 'Z' (0x5A) — which silently
// breaks the [start_time, end_time) comparison for any segment with a
// nonzero fractional second. Returns (nil, nil) for an empty raw string.
func localTimeToRFC3339UTC(raw string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := parseVehicleLocalTime(raw)
	if err != nil {
		return nil, fmt.Errorf("db: parse local time %q: %w", raw, err)
	}
	s := t.UTC().Format(time.RFC3339Nano)
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

// pendingVehicleEvent is one segment_id IS NULL vehicle_events row
// VehicleEventsLink is trying to (re)link.
type pendingVehicleEvent struct {
	id       int64
	wallTime string
}

// VehicleEventsLink links every not-yet-segment-linked (segment_id IS
// NULL) vehicle_events row under dashcamSourceID to the manager's own
// data, in two passes:
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
// The pending set is scoped to segment_id IS NULL only (not also
// trip_id IS NULL, as an earlier version required): the dashcam_pull job
// runs this *before* the scan it enqueues has indexed anything, so on
// every incremental pull an event lands in the fallback pass first (trip_id
// set, segment_id still NULL) and must stay eligible for the exact pass
// once that segment shows up later (this call, or the scan job's own
// call — see scanJobHandler) — requiring trip_id IS NULL too would freeze
// it on the fallback link forever. Re-running the exact pass on an
// already-fallback-linked row is intended to overwrite its trip_id/
// offset_s with the more precise segment-derived values.
//
// An event matching neither pass stays fully NULL — still listed, just
// unlinked — and is retried by a later call. Returns how many rows this
// call updated (segment-linked or (re-)fallback-linked).
func (d *DB) VehicleEventsLink(ctx context.Context, dashcamSourceID, shareSourceID int64) (int, error) {
	var n int
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, wall_time FROM vehicle_events
			 WHERE dashcam_source_id = ? AND segment_id IS NULL
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
		if len(pending) == 0 {
			return nil
		}

		// Fallback trip windows are fetched once per call (not once per
		// pending event — that was an N+1 query pattern) since the
		// candidate set (shareSourceID's own trips) doesn't change during
		// this call.
		windows, err := fetchTripWindows(ctx, tx, shareSourceID)
		if err != nil {
			return err
		}

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

			// Fallback: nearest-window manager trip, against the
			// already-fetched candidate set.
			tripID, offset, ok := matchTripWindow(windows, wt)
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

// tripWindow is one candidate manager trip for the ±120s fallback link
// pass: its id and parsed [start, end] span.
type tripWindow struct {
	id         int64
	start, end time.Time
}

// fetchTripWindows loads every shareSourceID trip with both started_at and
// ended_at set, parsed once — fetched a single time per VehicleEventsLink
// call (not once per pending event, which was an N+1 query pattern) since
// the candidate set doesn't change during that call.
func fetchTripWindows(ctx context.Context, tx *sql.Tx, shareSourceID int64) ([]tripWindow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, started_at, ended_at FROM trips
		 WHERE source_id = ? AND started_at IS NOT NULL AND ended_at IS NOT NULL`,
		shareSourceID)
	if err != nil {
		return nil, fmt.Errorf("db: vehicle events link: fallback trips: %w", err)
	}
	defer rows.Close()

	var out []tripWindow
	for rows.Next() {
		var tid int64
		var sa, ea string
		if err := rows.Scan(&tid, &sa, &ea); err != nil {
			return nil, fmt.Errorf("db: vehicle events link: scan fallback trip: %w", err)
		}
		st, err1 := time.Parse(time.RFC3339, sa)
		et, err2 := time.Parse(time.RFC3339, ea)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, tripWindow{id: tid, start: st, end: et})
	}
	return out, rows.Err()
}

// matchTripWindow finds the windows entry whose [start-120s, end+120s]
// window contains wt, returning its id and an offset_s clamped to the
// trip's own [start, end] span (not the widened window). ok is false when
// no window contains wt.
func matchTripWindow(windows []tripWindow, wt time.Time) (tripID int64, offset float64, ok bool) {
	const fallbackWindow = 120 * time.Second

	for _, w := range windows {
		lo := w.start.Add(-fallbackWindow)
		hi := w.end.Add(fallbackWindow)
		if wt.Before(lo) || wt.After(hi) {
			continue
		}
		off := wt.Sub(w.start).Seconds()
		span := w.end.Sub(w.start).Seconds()
		if off < 0 {
			off = 0
		} else if off > span {
			off = span
		}
		return w.id, off, true
	}
	return 0, 0, false
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
	// start_time <= tripEnd AND end_time >= tripStart is a SQL-pushable
	// (index-usable, once both sides are formatted the same way —
	// RFC3339Nano throughout, see localTimeToRFC3339UTC) superset of "these
	// two spans overlap": it can only ever admit extra non-overlapping rows
	// at the margins (never exclude a real overlap), and the exact overlap
	// check (overlapSeconds, below) still runs in Go over whatever this
	// returns — so a same-format text-comparison edge case here costs at
	// most a few extra candidate rows, never a wrong answer. Without this,
	// every vehicle_trips row for the source(s) was fetched and filtered
	// in Go regardless of date, which doesn't scale with trip history.
	placeholders = append(placeholders, tripEnd.Format(time.RFC3339Nano), tripStart.Format(time.RFC3339Nano))
	vtRows, err := d.read.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, start_time, end_time, dtcs, mil, batt_min_v, batt_max_v, coolant_max_c
		 FROM vehicle_trips
		 WHERE dashcam_source_id IN (%s) AND start_time IS NOT NULL AND end_time IS NOT NULL
		   AND start_time <= ? AND end_time >= ?`, qMarks),
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

// ============================================================================
// Task 6: HTTP API read models. Everything below composes the read-side
// helpers above (VehicleTripSummary, the vehicle_trips/vehicle_events
// tables) into the exact wire shapes cmd/api/handlers_vehicle.go serves —
// appended rather than folded into the functions above, which Task 5's
// review is reading concurrently.
// ============================================================================

// SegmentOBDSummary is GET /api/segments/{id}/obd's summary block — the
// segment_obd row's own aggregate fields, verbatim.
type SegmentOBDSummary struct {
	MaxSpeedKph *float64 `json:"max_speed_kph"`
	AvgRPM      *float64 `json:"avg_rpm"`
	DistanceM   *float64 `json:"distance_m"`
	FuelUsedL   *float64 `json:"fuel_used_l"`
	PointCount  int      `json:"point_count"`
}

// SegmentOBDPoint is one segment_obd_points row rendered relative to its
// segment's start_time (OffsetS, seconds) rather than as an absolute
// timestamp, matching how the frontend already plots segment-relative
// series.
type SegmentOBDPoint struct {
	OffsetS     float64  `json:"offset_s"`
	SpeedKph    *float64 `json:"speed_kph"`
	RPM         *float64 `json:"rpm"`
	ThrottlePct *float64 `json:"throttle_pct"`
	AccelMps2   *float64 `json:"accel_mps2"`
	CoolantC    *float64 `json:"coolant_c"`
	BattV       *float64 `json:"batt_v"`
}

// SegmentOBD is GET /api/segments/{id}/obd's response.
type SegmentOBD struct {
	Summary *SegmentOBDSummary `json:"summary"`
	Points  []SegmentOBDPoint  `json:"points"`
}

// SegmentOBDGet returns segmentID's OBD summary/points. Returns ErrNotFound
// both when the segment itself doesn't exist and when it exists but has no
// segment_obd row at all (the "no telemetry" case the frontend's
// getSegmentOBD treats as a 404, not a null summary).
func (d *DB) SegmentOBDGet(ctx context.Context, segmentID int64) (SegmentOBD, error) {
	var startTime string
	err := d.read.QueryRowContext(ctx, `SELECT start_time FROM segments WHERE id = ?`, segmentID).Scan(&startTime)
	if errors.Is(err, sql.ErrNoRows) {
		return SegmentOBD{}, ErrNotFound
	}
	if err != nil {
		return SegmentOBD{}, fmt.Errorf("db: segment obd get %d: %w", segmentID, err)
	}

	var sum SegmentOBDSummary
	err = d.read.QueryRowContext(ctx, `
		SELECT max_speed_kph, avg_rpm, distance_m, fuel_used_l, point_count
		FROM segment_obd WHERE segment_id = ?`, segmentID,
	).Scan(&sum.MaxSpeedKph, &sum.AvgRPM, &sum.DistanceM, &sum.FuelUsedL, &sum.PointCount)
	if errors.Is(err, sql.ErrNoRows) {
		return SegmentOBD{}, ErrNotFound
	}
	if err != nil {
		return SegmentOBD{}, fmt.Errorf("db: segment obd get %d: summary: %w", segmentID, err)
	}

	rows, err := d.read.QueryContext(ctx, `
		SELECT (julianday(t) - julianday(?)) * 86400.0, speed_kph, rpm, throttle_pct, accel_mps2, coolant_c, batt_v
		FROM segment_obd_points WHERE segment_id = ? ORDER BY t`, startTime, segmentID)
	if err != nil {
		return SegmentOBD{}, fmt.Errorf("db: segment obd get %d: points: %w", segmentID, err)
	}
	defer rows.Close()

	points := []SegmentOBDPoint{}
	for rows.Next() {
		var p SegmentOBDPoint
		if err := rows.Scan(&p.OffsetS, &p.SpeedKph, &p.RPM, &p.ThrottlePct, &p.AccelMps2, &p.CoolantC, &p.BattV); err != nil {
			return SegmentOBD{}, fmt.Errorf("db: segment obd get %d: scan point: %w", segmentID, err)
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return SegmentOBD{}, fmt.Errorf("db: segment obd get %d: points: %w", segmentID, err)
	}

	return SegmentOBD{Summary: &sum, Points: points}, nil
}

// VehicleTripRow is one vehicle_trips row rendered for the HTTP API (GET
// /api/trips/{id}/vehicle's recorder_trips, GET /api/vehicle/trips),
// matching the frontend's VehicleTrip wire type field-for-field.
type VehicleTripRow struct {
	ID            int64    `json:"id"`
	StartTime     *string  `json:"start_time"`
	EndTime       *string  `json:"end_time"`
	DistanceM     *float64 `json:"distance_m"`
	FuelL         *float64 `json:"fuel_l"`
	MaxSpeedKph   *float64 `json:"max_speed_kph"`
	AvgSpeedKph   *float64 `json:"avg_speed_kph"`
	BattMinV      *float64 `json:"batt_min_v"`
	BattMaxV      *float64 `json:"batt_max_v"`
	CoolantMaxC   *float64 `json:"coolant_max_c"`
	DTCs          []string `json:"dtcs"`
	MIL           bool     `json:"mil"`
	HardBrakes    int      `json:"hard_brakes"`
	HardAccels    int      `json:"hard_accels"`
	Speeding      int      `json:"speeding"`
	Idling        int      `json:"idling"`
	Overheat      int      `json:"overheat"`
	ChargingFault int      `json:"charging_fault"`
	ColdHard      int      `json:"cold_hard"`
	Cornering     int      `json:"cornering"`
	Stall         int      `json:"stall"`
}

// vehicleTripRowCols is the column list scanVehicleTripRow expects, shared
// by every query that renders a VehicleTripRow.
const vehicleTripRowCols = `id, start_time, end_time, distance_m, fuel_l, max_speed_kph, avg_speed_kph,
	batt_min_v, batt_max_v, coolant_max_c, dtcs, mil, hard_brakes, hard_accels, speeding, idling,
	overheat, charging_fault, cold_hard, cornering, stall`

// scanVehicleTripRow scans one row shaped by vehicleTripRowCols.
func scanVehicleTripRow(row interface{ Scan(...any) error }) (VehicleTripRow, error) {
	var v VehicleTripRow
	var dtcs sql.NullString
	var mil int
	if err := row.Scan(&v.ID, &v.StartTime, &v.EndTime, &v.DistanceM, &v.FuelL, &v.MaxSpeedKph, &v.AvgSpeedKph,
		&v.BattMinV, &v.BattMaxV, &v.CoolantMaxC, &dtcs, &mil, &v.HardBrakes, &v.HardAccels, &v.Speeding, &v.Idling,
		&v.Overheat, &v.ChargingFault, &v.ColdHard, &v.Cornering, &v.Stall); err != nil {
		return VehicleTripRow{}, err
	}
	v.MIL = mil != 0
	v.DTCs = []string{}
	if dtcs.Valid && dtcs.String != "" {
		_ = json.Unmarshal([]byte(dtcs.String), &v.DTCs)
	}
	return v, nil
}

// VehicleTripsByIDs returns the vehicle_trips rows for the given ids,
// newest-first by start_time — used to render VehicleTripSummary's
// OverlappingVehicleTripIDs as GET /api/trips/{id}/vehicle's
// recorder_trips.
func (d *DB) VehicleTripsByIDs(ctx context.Context, ids []int64) ([]VehicleTripRow, error) {
	if len(ids) == 0 {
		return []VehicleTripRow{}, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := d.read.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM vehicle_trips WHERE id IN (%s) ORDER BY start_time DESC, id DESC`,
		vehicleTripRowCols, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("db: vehicle trips by ids: %w", err)
	}
	defer rows.Close()

	out := []VehicleTripRow{}
	for rows.Next() {
		v, err := scanVehicleTripRow(rows)
		if err != nil {
			return nil, fmt.Errorf("db: vehicle trips by ids: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VehicleTripsFilter selects GET /api/vehicle/trips's page: an inclusive
// [From, To) RFC3339-or-date range (either half empty = unbounded), Cursor
// an opaque offset token from a previous page (mirroring
// TripsFilter/TripsList's own cursor), and Limit the page size.
type VehicleTripsFilter struct {
	From, To string
	Cursor   string
	Limit    int
}

// VehicleTripsList returns a newest-first (start_time DESC) page of
// vehicle_trips rows.
func (d *DB) VehicleTripsList(ctx context.Context, filter VehicleTripsFilter) ([]VehicleTripRow, string, error) {
	offset := 0
	if filter.Cursor != "" {
		n, err := strconv.Atoi(filter.Cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("db: vehicle trips list: invalid cursor %q", filter.Cursor)
		}
		offset = n
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	var where []string
	var args []any
	if filter.From != "" {
		where = append(where, "start_time >= ?")
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, "start_time < ?")
		args = append(args, filter.To)
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	query := fmt.Sprintf(`SELECT %s FROM vehicle_trips %s ORDER BY start_time DESC, id DESC LIMIT ? OFFSET ?`,
		vehicleTripRowCols, whereClause)
	args = append(args, limit+1, offset)

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("db: vehicle trips list: %w", err)
	}
	defer rows.Close()

	var out []VehicleTripRow
	for rows.Next() {
		v, err := scanVehicleTripRow(rows)
		if err != nil {
			return nil, "", fmt.Errorf("db: vehicle trips list: scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("db: vehicle trips list: %w", err)
	}

	nextCursor := ""
	if len(out) > limit {
		out = out[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}
	if out == nil {
		out = []VehicleTripRow{}
	}
	return out, nextCursor, nil
}

// VehicleEventRow is one vehicle_events row rendered for the HTTP API,
// matching the frontend's VehicleEvent wire type. TripID is only
// populated by VehicleEventsList (the global GET /api/vehicle/events
// listing, which spans trips); VehicleEventsForTrip's rows leave it nil
// (omitted via omitempty) since the response is already trip-scoped.
type VehicleEventRow struct {
	ID          int64    `json:"id"`
	EventType   string   `json:"event_type"`
	OffsetS     float64  `json:"offset_s"`
	SegmentID   *int64   `json:"segment_id"`
	TripID      *int64   `json:"trip_id,omitempty"`
	Lat         *float64 `json:"lat"`
	Lon         *float64 `json:"lon"`
	WallTime    *string  `json:"wall_time"`
	DurationSec *float64 `json:"duration_sec"`
	Peak        *float64 `json:"peak"`
	SpeedKph    *float64 `json:"speed_kph"`
	RPM         *float64 `json:"rpm"`
}

// VehicleEventsForTrip returns tripID's vehicle_events, ordered by their
// trip-relative offset: for a segment-linked event, its own
// (segment-relative) offset_s plus that segment's own offset from the
// trip's started_at; for a fallback-linked event (segment_id NULL), the
// already trip-clamped offset_s VehicleEventsLink stored, used as-is.
func (d *DB) VehicleEventsForTrip(ctx context.Context, tripID int64) ([]VehicleEventRow, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT ve.id, ve.event_type, ve.segment_id, ve.lat, ve.lon, ve.wall_time, ve.duration_sec,
			ve.peak, ve.speed_kph, ve.rpm,
			CASE WHEN ve.segment_id IS NOT NULL
				THEN (julianday(s.start_time) - julianday(t.started_at)) * 86400.0 + ve.offset_s
				ELSE ve.offset_s END AS trip_offset_s
		FROM vehicle_events ve
		JOIN trips t ON t.id = ?
		LEFT JOIN segments s ON s.id = ve.segment_id
		WHERE ve.trip_id = ?
		ORDER BY trip_offset_s ASC, ve.id ASC`, tripID, tripID)
	if err != nil {
		return nil, fmt.Errorf("db: vehicle events for trip %d: %w", tripID, err)
	}
	defer rows.Close()

	out := []VehicleEventRow{}
	for rows.Next() {
		var v VehicleEventRow
		if err := rows.Scan(&v.ID, &v.EventType, &v.SegmentID, &v.Lat, &v.Lon, &v.WallTime, &v.DurationSec,
			&v.Peak, &v.SpeedKph, &v.RPM, &v.OffsetS); err != nil {
			return nil, fmt.Errorf("db: vehicle events for trip %d: scan: %w", tripID, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VehicleEventsFilter selects GET /api/vehicle/events's page: Types (nil =
// all event types), an inclusive [From, To) wall_time range, TripID (0 =
// no filter), Cursor an opaque offset token, and Limit the page size.
type VehicleEventsFilter struct {
	Types  []string
	From   string
	To     string
	TripID int64
	Cursor string
	Limit  int
}

// VehicleEventsList returns a newest-first (wall_time DESC) page of
// vehicle_events rows across every trip, with TripID populated on each row
// (unlike VehicleEventsForTrip's already trip-scoped rows). An event with
// no wall_time/offset_s (never linked) sorts last and reports offset_s 0.
func (d *DB) VehicleEventsList(ctx context.Context, filter VehicleEventsFilter) ([]VehicleEventRow, string, error) {
	offset := 0
	if filter.Cursor != "" {
		n, err := strconv.Atoi(filter.Cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("db: vehicle events list: invalid cursor %q", filter.Cursor)
		}
		offset = n
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	var where []string
	var args []any
	if len(filter.Types) > 0 {
		placeholders := make([]string, len(filter.Types))
		for i, ty := range filter.Types {
			placeholders[i] = "?"
			args = append(args, ty)
		}
		where = append(where, fmt.Sprintf("event_type IN (%s)", strings.Join(placeholders, ",")))
	}
	if filter.From != "" {
		where = append(where, "wall_time >= ?")
		args = append(args, filter.From)
	}
	if filter.To != "" {
		where = append(where, "wall_time < ?")
		args = append(args, filter.To)
	}
	if filter.TripID != 0 {
		where = append(where, "trip_id = ?")
		args = append(args, filter.TripID)
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT id, event_type, offset_s, segment_id, trip_id, lat, lon, wall_time, duration_sec, peak, speed_kph, rpm
		FROM vehicle_events %s
		ORDER BY wall_time DESC, id DESC
		LIMIT ? OFFSET ?`, whereClause)
	args = append(args, limit+1, offset)

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("db: vehicle events list: %w", err)
	}
	defer rows.Close()

	var out []VehicleEventRow
	for rows.Next() {
		var v VehicleEventRow
		var offsetS sql.NullFloat64
		if err := rows.Scan(&v.ID, &v.EventType, &offsetS, &v.SegmentID, &v.TripID, &v.Lat, &v.Lon, &v.WallTime,
			&v.DurationSec, &v.Peak, &v.SpeedKph, &v.RPM); err != nil {
			return nil, "", fmt.Errorf("db: vehicle events list: scan: %w", err)
		}
		v.OffsetS = offsetS.Float64
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("db: vehicle events list: %w", err)
	}

	nextCursor := ""
	if len(out) > limit {
		out = out[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}
	if out == nil {
		out = []VehicleEventRow{}
	}
	return out, nextCursor, nil
}

// TripVehicleTotals is GET /api/trips/{id}/vehicle's totals block, built
// from VehicleTripSummary: DistanceM/FuelL/MaxSpeedKph are nil when the
// trip has no segment_obd data at all (VehicleTripSummary's SUM/MAX come
// back as the SQL-NULL-coalesced zero value in that case, so a genuine
// zero reading is indistinguishable from "no data" — an accepted
// imprecision rather than restructuring VehicleTripSummary's return
// shape).
type TripVehicleTotals struct {
	DistanceM   *float64 `json:"distance_m"`
	FuelL       *float64 `json:"fuel_l"`
	MaxSpeedKph *float64 `json:"max_speed_kph"`
	BattMinV    *float64 `json:"batt_min_v"`
	BattMaxV    *float64 `json:"batt_max_v"`
	CoolantMaxC *float64 `json:"coolant_max_c"`
	DTCs        []string `json:"dtcs"`
	MIL         bool     `json:"mil"`
}

// TripVehicleResponse is GET /api/trips/{id}/vehicle's response.
type TripVehicleResponse struct {
	RecorderTrips []VehicleTripRow  `json:"recorder_trips"`
	Totals        TripVehicleTotals `json:"totals"`
	Events        []VehicleEventRow `json:"events"`
	EventCounts   map[string]int    `json:"event_counts"`
}

// TripVehicleGet builds GET /api/trips/{id}/vehicle's response for tripID,
// or ErrNotFound. It composes VehicleTripSummary (totals/event_counts/
// overlapping ids) with VehicleTripsByIDs (recorder_trips) and
// VehicleEventsForTrip (trip-relative events) rather than re-deriving any
// of that rollup logic.
func (d *DB) TripVehicleGet(ctx context.Context, tripID int64) (TripVehicleResponse, error) {
	sum, err := d.VehicleTripSummary(ctx, tripID)
	if err != nil {
		return TripVehicleResponse{}, err
	}

	recorderTrips, err := d.VehicleTripsByIDs(ctx, sum.OverlappingVehicleTripIDs)
	if err != nil {
		return TripVehicleResponse{}, err
	}
	events, err := d.VehicleEventsForTrip(ctx, tripID)
	if err != nil {
		return TripVehicleResponse{}, err
	}

	totals := TripVehicleTotals{
		BattMinV: sum.BattMinV, BattMaxV: sum.BattMaxV, CoolantMaxC: sum.CoolantMaxC,
		DTCs: sum.DTCs, MIL: sum.MIL,
	}
	if sum.DistanceM != 0 {
		v := sum.DistanceM
		totals.DistanceM = &v
	}
	if sum.FuelUsedL != 0 {
		v := sum.FuelUsedL
		totals.FuelL = &v
	}
	if sum.MaxSpeedKph != 0 {
		v := sum.MaxSpeedKph
		totals.MaxSpeedKph = &v
	}
	if totals.DTCs == nil {
		totals.DTCs = []string{}
	}

	if events == nil {
		events = []VehicleEventRow{}
	}
	if recorderTrips == nil {
		recorderTrips = []VehicleTripRow{}
	}

	return TripVehicleResponse{
		RecorderTrips: recorderTrips,
		Totals:        totals,
		Events:        events,
		EventCounts:   sum.EventCounts,
	}, nil
}

// VehicleDTC is one distinct DTC code seen across every vehicle_trips row,
// for GET /api/vehicle/dtcs.
type VehicleDTC struct {
	Code      string `json:"code"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	TripCount int    `json:"trip_count"`
}

// VehicleDTCsList returns every distinct DTC code seen across
// vehicle_trips.dtcs (json_each over the stored JSON array), with the
// first/last trip start_time it appeared in and how many distinct trips
// reported it, ordered by code.
func (d *DB) VehicleDTCsList(ctx context.Context) ([]VehicleDTC, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT dtc.value AS code, MIN(vt.start_time), MAX(vt.start_time), COUNT(DISTINCT vt.id)
		FROM vehicle_trips vt, json_each(vt.dtcs) dtc
		WHERE vt.dtcs IS NOT NULL
		GROUP BY dtc.value
		ORDER BY dtc.value`)
	if err != nil {
		return nil, fmt.Errorf("db: vehicle dtcs list: %w", err)
	}
	defer rows.Close()

	out := []VehicleDTC{}
	for rows.Next() {
		var v VehicleDTC
		var first, last sql.NullString
		if err := rows.Scan(&v.Code, &first, &last, &v.TripCount); err != nil {
			return nil, fmt.Errorf("db: vehicle dtcs list: scan: %w", err)
		}
		v.FirstSeen, v.LastSeen = first.String, last.String
		out = append(out, v)
	}
	return out, rows.Err()
}

// VehicleTrend is one vehicle_trips row's battery/coolant extremes, for
// GET /api/vehicle/trends's chart.
type VehicleTrend struct {
	ID          int64    `json:"id"`
	StartTime   string   `json:"start_time"`
	BattMinV    *float64 `json:"batt_min_v"`
	BattMaxV    *float64 `json:"batt_max_v"`
	CoolantMaxC *float64 `json:"coolant_max_c"`
}

// VehicleTrendsList returns vehicle_trips rows with a start_time within
// the last `days` days, ascending by start_time.
func (d *DB) VehicleTrendsList(ctx context.Context, days int) ([]VehicleTrend, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := d.read.QueryContext(ctx, `
		SELECT id, start_time, batt_min_v, batt_max_v, coolant_max_c
		FROM vehicle_trips
		WHERE start_time IS NOT NULL AND start_time >= ?
		ORDER BY start_time ASC`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("db: vehicle trends list: %w", err)
	}
	defer rows.Close()

	out := []VehicleTrend{}
	for rows.Next() {
		var v VehicleTrend
		if err := rows.Scan(&v.ID, &v.StartTime, &v.BattMinV, &v.BattMaxV, &v.CoolantMaxC); err != nil {
			return nil, fmt.Errorf("db: vehicle trends list: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
