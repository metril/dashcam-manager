package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dashcam/internal/auth"
	"dashcam/internal/config"
	"dashcam/internal/db"
)

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return m
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

func TestSettingsPutValidatesPerKey(t *testing.T) {
	mux := newTestMux(t)
	cases := []struct {
		body string
		want int
	}{
		{`{"ssd_path": "/etc"}`, 400},
		{`{"ssd_path": "/mnt/../etc"}`, 400},
		{`{"ssd_path": "mnt/dashcam"}`, 400},
		{`{"ssd_path": "/mnt/shares/inner"}`, 400}, // nests inside the default local_root
		{`{"ssd_path": "/mnt/otherssd"}`, 200},
		{`{"local_root": "/home/x"}`, 400},
		{`{"local_root": "/mnt/shares"}`, 200},
		{`{"qsv_device": "/dev/sda"}`, 400},
		{`{"qsv_device": "/dev/dri/renderD129"}`, 200},
		{`{"cuda_device": "gpu0"}`, 400},
		{`{"cuda_device": "1"}`, 200},
		{`{"workers": 0}`, 400},
		{`{"workers": 65}`, 400},
		{`{"workers": 64}`, 200},
		{`{"max_ffmpeg_jobs": 0}`, 400},
		{`{"max_ffmpeg_jobs": 33}`, 400},
		{`{"max_ffmpeg_jobs": 3}`, 200},
		{`{"timezone": "Not/AZone"}`, 400},
		{`{"timezone": "UTC"}`, 200},
		{`{"hw_mode": "turbo"}`, 400},
		{`{"hw_mode": "software"}`, 200},
		{`{"ffmpeg_bin": "FF mpeg!"}`, 400},
		{`{"ffmpeg_bin": "/nonexistent/ffmpeg"}`, 400},
		{`{"ffprobe_bin": "ffprobe"}`, 200},
		{`{"public_url": "ftp://x.example.com"}`, 400},
		{`{"public_url": "https://x.example.com/path"}`, 400},
		{`{"oidc_issuer": "idp"}`, 400},
		{`{"oidc_scopes": ["email"]}`, 400},
		{`{"oidc_allowed_emails": ["not-an-email"]}`, 400},
		{`{"oidc_groups_claim": "bad claim"}`, 400},
		{`{"oidc_enabled": "yes"}`, 400},
	}
	for _, tc := range cases {
		if rec := putSettings(t, mux, tc.body); rec.Code != tc.want {
			t.Errorf("PUT %s = %d, want %d: %s", tc.body, rec.Code, tc.want, rec.Body)
		}
	}
}

func TestSettingsPutNormalizesPublicURL(t *testing.T) {
	mux := newTestMux(t)
	rec := putSettings(t, mux, `{"public_url": " https://Dash.example.com/ "}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var s map[string]json.RawMessage
	_ = json.Unmarshal(unwrapSettings(t, rec.Body.Bytes()), &s)
	if string(s["public_url"]) != `"https://Dash.example.com"` {
		t.Errorf("public_url = %s, want trailing slash and spaces trimmed", s["public_url"])
	}
}

func TestSettingsPutResponseShape(t *testing.T) {
	mux := newTestMux(t)
	rec := putSettings(t, mux, `{"workers": 5, "max_ffmpeg_jobs": 3}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	resp := decodeBody(t, rec)
	var applied, restart []string
	_ = json.Unmarshal(resp["applied"], &applied)
	_ = json.Unmarshal(resp["restart_required"], &restart)
	if strings.Join(applied, ",") != "max_ffmpeg_jobs,workers" {
		t.Errorf("applied = %v", applied)
	}
	if restart == nil || len(restart) != 0 {
		t.Errorf("restart_required = %s, want []", resp["restart_required"])
	}
	if string(resp["apply_errors"]) != "{}" {
		t.Errorf("apply_errors = %s, want {}", resp["apply_errors"])
	}
	if getRT().Workers != 5 || getRT().MaxFFmpeg != 3 {
		t.Errorf("runtime not swapped: %+v", getRT())
	}
	// A repeat of the same values changes nothing.
	resp = decodeBody(t, putSettings(t, mux, `{"workers": 5}`))
	if string(resp["applied"]) != "[]" {
		t.Errorf("no-op PUT applied = %s, want []", resp["applied"])
	}
}

func TestSettingsNeverReturnEncKeysAndReportSecretSet(t *testing.T) {
	t.Setenv("DASHCAM_SECRET_KEY", "test-key")
	mux, store := newTestMuxWithStore(t)
	rec := putSettings(t, mux, `{"oidc_client_secret": "hunter2"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "_enc") || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("PUT response leaks the secret: %s", rec.Body)
	}
	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if strings.Contains(get.Body.String(), "_enc") || strings.Contains(get.Body.String(), "hunter2") {
		t.Errorf("GET leaks the secret: %s", get.Body)
	}
	if string(decodeBody(t, get)["oidc_client_secret_set"]) != "true" {
		t.Errorf("oidc_client_secret_set = %s, want true", decodeBody(t, get)["oidc_client_secret_set"])
	}
	raw, err := store.SettingsGet(context.Background(), "oidc_client_secret_enc")
	if err != nil {
		t.Fatal(err)
	}
	var enc string
	_ = json.Unmarshal(raw, &enc)
	if plain, err := decryptSecret(config.Load().SecretKey, enc); enc == "" || err != nil || plain != "hunter2" {
		t.Errorf("stored secret = %q (%v), want encrypted hunter2", enc, err)
	}
	// Clearing it.
	putSettings(t, mux, `{"oidc_client_secret": ""}`)
	get = httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if string(decodeBody(t, get)["oidc_client_secret_set"]) != "false" {
		t.Errorf("secret not cleared")
	}
}

func TestSettingsOIDCGuardedKeysNeedTest(t *testing.T) {
	mux, store := newTestMuxWithStore(t)

	rec := putSettings(t, mux, `{"oidc_enabled": true}`)
	if rec.Code != 400 || errorCode(rec) != "oidc_test_required" {
		t.Errorf("enabling by PUT = %d %q, want 400 oidc_test_required", rec.Code, errorCode(rec))
	}

	// Pretend a test login enabled it.
	ctx := context.Background()
	if err := store.SettingsSetMany(ctx, map[string]json.RawMessage{
		"oidc_enabled": []byte("true"), "oidc_issuer": []byte(`"https://idp.example.com"`), "oidc_client_id": []byte(`"dashcam"`),
		"public_url": []byte(`"https://dash.example.com"`),
	}); err != nil {
		t.Fatal(err)
	}
	refreshRuntime(ctx, store, config.Load().SecretKey)
	if !getRT().OIDCEnabled {
		t.Fatal("test setup: OIDC should be enabled")
	}
	for _, body := range []string{
		`{"oidc_issuer": "https://other.example.com"}`, `{"oidc_client_id": "x"}`, `{"oidc_client_secret": "s"}`,
		`{"public_url": "https://elsewhere.example.com"}`, `{"public_url": ""}`,
	} {
		rec := putSettings(t, mux, body)
		if rec.Code != 400 || errorCode(rec) != "oidc_test_required" {
			t.Errorf("PUT %s while enabled = %d %q, want 400 oidc_test_required", body, rec.Code, errorCode(rec))
		}
	}
	// Allow-list edits stay plain-PUT editable (no session -> no lockout check).
	if rec := putSettings(t, mux, `{"oidc_allowed_groups": ["family"]}`); rec.Code != 200 {
		t.Errorf("allow-list edit = %d: %s", rec.Code, rec.Body)
	}
	// Turning it off is a plain PUT.
	rec = putSettings(t, mux, `{"oidc_enabled": false}`)
	if rec.Code != 200 || getRT().OIDCEnabled {
		t.Errorf("disable = %d, enabled still %v", rec.Code, getRT().OIDCEnabled)
	}
}

func TestGuardOIDCPatchRefusesLockout(t *testing.T) {
	old := &Runtime{OIDCEnabled: true, OIDC: config.OIDCConfig{
		Issuer: "https://idp", ClientID: "c", Scopes: []string{"openid"}, AllowedEmails: []string{"alice@example.com"}, GroupsClaim: "groups",
	}}
	sess := db.AuthSession{Email: "alice@example.com", EmailVerified: true, GroupsJSON: `["admins"]`}
	req := httptest.NewRequest(http.MethodPut, "/api/settings", nil)
	req = req.WithContext(auth.ContextWithSession(req.Context(), sess))
	authn := auth.New(nil, "k", false)

	patch := map[string]json.RawMessage{"oidc_allowed_emails": []byte(`["bob@example.com"]`)}
	err := guardOIDCPatch(req, patch, map[string]bool{"oidc_allowed_emails": true}, old, authn)
	var se *settingsError
	if err == nil || !asSettingsError(err, &se) || se.status != http.StatusConflict || se.code != "oidc_lockout" {
		t.Fatalf("lockout edit: err = %v, want 409 oidc_lockout", err)
	}

	// Still allowed when the caller stays authorized (via a group).
	patch = map[string]json.RawMessage{"oidc_allowed_emails": []byte(`["bob@example.com"]`), "oidc_allowed_groups": []byte(`["admins"]`)}
	if err := guardOIDCPatch(req, patch, map[string]bool{"oidc_allowed_emails": true, "oidc_allowed_groups": true}, old, authn); err != nil {
		t.Errorf("edit keeping the caller authorized: %v", err)
	}

	// DASHCAM_AUTH_DISABLE skips the guard.
	patch = map[string]json.RawMessage{"oidc_allowed_emails": []byte(`["bob@example.com"]`)}
	if err := guardOIDCPatch(req, patch, map[string]bool{"oidc_allowed_emails": true}, old, auth.New(nil, "k", true)); err != nil {
		t.Errorf("AuthDisable should skip the lockout guard: %v", err)
	}
}

func asSettingsError(err error, target **settingsError) bool {
	se, ok := err.(*settingsError)
	if ok {
		*target = se
	}
	return ok
}

func TestSettingsMetaAndOIDCTestEndpoints(t *testing.T) {
	t.Setenv("DASHCAM_SECRET_KEY", "test-key")
	mux, store := newTestMuxWithStore(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/meta", nil))
	if rec.Code != 200 {
		t.Fatalf("meta = %d: %s", rec.Code, rec.Body)
	}
	meta := decodeBody(t, rec)
	for _, k := range []string{"bootstrap", "secret_key_source", "auth_disabled", "env_differs", "restart_required", "redirect_uri"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("meta missing %q: %s", k, rec.Body)
		}
	}

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/api/settings/oidc/test", `{"issuer":"https://idp.example.com","client_id":"dashcam"}`); rec.Code != 400 || errorCode(rec) != "public_url_required" {
		t.Errorf("test without public_url = %d %q", rec.Code, errorCode(rec))
	}
	if rec := post("/api/settings/oidc/test", `{"issuer":"nope","client_id":"d","public_url":"https://d.example.com"}`); rec.Code != 400 {
		t.Errorf("bad issuer = %d", rec.Code)
	}
	rec = post("/api/settings/oidc/test", `{"issuer":"https://idp.example.com","client_id":"dashcam","client_secret":"s","allowed_emails":["A@x.com"],"public_url":"https://d.example.com/"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/auth/test-login") {
		t.Fatalf("test start = %d: %s", rec.Code, rec.Body)
	}
	cfgJSON, ok, err := store.OIDCPendingBind(context.Background(), "x", time.Now())
	if err != nil || !ok {
		t.Fatalf("pending not stored: %v %v", ok, err)
	}
	if strings.Contains(cfgJSON, `"s"`) || !strings.Contains(cfgJSON, `"a@x.com"`) || !strings.Contains(cfgJSON, `"https://d.example.com"`) {
		t.Errorf("pending cfg = %s (secret must be encrypted, email lower-cased, URL normalized)", cfgJSON)
	}
	if rec := post("/api/settings/oidc/discover", `{"issuer":"nope"}`); rec.Code != 400 {
		t.Errorf("discover bad issuer = %d", rec.Code)
	}
}
