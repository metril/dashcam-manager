package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestMigrateV8ToV9SharesKindFromAllowLocalSetting hand-builds a pre-v9
// database (v8 shares shape: no kind column) with the hidden
// allow_local_share_paths setting on and two share rows — mirroring the
// live database's share id 1 ("dashcam-pull", server "local-placeholder",
// enabled=0) plus one ordinary CIFS-looking share — stamps
// schema_version=8, then runs InitSchema and checks every existing share
// row is flipped to kind='local' (that setting being on is exactly what it
// meant) and the setting itself is deleted afterward.
func TestMigrateV8ToV9SharesKindFromAllowLocalSetting(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "dashcam.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

	// Pre-v9 shares (no kind column) and an already-v8-shape sources table
	// (so migrateSourcesToV8DashcamKind, gated on version < SchemaVersion,
	// sees its base_url marker column and no-ops instead of trying to
	// rebuild a sources table that was never created here).
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
	}
	for _, stmt := range setup {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share, enabled, status) VALUES ('dashcam-pull', 'local-placeholder', '', 0, 'unknown')`)
	if err != nil {
		t.Fatalf("insert share 1: %v", err)
	}
	share1ID, _ := res.LastInsertId()

	res, err = d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('nas', '10.0.0.5', 'footage')`)
	if err != nil {
		t.Fatalf("insert share 2: %v", err)
	}
	share2ID, _ := res.LastInsertId()

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO settings (key, value_json) VALUES ('allow_local_share_paths', 'true')`); err != nil {
		t.Fatalf("seed allow_local_share_paths: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO settings (key, value_json) VALUES ('schema_version', '8')`); err != nil {
		t.Fatalf("stamp schema_version: %v", err)
	}

	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v8->v9 migrate): %v", err)
	}

	for _, id := range []int64{share1ID, share2ID} {
		var kind string
		if err := d.Read().QueryRowContext(ctx, `SELECT kind FROM shares WHERE id = ?`, id).Scan(&kind); err != nil {
			t.Fatalf("query share %d kind: %v", id, err)
		}
		if kind != "local" {
			t.Errorf("share %d kind = %q, want local (allow_local_share_paths was on)", id, kind)
		}
	}

	if _, err := d.SettingsGet(ctx, "allow_local_share_paths"); !errors.Is(err, ErrNotFound) {
		t.Errorf("allow_local_share_paths setting after migration: err=%v, want ErrNotFound (setting deleted)", err)
	}

	var stored string
	if err := d.Read().QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = 'schema_version'`).Scan(&stored); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if stored != "9" {
		t.Fatalf("schema_version after migration = %q, want 9", stored)
	}

	// Idempotent: calling InitSchema again must be a no-op.
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("second InitSchema after migration: %v", err)
	}
}

// TestMigrateV8ToV9SharesKindDefaultsToCIFSWithoutSetting is the negative
// case: without allow_local_share_paths ever set, existing share rows get
// the plain 'cifs' default rather than being flipped, and the (never
// present) setting delete is a harmless no-op.
func TestMigrateV8ToV9SharesKindDefaultsToCIFSWithoutSetting(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "dashcam.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

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
	}
	for _, stmt := range setup {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('nas', '10.0.0.5', 'footage')`)
	if err != nil {
		t.Fatalf("insert share: %v", err)
	}
	shareID, _ := res.LastInsertId()

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO settings (key, value_json) VALUES ('schema_version', '8')`); err != nil {
		t.Fatalf("stamp schema_version: %v", err)
	}

	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v8->v9 migrate): %v", err)
	}

	var kind string
	if err := d.Read().QueryRowContext(ctx, `SELECT kind FROM shares WHERE id = ?`, shareID).Scan(&kind); err != nil {
		t.Fatalf("query share kind: %v", err)
	}
	if kind != "cifs" {
		t.Errorf("kind = %q, want cifs (allow_local_share_paths was never set)", kind)
	}
}
