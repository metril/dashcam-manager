package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dashcam/internal/auth"
	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// newTestMux builds a full API mux over a fresh temp-file sqlite database,
// with a real (but test-scoped) job queue: 2 workers running until the
// test's context is cancelled at cleanup, so POST /api/sources/scan and the
// jobs endpoints behave as they would in production rather than being
// stubbed out.
func newTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	queue := jobs.New(store)
	b := newBroker()
	queue.OnEvent(b.publishJob)
	registerJobHandlers(queue, store, config.Load())
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, queue, b, ctx, testAuthn(store), testApplier(store, queue))
	return mux
}

func TestHealthEndpoint(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
	if body["version"] == "" {
		t.Errorf("version field is empty")
	}
}

// TestStaticSPAFallback exercises the real registerRoutes -> staticHandler
// wiring against whatever is actually embedded in cmd/api/dist at build
// time. Only cmd/api/dist/.gitkeep is tracked in git (the built index.html
// is gitignored — see spa_test.go for the fs.FS-level tests that don't
// depend on a frontend build having run), so on a fresh checkout without
// `npm run build` this legitimately gets a 503; after a build, 200.
func TestStaticSPAFallback(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/some/client/route", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 200 (built) or 503 (no frontend build yet)", rec.Code)
	}
}

func TestUnknownAPIPathIs404(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestMeEndpointAuthDisabled(t *testing.T) {
	mux := newTestMux(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"auth_disabled_override":false,"auth_enabled":false,"enabled":false,"user":null}` {
		t.Errorf("body = %s", got)
	}
}

// testAuthn/testApplier give registerRoutes the always-present auth and
// settings-apply dependencies without an SSD watcher.
func testAuthn(store *db.DB) *auth.Authenticator {
	return auth.New(store, config.Load().SecretKey, false)
}

func testApplier(store *db.DB, queue *jobs.Queue) *applier {
	return newApplier(store, queue, nil, nil, config.Load().SecretKey)
}

func seedStore(t *testing.T) *db.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func settingString(t *testing.T, store *db.DB, key string) string {
	t.Helper()
	raw, err := store.SettingsGet(context.Background(), key)
	if err != nil {
		t.Fatalf("setting %s: %v", key, err)
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	if s == "" {
		return string(raw)
	}
	return s
}

// TestSeedDefaultSettingsIsInsertIfAbsent: the env only seeds; once a value
// is in the table a different env value (or a UI edit) is never applied.
func TestSeedDefaultSettingsIsInsertIfAbsent(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	cfg := config.Load()
	cfg.Seed.SSDPath = "/mnt/first"
	cfg.Seed.Workers = 3
	if err := seedDefaultSettings(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	if settingString(t, store, "ssd_path") != "/mnt/first" || settingString(t, store, "workers") != "3" {
		t.Fatal("first seed did not insert env values")
	}

	if err := store.SettingsSet(ctx, "workers", []byte("7")); err != nil { // a UI edit
		t.Fatal(err)
	}
	cfg.Seed.SSDPath = "/mnt/second"
	cfg.Seed.Workers = 9
	if err := seedDefaultSettings(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	if settingString(t, store, "ssd_path") != "/mnt/first" || settingString(t, store, "workers") != "7" {
		t.Errorf("second seed overwrote saved values: ssd_path=%s workers=%s", settingString(t, store, "ssd_path"), settingString(t, store, "workers"))
	}
}

func TestSeedEnvOIDCEnablesAndEncryptsSecret(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	cfg := config.Load()
	cfg.SecretKey = "existing-install-key"
	cfg.Seed.PublicURL = "https://dash.example.com"
	cfg.Seed.OIDC = config.OIDCConfig{
		Issuer: "https://idp.example.com", ClientID: "dashcam", ClientSecret: "topsecret",
		Scopes: []string{"openid", "email"}, AllowedEmails: []string{"a@x.com"}, GroupsClaim: "groups",
	}
	if err := seedDefaultSettings(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	if settingString(t, store, "oidc_enabled") != "true" || settingString(t, store, "oidc_issuer") != "https://idp.example.com" {
		t.Fatal("env OIDC must seed as enabled")
	}
	if enc := settingString(t, store, "oidc_client_secret_enc"); enc == "" || strings.Contains(enc, "topsecret") {
		t.Fatalf("client secret must be stored encrypted, got %q", enc)
	}

	// The same key (DASHCAM_SECRET_KEY on an existing install) decrypts it.
	r, err := loadRuntime(ctx, store, "existing-install-key")
	if err != nil {
		t.Fatal(err)
	}
	if !r.OIDCEnabled || r.OIDC.ClientSecret != "topsecret" || r.OIDCSecretErr != "" || r.PublicURL != "https://dash.example.com" {
		t.Errorf("runtime = %+v", r)
	}
	// A different key can't: OIDC stays off and says why.
	r, _ = loadRuntime(ctx, store, "some-other-key")
	if r.OIDCEnabled || r.OIDCSecretErr == "" {
		t.Errorf("wrong key: enabled=%v err=%q, want disabled with an error", r.OIDCEnabled, r.OIDCSecretErr)
	}
}

func TestSeedPartialEnvOIDCIsSkipped(t *testing.T) {
	store := seedStore(t)
	cfg := config.Load()
	cfg.Seed.OIDC = config.OIDCConfig{ClientSecret: "orphan", Scopes: []string{"openid"}, GroupsClaim: "groups"}
	if err := seedDefaultSettings(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	if settingString(t, store, "oidc_enabled") != "false" {
		t.Error("partial env OIDC must not enable sign-in")
	}
	if _, err := store.SettingsGet(context.Background(), "oidc_client_secret_enc"); err == nil {
		t.Error("partial env OIDC must not seed a secret")
	}
}
