package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return d
}

// TestMigrateV11ToV12 downgrades a fresh database to the v11 shape (no
// purpose column, no oidc_pending, an invalid stored ssd_uuid) and checks
// InitSchema brings it back up.
func TestMigrateV11ToV12(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`ALTER TABLE auth_login_states DROP COLUMN purpose`,
		`DROP TABLE oidc_pending`,
		`INSERT INTO settings (key, value_json) VALUES ('ssd_uuid', '"../etc"')`,
		`UPDATE settings SET value_json = '11' WHERE key = 'schema_version'`,
	} {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v11->v12): %v", err)
	}

	if err := d.AuthLoginStatePut(ctx, AuthLoginState{StateHash: "h", Nonce: "n", PKCEVerifier: "v", NextPath: "/", ExpiresAt: time.Now().Add(time.Minute), Purpose: "oidc_test"}); err != nil {
		t.Fatalf("put login state: %v", err)
	}
	st, ok, err := d.AuthLoginStateTake(ctx, "h", time.Now())
	if err != nil || !ok || st.Purpose != "oidc_test" {
		t.Fatalf("take = %+v, %v, %v", st, ok, err)
	}
	if err := d.OIDCPendingPut(ctx, "{}", time.Now(), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("oidc_pending unusable: %v", err)
	}
	if _, err := d.SettingsGet(ctx, "ssd_uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("invalid ssd_uuid should be dropped, err = %v", err)
	}
	raw, err := d.SettingsGet(ctx, "schema_version")
	if err != nil || string(raw) != "12" {
		t.Errorf("schema_version = %s, %v", raw, err)
	}
}

func TestMigrateV12KeepsValidSSDUUID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO settings (key, value_json) VALUES ('ssd_uuid', '"F9B1-23CB"')`,
		`UPDATE settings SET value_json = '11' WHERE key = 'schema_version'`,
		`ALTER TABLE auth_login_states DROP COLUMN purpose`,
	} {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := d.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if raw, err := d.SettingsGet(ctx, "ssd_uuid"); err != nil || string(raw) != `"F9B1-23CB"` {
		t.Errorf("ssd_uuid = %s, %v", raw, err)
	}
}

func TestOIDCPendingRoundTrip(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	if err := d.OIDCPendingPut(ctx, `{"a":1}`, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.OIDCPendingTake(ctx, "s1", now); ok {
		t.Error("take before bind must find nothing")
	}
	cfg, ok, err := d.OIDCPendingBind(ctx, "s1", now)
	if err != nil || !ok || cfg != `{"a":1}` {
		t.Fatalf("bind = %q, %v, %v", cfg, ok, err)
	}
	if _, ok, _ := d.OIDCPendingTake(ctx, "other", now); ok {
		t.Error("take with wrong state must find nothing")
	}
	if cfg, ok, _ := d.OIDCPendingTake(ctx, "s1", now); !ok || cfg != `{"a":1}` {
		t.Errorf("take = %q, %v", cfg, ok)
	}
	if _, ok, _ := d.OIDCPendingTake(ctx, "s1", now); ok {
		t.Error("second take must find nothing")
	}

	// Expired entries are rejected at bind and take.
	_ = d.OIDCPendingPut(ctx, "{}", now, now.Add(time.Minute))
	if _, ok, _ := d.OIDCPendingBind(ctx, "s2", now.Add(time.Hour)); ok {
		t.Error("expired pending must not bind")
	}
	_, _, _ = d.OIDCPendingBind(ctx, "s2", now)
	if _, ok, _ := d.OIDCPendingTake(ctx, "s2", now.Add(time.Hour)); ok {
		t.Error("expired pending must not be taken")
	}
	_ = d.OIDCPendingPut(ctx, "{}", now, now.Add(time.Minute))
	if err := d.OIDCPendingClear(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.OIDCPendingBind(ctx, "s3", now); ok {
		t.Error("cleared pending must not bind")
	}
}

func TestSourceSetSSDRoot(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	id, err := d.SourceEnsure(ctx, "ssd", "/mnt/old", "SSD")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SourceSetSSDRoot(ctx, id, "/mnt/new"); err != nil {
		t.Fatal(err)
	}
	if s, err := d.SourceGet(ctx, id); err != nil || s.RootPath != "/mnt/new" {
		t.Fatalf("source = %+v, %v", s, err)
	}
	other, _ := d.SourceEnsure(ctx, "share", "/mnt/shares/a", "A")
	if err := d.SourceSetSSDRoot(ctx, other, "/mnt/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("non-ssd source: err = %v, want ErrNotFound", err)
	}
	if err := d.SourceSetSSDRoot(ctx, id, "/mnt/shares/a"); err == nil {
		t.Error("expected unique-root conflict")
	}
}

func TestSettingsDelete(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	_ = d.SettingsSet(ctx, "a", []byte(`1`))
	_ = d.SettingsSet(ctx, "b", []byte(`2`))
	if err := d.SettingsDelete(ctx, "a", "missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SettingsGet(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a not deleted: %v", err)
	}
	if _, err := d.SettingsGet(ctx, "b"); err != nil {
		t.Errorf("b should remain: %v", err)
	}
}
