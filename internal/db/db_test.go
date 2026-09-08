package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "dashcam.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return d
}

func TestInitSchemaIdempotent(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	// Calling InitSchema again must not error (all DDL is IF NOT EXISTS).
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("second InitSchema: %v", err)
	}

	var version string
	err := d.Read().QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != "1" {
		t.Fatalf("schema_version = %q, want 1", version)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if err := d.SettingsSet(ctx, "workers", json.RawMessage(`4`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	got, err := d.SettingsGet(ctx, "workers")
	if err != nil {
		t.Fatalf("SettingsGet: %v", err)
	}
	if string(got) != "4" {
		t.Fatalf("SettingsGet = %q, want 4", got)
	}

	// Overwrite.
	if err := d.SettingsSet(ctx, "workers", json.RawMessage(`8`)); err != nil {
		t.Fatalf("SettingsSet overwrite: %v", err)
	}
	got, err = d.SettingsGet(ctx, "workers")
	if err != nil {
		t.Fatalf("SettingsGet after overwrite: %v", err)
	}
	if string(got) != "8" {
		t.Fatalf("SettingsGet after overwrite = %q, want 8", got)
	}

	// Missing key.
	if _, err := d.SettingsGet(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SettingsGet missing key: err = %v, want ErrNotFound", err)
	}

	// SettingsSetMany + SettingsGetAll (PUT /api/settings merge semantics).
	if err := d.SettingsSetMany(ctx, map[string]json.RawMessage{
		"ssd_path":   json.RawMessage(`"/mnt/dashcam"`),
		"qsv_device": json.RawMessage(`"/dev/dri/renderD128"`),
	}); err != nil {
		t.Fatalf("SettingsSetMany: %v", err)
	}
	all, err := d.SettingsGetAll(ctx)
	if err != nil {
		t.Fatalf("SettingsGetAll: %v", err)
	}
	if string(all["workers"]) != "8" {
		t.Fatalf("SettingsGetAll[workers] = %q, want 8 (merge must not drop existing keys)", all["workers"])
	}
	if string(all["ssd_path"]) != `"/mnt/dashcam"` {
		t.Fatalf("SettingsGetAll[ssd_path] = %q", all["ssd_path"])
	}

	// SettingsSeedDefaults must not overwrite an existing value.
	if err := d.SettingsSeedDefaults(ctx, map[string]json.RawMessage{
		"workers": json.RawMessage(`99`),
		"new_key": json.RawMessage(`"seeded"`),
	}); err != nil {
		t.Fatalf("SettingsSeedDefaults: %v", err)
	}
	got, _ = d.SettingsGet(ctx, "workers")
	if string(got) != "8" {
		t.Fatalf("SeedDefaults overwrote existing key: workers = %q, want 8", got)
	}
	got, err = d.SettingsGet(ctx, "new_key")
	if err != nil || string(got) != `"seeded"` {
		t.Fatalf("SeedDefaults did not insert new_key: got=%q err=%v", got, err)
	}
}

// insertSearchTerm inserts a segment (so the FK on search_terms.segment_id
// is satisfiable) and one search_terms row for it, returning the segment id.
// The AFTER INSERT trigger on search_terms mirrors the row into search_fts.
func insertSearchTerm(t *testing.T, d *DB, ctx context.Context, kind, text string) int64 {
	t.Helper()
	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	res, err = d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time)
		 VALUES (?, 'front', 'front/2026/01/01/x/segment_00001.mp4', '2026-01-01T00:00:00Z')`,
		sourceID)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	segID, _ := res.LastInsertId()

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO search_terms (segment_id, kind, text) VALUES (?, ?, ?)`,
		segID, kind, text,
	); err != nil {
		t.Fatalf("insert search_terms: %v", err)
	}
	return segID
}

func TestFTS5TrigramSearch(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	segID := insertSearchTerm(t, d, ctx, "plate", "ABC1234")

	var gotSegID int64
	var gotText string
	err := d.Read().QueryRowContext(ctx, `
		SELECT st.segment_id, st.text
		FROM search_fts
		JOIN search_terms st ON st.id = search_fts.rowid
		WHERE search_fts MATCH 'BC12'`,
	).Scan(&gotSegID, &gotText)
	if err != nil {
		t.Fatalf("MATCH query: %v", err)
	}
	if gotSegID != segID {
		t.Fatalf("segment_id = %d, want %d", gotSegID, segID)
	}
	if gotText != "ABC1234" {
		t.Fatalf("text = %q, want ABC1234", gotText)
	}
}

func TestFTS5CascadeDeleteRemovesMatch(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	segID := insertSearchTerm(t, d, ctx, "plate", "ZZZ9999")

	// Sanity: the term is findable before the delete.
	var count int
	if err := d.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM search_fts WHERE search_fts MATCH 'ZZZ99'`).Scan(&count); err != nil {
		t.Fatalf("count before delete: %v", err)
	}
	if count != 1 {
		t.Fatalf("count before delete = %d, want 1", count)
	}

	// Deleting the segment cascades to search_terms (FK ON DELETE CASCADE),
	// which fires the AFTER DELETE trigger that removes the row from
	// search_fts too.
	if _, err := d.Write().ExecContext(ctx, `DELETE FROM segments WHERE id = ?`, segID); err != nil {
		t.Fatalf("delete segment: %v", err)
	}

	if err := d.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM search_terms WHERE segment_id = ?`, segID).Scan(&count); err != nil {
		t.Fatalf("count search_terms after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("search_terms after cascade delete: %d rows remain, want 0", count)
	}

	if err := d.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM search_fts WHERE search_fts MATCH 'ZZZ99'`).Scan(&count); err != nil {
		t.Fatalf("count search_fts after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("search_fts MATCH after cascade delete: %d rows remain, want 0 (FTS not kept in sync)", count)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	// segments.source_id references sources(id); 999 does not exist.
	_, err := d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time)
		 VALUES (999, 'front', 'front/2026/01/01/x/segment_00001.mp4', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected foreign key violation inserting segment with unknown source_id, got nil error")
	}

	// A valid parent row must allow the insert.
	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	_, err = d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time)
		 VALUES (?, 'front', 'front/2026/01/01/x/segment_00001.mp4', '2026-01-01T00:00:00Z')`,
		sourceID)
	if err != nil {
		t.Fatalf("insert segment with valid source_id: %v", err)
	}

	// Deleting the parent source should cascade-delete the segment.
	if _, err := d.Write().ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, sourceID); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	var count int
	if err := d.Read().QueryRowContext(ctx, `SELECT count(*) FROM segments WHERE source_id = ?`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected cascade delete of segments, %d remain", count)
	}
}

func TestSourcesShareCascadeDelete(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('nas', '10.0.0.5', 'dashcam')`)
	if err != nil {
		t.Fatalf("insert share: %v", err)
	}
	shareID, _ := res.LastInsertId()

	res, err = d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, share_id, root_path) VALUES ('share', ?, '/mnt/shares/dashcam')`,
		shareID)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	if _, err := d.Write().ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, shareID); err != nil {
		t.Fatalf("delete share: %v", err)
	}

	var count int
	if err := d.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM sources WHERE id = ?`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected cascade delete of source when its share is deleted, %d remain", count)
	}
}

func TestSourcesRootPathUnique(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	_, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)
	if err == nil {
		t.Fatal("expected UNIQUE violation inserting duplicate root_path, got nil error")
	}
}

func TestOpenRejectsInMemory(t *testing.T) {
	if _, err := Open(":memory:"); err == nil {
		t.Fatal("expected Open(\":memory:\") to fail, got nil error")
	}
}

func TestInitSchemaRejectsNewerVersion(t *testing.T) {
	d := openTest(t) // already at SchemaVersion
	ctx := context.Background()

	if err := d.SettingsSet(ctx, "schema_version", json.RawMessage(`999`)); err != nil {
		t.Fatalf("bump schema_version: %v", err)
	}

	err := d.InitSchema(ctx)
	if err == nil {
		t.Fatal("expected InitSchema to fail against a newer schema_version, got nil error")
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	wantErr := errors.New("boom")
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value_json) VALUES ('rollback_test', '1')`); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Tx returned %v, want %v", err, wantErr)
	}

	if _, err := d.SettingsGet(ctx, "rollback_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write inside failed Tx was not rolled back: err = %v", err)
	}
}
