package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// SchemaVersion is recorded in settings["schema_version"] the first time
// InitSchema runs against a fresh database.
const SchemaVersion = 1

// schemaStatements is the full DDL for dashcam-manager, matching the "DB
// schema" section of docs/superpowers/specs/2026-09-08-dashcam-manager-design.md.
// Every statement is idempotent (IF NOT EXISTS) so InitSchema can run on
// every boot.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS shares (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		server TEXT NOT NULL,
		share TEXT NOT NULL,
		subpath TEXT NOT NULL DEFAULT '',
		username TEXT NOT NULL DEFAULT '',
		password_enc BLOB,
		domain TEXT NOT NULL DEFAULT '',
		mount_point TEXT NOT NULL DEFAULT '',
		options TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'unknown',
		last_mounted_at TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS sources (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL CHECK (kind IN ('ssd','share')),
		share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
		root_path TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		last_scan_at TEXT,
		status TEXT NOT NULL DEFAULT 'idle'
	)`,

	`CREATE TABLE IF NOT EXISTS trips (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		trip_key TEXT NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT,
		duration_s REAL,
		front_dir TEXT,
		rear_dir TEXT,
		pair_offset_s REAL,
		segment_count INTEGER NOT NULL DEFAULT 0,
		bytes INTEGER NOT NULL DEFAULT 0,
		distance_m REAL,
		max_speed_mps REAL,
		start_lat REAL,
		start_lon REAL,
		end_lat REAL,
		end_lon REAL,
		cover_thumb TEXT,
		UNIQUE(source_id, trip_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_trips_started_at ON trips(started_at)`,

	`CREATE TABLE IF NOT EXISTS segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		trip_id INTEGER REFERENCES trips(id) ON DELETE SET NULL,
		camera TEXT NOT NULL CHECK (camera IN ('front','rear')),
		rel_path TEXT NOT NULL,
		seq INTEGER NOT NULL DEFAULT 0,
		start_time TEXT NOT NULL,
		end_time TEXT,
		duration_s REAL,
		bytes INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT,
		width INTEGER,
		height INTEGER,
		has_detections INTEGER NOT NULL DEFAULT 0,
		has_gps INTEGER NOT NULL DEFAULT 0,
		thumb_status TEXT NOT NULL DEFAULT 'pending',
		indexed_at TEXT,
		UNIQUE(source_id, rel_path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_start_time ON segments(start_time)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_trip_camera_seq ON segments(trip_id, camera, seq)`,

	`CREATE TABLE IF NOT EXISTS segment_gps (
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		t TEXT NOT NULL,
		lat REAL NOT NULL,
		lon REAL NOT NULL,
		speed_mps REAL,
		heading REAL,
		PRIMARY KEY (segment_id, t)
	)`,

	`CREATE TABLE IF NOT EXISTS segment_summary (
		segment_id INTEGER PRIMARY KEY REFERENCES segments(id) ON DELETE CASCADE,
		n_car INTEGER NOT NULL DEFAULT 0,
		n_truck INTEGER NOT NULL DEFAULT 0,
		n_bus INTEGER NOT NULL DEFAULT 0,
		n_person INTEGER NOT NULL DEFAULT 0,
		n_bicycle INTEGER NOT NULL DEFAULT 0,
		n_motorcycle INTEGER NOT NULL DEFAULT 0,
		n_plates INTEGER NOT NULL DEFAULT 0,
		top_classes TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS detections (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		pts_offset REAL NOT NULL,
		wall_time TEXT,
		frame_index INTEGER,
		lat REAL,
		lon REAL,
		speed_mps REAL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_detections_segment_pts ON detections(segment_id, pts_offset)`,

	`CREATE TABLE IF NOT EXISTS det_objects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		det_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE,
		class TEXT NOT NULL,
		confidence REAL,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_det_objects_class ON det_objects(class)`,

	`CREATE TABLE IF NOT EXISTS det_plates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		det_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE,
		text TEXT NOT NULL,
		confidence REAL,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_det_plates_text ON det_plates(text)`,

	`CREATE TABLE IF NOT EXISTS det_vehicles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		det_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE,
		type TEXT,
		make TEXT,
		confidence REAL,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL
	)`,

	// Gzipped per-segment raw detection JSON, kept for re-indexing /
	// re-normalizing without re-parsing sidecars; class/plate/make rows
	// above are the exploded, queryable projection.
	`CREATE TABLE IF NOT EXISTS detections_raw (
		segment_id INTEGER PRIMARY KEY REFERENCES segments(id) ON DELETE CASCADE,
		gz BLOB NOT NULL
	)`,

	`CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(
		kind,
		text,
		seg_id UNINDEXED,
		det_id UNINDEXED,
		tokenize='trigram'
	)`,

	`CREATE TABLE IF NOT EXISTS jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		type TEXT NOT NULL,
		state TEXT NOT NULL DEFAULT 'queued',
		priority INTEGER NOT NULL DEFAULT 0,
		params_json TEXT,
		progress REAL NOT NULL DEFAULT 0,
		message TEXT,
		result_json TEXT,
		error TEXT,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		started_at TEXT,
		finished_at TEXT,
		cancel_requested INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS exports (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
		name TEXT NOT NULL,
		path TEXT,
		share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
		bytes INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		expires_at TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value_json TEXT NOT NULL
	)`,
}

// InitSchema creates every table, index and the FTS5 search table if they
// don't already exist, and seeds settings["schema_version"] on first run.
// Safe to call on every boot.
func (d *DB) InitSchema(ctx context.Context) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range schemaStatements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("db: init schema: %w\n%s", err, stmt)
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value_json) VALUES ('schema_version', ?)
			 ON CONFLICT(key) DO NOTHING`,
			strconv.Itoa(SchemaVersion),
		)
		return err
	})
}
