package db

import (
	"context"
	"testing"
	"time"
)

func TestAuthSessionLifecycle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	s := AuthSession{TokenHash: "h1", Subject: "sub", Email: "a@x.com", Name: "A", EmailVerified: true,
		GroupsJSON: `["g"]`, IDToken: "tok", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := d.AuthSessionCreate(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok, err := d.AuthSessionGet(ctx, "h1", now)
	if err != nil || !ok {
		t.Fatalf("get = %v, %v, %v", got, ok, err)
	}
	if got.Email != "a@x.com" || !got.EmailVerified || got.GroupsJSON != `["g"]` || got.IDToken != "tok" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if _, ok, _ := d.AuthSessionGet(ctx, "h1", now.Add(2*time.Hour)); ok {
		t.Error("expired session returned")
	}
	if _, ok, _ := d.AuthSessionGet(ctx, "nope", now); ok {
		t.Error("unknown session returned")
	}
	if err := d.AuthSessionDelete(ctx, "h1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := d.AuthSessionGet(ctx, "h1", now); ok {
		t.Error("deleted session returned")
	}
}

func TestAuthLoginStateTakeIsSingleUse(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	st := AuthLoginState{StateHash: "s1", Nonce: "n", PKCEVerifier: "v", NextPath: "/trips/1", ExpiresAt: now.Add(10 * time.Minute)}
	if err := d.AuthLoginStatePut(ctx, st); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := d.AuthLoginStateTake(ctx, "s1", now)
	if err != nil || !ok || got.Nonce != "n" || got.PKCEVerifier != "v" || got.NextPath != "/trips/1" {
		t.Fatalf("first take = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := d.AuthLoginStateTake(ctx, "s1", now); ok {
		t.Error("second take succeeded; want single use")
	}

	// An expired state is consumed but refused.
	_ = d.AuthLoginStatePut(ctx, AuthLoginState{StateHash: "s2", ExpiresAt: now.Add(-time.Minute)})
	if _, ok, _ := d.AuthLoginStateTake(ctx, "s2", now); ok {
		t.Error("expired state returned")
	}
}

func TestAuthPurgeExpired(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	_ = d.AuthSessionCreate(ctx, AuthSession{TokenHash: "old", CreatedAt: now, ExpiresAt: now.Add(-time.Hour)})
	_ = d.AuthSessionCreate(ctx, AuthSession{TokenHash: "new", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	_ = d.AuthLoginStatePut(ctx, AuthLoginState{StateHash: "old", ExpiresAt: now.Add(-time.Hour)})
	_ = d.AuthLoginStatePut(ctx, AuthLoginState{StateHash: "new", ExpiresAt: now.Add(time.Hour)})

	n, err := d.AuthPurgeExpired(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("purge = %d, %v; want 2", n, err)
	}
	if _, ok, _ := d.AuthSessionGet(ctx, "new", now); !ok {
		t.Error("live session purged")
	}
}

func TestMigrateV10ToV11AddsAuthTables(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	for _, q := range []string{`DROP TABLE auth_sessions`, `DROP TABLE auth_login_states`,
		`UPDATE settings SET value_json = '10' WHERE key = 'schema_version'`} {
		if _, err := d.Write().ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v10->v11 migrate): %v", err)
	}
	now := time.Now()
	if err := d.AuthSessionCreate(ctx, AuthSession{TokenHash: "h", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("auth_sessions unusable after migration: %v", err)
	}
	if err := d.AuthLoginStatePut(ctx, AuthLoginState{StateHash: "s", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("auth_login_states unusable after migration: %v", err)
	}
}
