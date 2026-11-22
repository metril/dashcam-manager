package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
)

// Real recorder schema (resources/metadata.py in the recorder repo).
const recorderTripsDDL = `
    CREATE TABLE IF NOT EXISTS trips (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        start_time TEXT,
        end_time TEXT,
        start_t REAL,
        end_t REAL,
        distance_m REAL,
        fuel_l REAL,
        fuel_pct_start REAL,
        fuel_pct_end REAL,
        max_speed_kph REAL,
        avg_speed_kph REAL,
        batt_min_v REAL,
        batt_max_v REAL,
        coolant_max_c REAL,
        sample_count INTEGER,
        dtcs TEXT,
        mil INTEGER,
        hard_brakes INTEGER DEFAULT 0,
        hard_accels INTEGER DEFAULT 0,
        speeding INTEGER DEFAULT 0,
        idling INTEGER DEFAULT 0,
        overheat INTEGER DEFAULT 0,
        charging_fault INTEGER DEFAULT 0,
        cold_hard INTEGER DEFAULT 0,
        cornering INTEGER DEFAULT 0,
        stall INTEGER DEFAULT 0,
        odometer_start_km REAL,
        odometer_end_km REAL,
        range_start_km REAL,
        range_end_km REAL,
        odometer_distance_km REAL,
        ambient_min_c REAL,
        ambient_max_c REAL,
        ambient_avg_c REAL,
        tire_fl_min_kpa REAL,
        tire_fl_max_kpa REAL,
        tire_fl_avg_kpa REAL,
        tire_fr_min_kpa REAL,
        tire_fr_max_kpa REAL,
        tire_fr_avg_kpa REAL,
        tire_rl_min_kpa REAL,
        tire_rl_max_kpa REAL,
        tire_rl_avg_kpa REAL,
        tire_rr_min_kpa REAL,
        tire_rr_max_kpa REAL,
        tire_rr_avg_kpa REAL
    );
    CREATE INDEX IF NOT EXISTS idx_trip_start_time ON trips(start_time);

`

const recorderEventsDDL = `
    CREATE TABLE IF NOT EXISTS vehicle_events (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        event_type TEXT NOT NULL,
        wall_time TEXT,
        t REAL,
        duration_sec REAL,
        peak REAL,
        speed_kph REAL,
        rpm REAL,
        segment_path TEXT,
        gps_lat REAL,
        gps_lon REAL,
        trip_id INTEGER,
        road_name TEXT,
        limit_kph REAL,
        wheel TEXT
    );
`

var fullInserts = []string{
	`INSERT INTO trips (id, start_time, end_time, start_t, end_t, distance_m, sample_count, dtcs, mil, hard_brakes, tire_fl_min_kpa, odometer_start_km)
	 VALUES (7, '2026-09-01T08:00:00', '2026-09-01T08:30:00', 1.5, 100.5, 12345.0, 42, '["P0301"]', 1, 2, 230.5, 1000.0)`,
	`INSERT INTO trips (id, start_time, end_time, dtcs) VALUES (8, '2026-09-02T09:00:00', NULL, NULL)`,
	`INSERT INTO vehicle_events (id, event_type, wall_time, t, peak, trip_id, wheel) VALUES (1, 'low_tire', '2026-09-01T08:10:00', 5.0, 180.0, 7, 'FL')`,
	`INSERT INTO vehicle_events (id, event_type, wall_time, speed_kph) VALUES (2, 'hard_brake', '2026-09-02T09:05:00', 88.0)`,
}

func newSSDImportStore(t *testing.T) (*db.DB, int64, int64) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	shareID, _, err := store.ShareCreateWithSource(ctx, db.ShareCreateParams{
		Name: "t", Server: "s", ShareName: "f", Enabled: true, Kind: db.KindLocal,
	}, func(id int64) string { return t.TempDir() }, "target")
	if err != nil {
		t.Fatal(err)
	}
	dcID, err := store.DashcamCreate(ctx, "car", "http://127.0.0.1:1", shareID)
	if err != nil {
		t.Fatal(err)
	}
	ssdID, err := store.SourceCreate(ctx, "ssd", t.TempDir(), "ssd")
	if err != nil {
		t.Fatal(err)
	}
	return store, dcID, ssdID
}

func writeRecorderDB(t *testing.T, root string, stmts ...[]string) {
	t.Helper()
	rdb, err := sql.Open("sqlite", filepath.Join(root, "dashcam.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	for _, list := range stmts {
		for _, s := range list {
			if _, err := rdb.Exec(s); err != nil {
				t.Fatalf("%v: %s", err, s)
			}
		}
	}
}

func count(t *testing.T, store *db.DB, table string) int {
	t.Helper()
	var n int
	if err := store.Read().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestImportVehicleFromSSD_MapsAndIsIdempotent(t *testing.T) {
	store, dcID, ssdID := newSSDImportStore(t)
	root := t.TempDir()
	writeRecorderDB(t, root, []string{recorderTripsDDL, recorderEventsDDL}, fullInserts)
	ctx := context.Background()

	tr, ev := importVehicleFromSSD(ctx, store, ssdID, root)
	if tr != 2 || ev != 2 {
		t.Fatalf("imported %d/%d, want 2/2", tr, ev)
	}
	var odo, tire sql.NullFloat64
	var dtcs string
	var mil, brakes int
	if err := store.Read().QueryRow(`SELECT odometer_start_km, tire_fl_min_kpa, dtcs, mil, hard_brakes FROM vehicle_trips WHERE dashcam_source_id=? AND remote_trip_id='7'`, dcID).
		Scan(&odo, &tire, &dtcs, &mil, &brakes); err != nil {
		t.Fatal(err)
	}
	if odo.Float64 != 1000 || tire.Float64 != 230.5 || dtcs != `["P0301"]` || mil != 1 || brakes != 2 {
		t.Fatalf("bad trip row: %v %v %s %d %d", odo, tire, dtcs, mil, brakes)
	}
	var tripFK sql.NullInt64
	var wheel sql.NullString
	if err := store.Read().QueryRow(`SELECT vehicle_trip_id, wheel FROM vehicle_events WHERE remote_event_id='1'`).Scan(&tripFK, &wheel); err != nil {
		t.Fatal(err)
	}
	if !tripFK.Valid || wheel.String != "FL" {
		t.Fatalf("event not linked to trip / wheel: %v %v", tripFK, wheel)
	}

	// Idempotent.
	importVehicleFromSSD(ctx, store, ssdID, root)
	// Pull-shaped upsert of the same rows: no dupes.
	id7 := int64(7)
	if _, err := store.VehicleTripsUpsert(ctx, dcID, dashcamVehicleTripToDB(dashcam.VehicleTrip{ID: id7, StartTime: "2026-09-01T08:00:00", EndTime: "2026-09-01T08:30:00"})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VehicleEventsUpsert(ctx, dcID, dashcamVehicleEventToDB(dashcam.VehicleEvent{ID: 1, EventType: "low_tire", WallTime: "2026-09-01T08:10:00", TripID: &id7})); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, "vehicle_trips"); n != 2 {
		t.Fatalf("trips = %d, want 2", n)
	}
	if n := count(t, store, "vehicle_events"); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
}

func TestImportVehicleFromSSD_OldSchemaAndNoop(t *testing.T) {
	store, _, ssdID := newSSDImportStore(t)
	root := t.TempDir()
	// Older recorder: no ext columns, no road_name/wheel/limit_kph.
	writeRecorderDB(t, root, []string{
		`CREATE TABLE trips (id INTEGER PRIMARY KEY AUTOINCREMENT, start_time TEXT, end_time TEXT, start_t REAL, end_t REAL, distance_m REAL, sample_count INTEGER, dtcs TEXT, mil INTEGER)`,
		`CREATE TABLE vehicle_events (id INTEGER PRIMARY KEY AUTOINCREMENT, event_type TEXT NOT NULL, wall_time TEXT, t REAL, speed_kph REAL, trip_id INTEGER, peak REAL)`,
		`INSERT INTO trips (id, start_time, end_time, dtcs) VALUES (1, '2026-09-01T08:00:00', '2026-09-01T08:30:00', '[]')`,
		`INSERT INTO trips (id, start_time) VALUES (2, '2026-09-02T09:00:00')`,
		`INSERT INTO vehicle_events (id, event_type, wall_time, trip_id) VALUES (1, 'hard_brake', '2026-09-01T08:10:00', 1)`,
		`INSERT INTO vehicle_events (id, event_type, wall_time) VALUES (2, 'speeding', '2026-09-02T09:05:00')`,
	})
	tr, ev := importVehicleFromSSD(context.Background(), store, ssdID, root)
	if tr != 2 || ev != 2 {
		t.Fatalf("imported %d/%d, want 2/2", tr, ev)
	}
	if tr, ev := importVehicleFromSSD(context.Background(), store, ssdID, t.TempDir()); tr+ev != 0 {
		t.Fatal("expected no-op without dashcam.db")
	}
}

func TestImportVehicleFromSSD_NeedsExactlyOneDashcam(t *testing.T) {
	store, _, ssdID := newSSDImportStore(t)
	ctx := context.Background()
	dcs, _ := store.DashcamList(ctx)
	if _, err := store.DashcamCreate(ctx, "second", "http://127.0.0.1:2", *dcs[0].TargetShareID); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeRecorderDB(t, root, []string{recorderTripsDDL, recorderEventsDDL}, fullInserts)
	if tr, ev := importVehicleFromSSD(ctx, store, ssdID, root); tr+ev != 0 {
		t.Fatal("expected skip with 2 dashcam sources")
	}
}
