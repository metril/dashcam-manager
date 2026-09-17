package db

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

// TestMigrateV9ToV10AddsOBDTelemetry hand-builds a pre-v10 database (v9
// shape: sources/segments without the v10 OBD columns, and none of the
// v10 OBD tables) stamped schema_version=9, then runs InitSchema and
// checks the v9->v10 migration brings it up to the full v10 shape:
// segments.has_obd, sources.vehicle_trips_cursor/vehicle_events_cursor,
// and the new segment_obd/segment_obd_points/vehicle_trips/vehicle_events
// tables all exist and are usable.
func TestMigrateV9ToV10AddsOBDTelemetry(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "dashcam.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

	// v9-shape sources/segments/shares: sources already has base_url (so
	// migrateSourcesToV8DashcamKind no-ops) but not the v10 cursor
	// columns; segments has has_gps but not has_obd.
	setup := []string{
		settingsTableStmt,
		`CREATE TABLE shares (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			server TEXT NOT NULL,
			share TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'cifs'
		)`,
		`CREATE TABLE sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL CHECK (kind IN ('ssd','share','dashcam')),
			share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
			root_path TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			last_scan_at TEXT,
			status TEXT NOT NULL DEFAULT 'idle',
			base_url TEXT,
			target_share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
			pull_cursor TEXT,
			last_pull_at TEXT,
			UNIQUE(root_path)
		)`,
		`CREATE TABLE trips (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			trip_key TEXT NOT NULL,
			started_at TEXT NOT NULL,
			UNIQUE(source_id, trip_key)
		)`,
		`CREATE TABLE segments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			trip_id INTEGER REFERENCES trips(id) ON DELETE SET NULL,
			camera TEXT NOT NULL CHECK (camera IN ('front','rear')),
			rel_path TEXT NOT NULL,
			seq INTEGER NOT NULL DEFAULT 0,
			start_time TEXT NOT NULL,
			end_time TEXT,
			has_detections INTEGER NOT NULL DEFAULT 0,
			has_gps INTEGER NOT NULL DEFAULT 0,
			unreadable_reason TEXT,
			unreadable_at TEXT,
			UNIQUE(source_id, rel_path)
		)`,
	}
	for _, stmt := range setup {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/ssd', 'test')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	res, err = d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, seq, start_time) VALUES (?, 'front', 'front/seg.mp4', 0, '2026-01-01T00:00:00Z')`, sourceID)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	segID, _ := res.LastInsertId()

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO settings (key, value_json) VALUES ('schema_version', '9')`); err != nil {
		t.Fatalf("stamp schema_version: %v", err)
	}

	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v9->v10 migrate): %v", err)
	}

	var hasOBD int
	if err := d.Read().QueryRowContext(ctx, `SELECT has_obd FROM segments WHERE id = ?`, segID).Scan(&hasOBD); err != nil {
		t.Fatalf("query segments.has_obd: %v", err)
	}
	if hasOBD != 0 {
		t.Errorf("has_obd = %d, want 0 (default for pre-existing row)", hasOBD)
	}

	var tripsCursor, eventsCursor any
	if err := d.Read().QueryRowContext(ctx,
		`SELECT vehicle_trips_cursor, vehicle_events_cursor FROM sources WHERE id = ?`, sourceID,
	).Scan(&tripsCursor, &eventsCursor); err != nil {
		t.Fatalf("query sources cursors: %v", err)
	}
	if tripsCursor != nil || eventsCursor != nil {
		t.Errorf("vehicle_trips_cursor/vehicle_events_cursor = %v/%v, want nil/nil", tripsCursor, eventsCursor)
	}

	// New tables must exist and be usable.
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segment_obd (segment_id, max_speed_kph, avg_rpm, distance_m, fuel_used_l, point_count) VALUES (?, 40, 1200, 30, 0.02, 1)`, segID,
	); err != nil {
		t.Fatalf("insert into segment_obd: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segment_obd_points (segment_id, t, speed_kph) VALUES (?, '2026-01-01T00:00:00Z', 40)`, segID,
	); err != nil {
		t.Fatalf("insert into segment_obd_points: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO vehicle_trips (dashcam_source_id, remote_trip_id) VALUES (?, 'trip-1')`, sourceID,
	); err != nil {
		t.Fatalf("insert into vehicle_trips: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO vehicle_events (dashcam_source_id, remote_event_id, event_type) VALUES (?, 'event-1', 'hard_brake')`, sourceID,
	); err != nil {
		t.Fatalf("insert into vehicle_events: %v", err)
	}

	var stored string
	if err := d.Read().QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = 'schema_version'`).Scan(&stored); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if stored != strconv.Itoa(SchemaVersion) {
		t.Fatalf("schema_version after migration = %q, want %d", stored, SchemaVersion)
	}

	// Idempotent: calling InitSchema again must be a no-op.
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("second InitSchema after migration: %v", err)
	}
}
