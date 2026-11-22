package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
)

// recorderTableRows reads every row of a recorder table (SELECT *, id ASC)
// as the same JSON object the recorder's ingest API serves: the API does
// `SELECT *` + dict(zip(cols,row)), with trips.dtcs json-decoded to a list.
// Columns an older recorder DB lacks are simply absent, which decodes as
// nil exactly like an older recorder's API response. A missing table yields
// no rows.
func recorderTableRows(ctx context.Context, rdb *sql.DB, table string) ([]json.RawMessage, error) {
	var n int
	if err := rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := rdb.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		obj := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if c == "dtcs" {
				var list []any
				if s, ok := v.(string); ok && s != "" {
					if json.Unmarshal([]byte(s), &list) != nil {
						list = nil
					}
				}
				if list == nil {
					list = []any{}
				}
				v = list
			}
			obj[c] = v
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// importVehicleFromSSD imports OBD trips/events from the recorder's own
// SQLite DB at <rootPath>/dashcam.db (opened read-only/immutable) into
// vehicle_trips/vehicle_events, via the exact wire types and converters the
// network pull uses (dashcamVehicleTripToDB / dashcamVehicleEventToDB) and
// the same (dashcam_source_id, remote_*_id) keyed upserts, so the two paths
// never duplicate each other.
//
// vehicle_* rows require a kind='dashcam' source: the rows attach to the
// single dashcam source when exactly one exists; with none or several the
// import is skipped (logged) rather than guessing or inventing a fake source.
// Best-effort: never returns an error to fail a scan.
func importVehicleFromSSD(ctx context.Context, store *db.DB, ssdSourceID int64, rootPath string) (trips, events int) {
	legacyPath := filepath.Join(rootPath, "dashcam.db")
	if _, err := os.Stat(legacyPath); err != nil {
		return 0, 0
	}
	dashcams, err := store.DashcamList(ctx)
	if err != nil {
		log.Printf("[scan] ssd vehicle import: list dashcam sources: %v", err)
		return 0, 0
	}
	if len(dashcams) != 1 {
		log.Printf("[scan] ssd vehicle import: skipped, need exactly one dashcam source to attach to (have %d)", len(dashcams))
		return 0, 0
	}
	dashcamID := dashcams[0].ID

	rdb, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&immutable=1", legacyPath))
	if err != nil {
		log.Printf("[scan] ssd vehicle import: open %s: %v", legacyPath, err)
		return 0, 0
	}
	defer rdb.Close()

	tripRows, err := recorderTableRows(ctx, rdb, "trips")
	if err != nil {
		log.Printf("[scan] ssd vehicle import: read trips: %v", err)
		return 0, 0
	}
	var dbTrips []db.VehicleTrip
	for _, raw := range tripRows {
		var t dashcam.VehicleTrip
		if err := json.Unmarshal(raw, &t); err != nil {
			log.Printf("[scan] ssd vehicle import: skip trip: %v", err)
			continue
		}
		dbTrips = append(dbTrips, dashcamVehicleTripToDB(t))
	}
	if len(dbTrips) > 0 {
		if _, err := store.VehicleTripsUpsertBulk(ctx, dashcamID, dbTrips); err != nil {
			log.Printf("[scan] ssd vehicle import: upsert trips: %v", err)
			return 0, 0
		}
		trips = len(dbTrips)
	}

	eventRows, err := recorderTableRows(ctx, rdb, "vehicle_events")
	if err != nil {
		log.Printf("[scan] ssd vehicle import: read events: %v", err)
		return trips, 0
	}
	var dbEvents []db.VehicleEvent
	for _, raw := range eventRows {
		var e dashcam.VehicleEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			log.Printf("[scan] ssd vehicle import: skip event: %v", err)
			continue
		}
		dbEvents = append(dbEvents, dashcamVehicleEventToDB(e))
	}
	if len(dbEvents) > 0 {
		if _, err := store.VehicleEventsUpsertBulk(ctx, dashcamID, dbEvents); err != nil {
			log.Printf("[scan] ssd vehicle import: upsert events: %v", err)
			return trips, 0
		}
		events = len(dbEvents)
	}
	if _, err := store.VehicleEventsLink(ctx, dashcamID, ssdSourceID); err != nil {
		log.Printf("[scan] ssd vehicle import: link events: %v", err)
	}
	log.Printf("[scan] ssd vehicle import: imported %d trips, %d events (dashcam source %d)", trips, events, dashcamID)
	return trips, events
}
