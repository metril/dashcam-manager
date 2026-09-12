package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

// TestMigrateV7ToV8PreservesRowsAndFKs hand-builds a pre-v8 database (the
// old sources shape: no 'dashcam' in the kind CHECK, none of the new
// columns) with sources/trips/segments rows already in place, stamps it
// schema_version=7, then runs InitSchema and checks: existing rows and
// their FK relationships survive the sources table rebuild untouched, the
// widened CHECK now accepts kind='dashcam', dashcam_pulls exists, and
// foreign key enforcement is back on afterward.
func TestMigrateV7ToV8PreservesRowsAndFKs(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "dashcam.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

	// Hand-build the pre-v8 shape. shares/trips/segments are copied
	// verbatim from schemaStatements (unaffected by v8); sources uses the
	// old (pre-widened) CHECK and column set.
	setup := []string{
		settingsTableStmt,
		`CREATE TABLE shares (
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
		`CREATE TABLE sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL CHECK (kind IN ('ssd','share')),
			share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
			root_path TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			last_scan_at TEXT,
			status TEXT NOT NULL DEFAULT 'idle',
			UNIQUE(root_path)
		)`,
		`CREATE TABLE trips (
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
		// segments' full DDL, matching schemaStatements exactly (unaffected
		// by v8) — schemaStatements' CREATE TABLE IF NOT EXISTS will skip
		// it since it already exists, but the CREATE INDEX statements that
		// follow it (idx_segments_trip_camera_seq etc.) reference columns
		// (seq, ...) that a trimmed-down stand-in table here wouldn't have.
		`CREATE TABLE segments (
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
			mtime INTEGER,
			sha256 TEXT,
			width INTEGER,
			height INTEGER,
			has_detections INTEGER NOT NULL DEFAULT 0,
			has_gps INTEGER NOT NULL DEFAULT 0,
			thumb_status TEXT NOT NULL DEFAULT 'pending',
			indexed_at TEXT,
			archived_at TEXT,
			unreadable_reason TEXT,
			unreadable_at TEXT,
			tracks_built_at TEXT,
			tracks_version INTEGER,
			UNIQUE(source_id, rel_path)
		)`,
	}
	for _, stmt := range setup {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	res, err = d.Write().ExecContext(ctx,
		`INSERT INTO trips (source_id, trip_key, started_at) VALUES (?, 'trip1', '2026-01-01T00:00:00Z')`, sourceID)
	if err != nil {
		t.Fatalf("insert trip: %v", err)
	}
	tripID, _ := res.LastInsertId()

	res, err = d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, trip_id, camera, rel_path, start_time)
		 VALUES (?, ?, 'front', 'front/2026/01/01/x/segment_00001.mp4', '2026-01-01T00:00:00Z')`,
		sourceID, tripID)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	segID, _ := res.LastInsertId()

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO settings (key, value_json) VALUES ('schema_version', '7')`); err != nil {
		t.Fatalf("stamp schema_version: %v", err)
	}

	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v7->v8 migrate): %v", err)
	}

	// The source row (same id) must survive, original columns intact, new
	// columns present but NULL.
	var kind, rootPath, label string
	var baseURL, pullCursor, lastPullAt sql.NullString
	var targetShareID sql.NullInt64
	if err := d.Read().QueryRowContext(ctx,
		`SELECT kind, root_path, label, base_url, target_share_id, pull_cursor, last_pull_at
		 FROM sources WHERE id = ?`, sourceID,
	).Scan(&kind, &rootPath, &label, &baseURL, &targetShareID, &pullCursor, &lastPullAt); err != nil {
		t.Fatalf("query migrated source: %v", err)
	}
	if kind != "ssd" || rootPath != "/mnt/dashcam" || label != "SSD" {
		t.Fatalf("migrated source = (%q,%q,%q), want (ssd,/mnt/dashcam,SSD)", kind, rootPath, label)
	}
	if baseURL.Valid || targetShareID.Valid || pullCursor.Valid || lastPullAt.Valid {
		t.Fatalf("expected the new dashcam columns to be NULL on a migrated non-dashcam source")
	}

	// FK-linked rows must still be there under the same ids.
	var gotTripID, gotSegID int64
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM trips WHERE id = ?`, tripID).Scan(&gotTripID); err != nil {
		t.Fatalf("trip did not survive migration: %v", err)
	}
	if err := d.Read().QueryRowContext(ctx, `SELECT id FROM segments WHERE id = ?`, segID).Scan(&gotSegID); err != nil {
		t.Fatalf("segment did not survive migration: %v", err)
	}

	// Foreign key enforcement must be back on.
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO trips (source_id, trip_key, started_at) VALUES (999999, 'x', '2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("expected a foreign key violation inserting a trip with a bogus source_id after migration")
	}

	// The widened CHECK now accepts kind='dashcam'.
	shareRes, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('nas', '10.0.0.5', 'dashcam')`)
	if err != nil {
		t.Fatalf("insert share: %v", err)
	}
	shareID, _ := shareRes.LastInsertId()
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label, base_url, target_share_id) VALUES ('dashcam', '', 'Cam', 'http://10.0.0.9', ?)`,
		shareID); err != nil {
		t.Fatalf("insert dashcam source after migration: %v", err)
	}

	// dashcam_pulls exists.
	var cnt int
	if err := d.Read().QueryRowContext(ctx, `SELECT count(*) FROM dashcam_pulls`).Scan(&cnt); err != nil {
		t.Fatalf("dashcam_pulls table missing: %v", err)
	}

	// schema_version bumped to SchemaVersion (the fixture starts at v7, and
	// InitSchema always migrates all the way up in one pass — this test
	// predates v9's shares.kind, hence the current SchemaVersion rather
	// than a literal 8).
	var stored string
	if err := d.Read().QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = 'schema_version'`).Scan(&stored); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if stored != strconv.Itoa(SchemaVersion) {
		t.Fatalf("schema_version after migration = %q, want %d", stored, SchemaVersion)
	}

	// Calling InitSchema again must be a no-op (idempotent), not attempt
	// the rebuild a second time.
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("second InitSchema after migration: %v", err)
	}
}

// TestDashcamPullsCascadeDeleteOnSourceDelete covers
// dashcam_pulls.dashcam_source_id's ON DELETE CASCADE.
func TestDashcamPullsCascadeDeleteOnSourceDelete(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	if _, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID,
		RemoteRelPath:   "clips/a.mp4",
		ShareID:         shareID,
		DestRelPath:     "front/a.mp4",
		Bytes:           100,
		PulledAt:        "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("PullUpsert: %v", err)
	}

	if err := d.DashcamDelete(ctx, dashcamID); err != nil {
		t.Fatalf("DashcamDelete: %v", err)
	}

	var cnt int
	if err := d.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM dashcam_pulls WHERE dashcam_source_id = ?`, dashcamID).Scan(&cnt); err != nil {
		t.Fatalf("count dashcam_pulls: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected cascade delete of dashcam_pulls, %d remain", cnt)
	}
}

// mustInsertShare inserts a minimal share row and returns its id, for
// tests that just need a valid shares(id) to reference.
func mustInsertShare(t *testing.T, d *DB, ctx context.Context) int64 {
	t.Helper()
	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('nas', '10.0.0.5', 'dashcam')`)
	if err != nil {
		t.Fatalf("insert share: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}
