package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/dashcam/dashcamtest"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// dashcamHandlerFixture bundles a store/mux/queue (workers not started, so
// 202 snapshot assertions see the job still queued) plus a fake recorder
// and the share id dashcams can target.
type dashcamHandlerFixture struct {
	mux     *http.ServeMux
	store   *db.DB
	queue   *jobs.Queue
	fake    *dashcamtest.Fake
	shareID int64
}

func newDashcamHandlerFixture(t *testing.T) *dashcamHandlerFixture {
	t.Helper()
	t.Setenv("DASHCAM_SECRET_KEY", "test-secret-key")

	dbPath := filepath.Join(t.TempDir(), "dashcam.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Setenv("DASHCAM_DB", dbPath)

	ctx := context.Background()
	if err := store.SettingsSet(ctx, "allow_local_share_paths", []byte("true")); err != nil {
		t.Fatalf("SettingsSet allow_local_share_paths: %v", err)
	}
	shareID, _, err := store.ShareCreateWithSource(ctx, db.ShareCreateParams{
		Name: "target", Server: "srv", ShareName: "footage", Enabled: true,
	}, func(id int64) string { return t.TempDir() }, "target share")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}

	fake := dashcamtest.New(t.TempDir())
	t.Cleanup(fake.Close)

	queue := jobs.New(store)
	b := newBroker()
	queue.OnEvent(b.publishJob)
	registerJobHandlers(queue, store)

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, queue, b, runCtx)

	return &dashcamHandlerFixture{mux: mux, store: store, queue: queue, fake: fake, shareID: shareID}
}

func (f *dashcamHandlerFixture) do(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// createDashcam POSTs a dashcam pointed at f.fake and returns its id.
func (f *dashcamHandlerFixture) createDashcam(t *testing.T) int64 {
	t.Helper()
	w := f.do(t, "POST", "/api/dashcams", map[string]any{
		"label": "Front Cam", "base_url": f.fake.URL(), "target_share_id": f.shareID,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/dashcams = %d: %s", w.Code, w.Body.String())
	}
	var view dashcamView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return view.ID
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	return out
}

// -- CRUD --------------------------------------------------------------------

func TestDashcamsCRUD(t *testing.T) {
	f := newDashcamHandlerFixture(t)

	if w := f.do(t, "GET", "/api/dashcams", nil); w.Code != http.StatusOK {
		t.Fatalf("GET /api/dashcams (empty) = %d", w.Code)
	} else if got := decodeJSON[[]dashcamView](t, w); len(got) != 0 {
		t.Fatalf("GET /api/dashcams (empty) = %v, want []", got)
	}

	id := f.createDashcam(t)

	w := f.do(t, "GET", "/api/dashcams", nil)
	list := decodeJSON[[]dashcamView](t, w)
	if len(list) != 1 || list[0].ID != id || list[0].BaseURL != f.fake.URL() || list[0].TargetShareID != f.shareID {
		t.Fatalf("GET /api/dashcams = %+v", list)
	}
	if list[0].TargetShareName != "target" {
		t.Fatalf("target_share_label = %q, want %q", list[0].TargetShareName, "target")
	}

	w = f.do(t, "PATCH", "/api/dashcams/"+itoa(id), map[string]any{"label": "Renamed"})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", w.Code, w.Body.String())
	}
	patched := decodeJSON[dashcamView](t, w)
	if patched.Label != "Renamed" || patched.BaseURL != f.fake.URL() {
		t.Fatalf("PATCH result = %+v, want label=Renamed base_url unchanged", patched)
	}

	w = f.do(t, "DELETE", "/api/dashcams/"+itoa(id), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	w = f.do(t, "GET", "/api/dashcams", nil)
	if got := decodeJSON[[]dashcamView](t, w); len(got) != 0 {
		t.Fatalf("GET /api/dashcams after delete = %v, want []", got)
	}
}

func TestDashcamsPostValidation(t *testing.T) {
	f := newDashcamHandlerFixture(t)

	cases := []map[string]any{
		{"label": "", "base_url": "http://x", "target_share_id": f.shareID},
		{"label": "x", "base_url": "not-a-url", "target_share_id": f.shareID},
		{"label": "x", "base_url": "ftp://x", "target_share_id": f.shareID},
		{"label": "x", "base_url": "http://x", "target_share_id": 999999},
		{"label": "x", "base_url": "http://x"},                                   // missing target_share_id
		{"label": "x", "base_url": "http://", "target_share_id": f.shareID},      // no host
		{"label": "x", "base_url": "http:///path", "target_share_id": f.shareID}, // no host
	}
	for i, body := range cases {
		w := f.do(t, "POST", "/api/dashcams", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("case %d: POST /api/dashcams = %d, want 400: %s", i, w.Code, w.Body.String())
		}
	}
}

func TestValidBaseURL(t *testing.T) {
	cases := map[string]bool{
		"http://10.0.0.9":      true,
		"https://cam.local:80": true,
		"http://cam.local/x":   true,
		"not-a-url":            false,
		"ftp://10.0.0.9":       false,
		"":                     false,
		"http://":              false, // no host
		"http:///path":         false, // no host
		"/relative/path":       false, // not absolute
	}
	for raw, want := range cases {
		if got := validBaseURL(raw); got != want {
			t.Errorf("validBaseURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestDashcamsDeleteNotFound(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	if w := f.do(t, "DELETE", "/api/dashcams/999", nil); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing = %d", w.Code)
	}
	if w := f.do(t, "PATCH", "/api/dashcams/999", map[string]any{"label": "x"}); w.Code != http.StatusNotFound {
		t.Fatalf("PATCH missing = %d", w.Code)
	}
}

// -- 202 job snapshots ---------------------------------------------------------

func TestDashcamPullPostEnqueuesJob(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	w := f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/pull", map[string]any{"full": true})
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST pull = %d: %s", w.Code, w.Body.String())
	}
	var snap struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Job.Type != "dashcam_pull" {
		t.Fatalf("job type = %q, want dashcam_pull", snap.Job.Type)
	}
	var params dashcamPullJobParams
	if err := json.Unmarshal(snap.Job.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.SourceID != id || !params.Full {
		t.Fatalf("params = %+v, want source_id=%d full=true", params, id)
	}
}

func TestDashcamCleanupPostDefaultsDryRunTrue(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	w := f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/cleanup", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST cleanup = %d: %s", w.Code, w.Body.String())
	}
	var snap struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	var params dashcamCleanupJobParams
	if err := json.Unmarshal(snap.Job.Params, &params); err != nil {
		t.Fatalf("decode job params %s: %v", snap.Job.Params, err)
	}
	if params.SourceID != id {
		t.Fatalf("source_id = %d, want %d", params.SourceID, id)
	}
	if params.DryRun != nil {
		t.Fatalf("dry_run = %v, want nil (job handler defaults it to true)", *params.DryRun)
	}

	falseVal := false
	w = f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/cleanup", map[string]any{"dry_run": false})
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	if err := json.Unmarshal(snap.Job.Params, &params); err != nil {
		t.Fatalf("decode job params %s: %v", snap.Job.Params, err)
	}
	if params.DryRun == nil || *params.DryRun != falseVal {
		t.Fatalf("dry_run = %v, want false", params.DryRun)
	}
}

func TestDashcamReprocessPostValidatesScope(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	if w := f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/reprocess", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Fatalf("empty scope = %d, want 400: %s", w.Code, w.Body.String())
	}

	w := f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/reprocess", map[string]any{"camera": "front", "date": "2026-09-08"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("valid scope = %d: %s", w.Code, w.Body.String())
	}
	var snap struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	if snap.Job.Type != "dashcam_reprocess" {
		t.Fatalf("job type = %q", snap.Job.Type)
	}
}

// -- key management ------------------------------------------------------------

func TestDashcamKeyGetGenerateForce(t *testing.T) {
	f := newDashcamHandlerFixture(t)

	w := f.do(t, "GET", "/api/dashcams/key", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET key = %d", w.Code)
	}
	got := decodeJSON[dashcamKeyView](t, w)
	if got.Configured {
		t.Fatalf("GET key before generate: Configured=true")
	}

	w = f.do(t, "POST", "/api/dashcams/key/generate", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST generate = %d: %s", w.Code, w.Body.String())
	}
	gen := decodeJSON[dashcamKeyView](t, w)
	if !gen.Configured || gen.KeyID == "" || gen.PublicKeyB64 == "" {
		t.Fatalf("generate result = %+v", gen)
	}
	if gen.ConfigSnippet == "" {
		t.Fatalf("config_snippet is empty")
	}

	// A second generate without force is refused.
	w = f.do(t, "POST", "/api/dashcams/key/generate", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("second generate without force = %d, want 409", w.Code)
	}

	// force:true replaces it with a fresh key.
	w = f.do(t, "POST", "/api/dashcams/key/generate", map[string]any{"force": true})
	if w.Code != http.StatusCreated {
		t.Fatalf("forced generate = %d: %s", w.Code, w.Body.String())
	}
	forced := decodeJSON[dashcamKeyView](t, w)
	if forced.KeyID == gen.KeyID {
		t.Fatalf("forced generate returned the same key id %q", forced.KeyID)
	}

	// GET now reflects the forced key.
	w = f.do(t, "GET", "/api/dashcams/key", nil)
	after := decodeJSON[dashcamKeyView](t, w)
	if after.KeyID != forced.KeyID {
		t.Fatalf("GET key after force = %+v, want key_id=%q", after, forced.KeyID)
	}
}

// -- /test and /status against the fake ----------------------------------------

func TestDashcamTestAgainstFake(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	// No key configured yet: never a 500, ok:false with an error_code.
	w := f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/test", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST test (no key) = %d", w.Code)
	}
	res := decodeJSON[dashcamTestResult](t, w)
	if res.OK {
		t.Fatalf("test with no key configured: OK=true, want false")
	}
	if res.ErrorCode != "key_not_configured" {
		t.Fatalf("test with no key configured: error_code = %q, want stable code %q, not prose", res.ErrorCode, "key_not_configured")
	}

	// Generate a key but don't register it with the fake: expect a
	// reachable-but-rejected outcome, still 200/ok:false.
	genW := f.do(t, "POST", "/api/dashcams/key/generate", nil)
	gen := decodeJSON[dashcamKeyView](t, genW)

	w = f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/test", nil)
	res = decodeJSON[dashcamTestResult](t, w)
	if w.Code != http.StatusOK || res.OK {
		t.Fatalf("test with unregistered key: code=%d ok=%v, want 200/false", w.Code, res.OK)
	}
	if res.ErrorCode == "" {
		t.Fatalf("test with unregistered key: error_code empty")
	}

	// Register the key with the fake: now it should succeed.
	pubRaw, err := base64.StdEncoding.DecodeString(gen.PublicKeyB64)
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	f.fake.AddKey(gen.KeyID, ed25519.PublicKey(pubRaw))

	w = f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/test", nil)
	res = decodeJSON[dashcamTestResult](t, w)
	if w.Code != http.StatusOK || !res.OK {
		t.Fatalf("test with registered key: code=%d ok=%v body=%s", w.Code, res.OK, w.Body.String())
	}
	if res.Lease == nil {
		t.Fatalf("test success: lease is nil")
	}
}

// TestDashcamTestReportsRealSkew covers fix round 1 item 2:
// handleDashcamTest used to hardcode skew_s:0 and server_time from the
// manager's own clock; it must now report the client's actually-measured
// skew (learned via the transparent clock_skew retry) instead.
func TestDashcamTestReportsRealSkew(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	genW := f.do(t, "POST", "/api/dashcams/key/generate", nil)
	gen := decodeJSON[dashcamKeyView](t, genW)
	pubRaw, err := base64.StdEncoding.DecodeString(gen.PublicKeyB64)
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	f.fake.AddKey(gen.KeyID, ed25519.PublicKey(pubRaw))

	offset := 10 * time.Minute
	skewedNow := time.Now().Add(offset)
	f.fake.SetNow(func() time.Time { return skewedNow })

	w := f.do(t, "POST", "/api/dashcams/"+itoa(id)+"/test", nil)
	res := decodeJSON[dashcamTestResult](t, w)
	if w.Code != http.StatusOK || !res.OK {
		t.Fatalf("test with skewed fake clock: code=%d ok=%v body=%s", w.Code, res.OK, w.Body.String())
	}
	wantSkewS := offset.Seconds()
	if d := res.SkewS - wantSkewS; d < -2 || d > 2 {
		t.Fatalf("skew_s = %v, want ~%v", res.SkewS, wantSkewS)
	}
	if res.ServerTime == "" {
		t.Fatalf("server_time is empty")
	}
	gotServerTime, err := time.Parse(time.RFC3339, res.ServerTime)
	if err != nil {
		t.Fatalf("parse server_time %q: %v", res.ServerTime, err)
	}
	if d := gotServerTime.Sub(skewedNow); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("server_time = %v, want close to the fake's skewed clock %v", gotServerTime, skewedNow)
	}
}

func TestDashcamStatusShape(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	genW := f.do(t, "POST", "/api/dashcams/key/generate", nil)
	gen := decodeJSON[dashcamKeyView](t, genW)
	pubRaw, err := base64.StdEncoding.DecodeString(gen.PublicKeyB64)
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	f.fake.AddKey(gen.KeyID, ed25519.PublicKey(pubRaw))

	w := f.do(t, "GET", "/api/dashcams/"+itoa(id)+"/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", w.Code, w.Body.String())
	}
	got := decodeJSON[dashcamStatusResult](t, w)
	if !got.Reachable {
		t.Fatalf("status Reachable = false, want true: %+v", got)
	}
	if got.Lease == nil {
		t.Fatalf("status Lease = nil")
	}
	if got.Reprocess == nil || got.Reprocess.State != "idle" {
		t.Fatalf("status Reprocess = %+v, want state=idle", got.Reprocess)
	}
	if got.Pulls.Verified != 0 || got.Pulls.PendingCleanup != 0 {
		t.Fatalf("status Pulls = %+v, want zero", got.Pulls)
	}
}

func TestDashcamStatusNotFound(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	if w := f.do(t, "GET", "/api/dashcams/999/status", nil); w.Code != http.StatusNotFound {
		t.Fatalf("status for missing dashcam = %d", w.Code)
	}
}

// TestDashcamStatusStableErrorCodeAndAlwaysPresentKeys covers fix round 1
// minor items: handleDashcamStatus must report a stable machine code
// ("key_not_configured", not err.Error() prose) when no key is set yet,
// and error_code/lease/reprocess must always be present keys in the JSON
// (no omitempty), even though lease/reprocess are null in that case.
func TestDashcamStatusStableErrorCodeAndAlwaysPresentKeys(t *testing.T) {
	f := newDashcamHandlerFixture(t)
	id := f.createDashcam(t)

	w := f.do(t, "GET", "/api/dashcams/"+itoa(id)+"/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", w.Code, w.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	for _, key := range []string{"error_code", "lease", "reprocess"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response missing key %q (omitempty must be dropped): %s", key, w.Body.String())
		}
	}

	got := decodeJSON[dashcamStatusResult](t, w)
	if got.Reachable {
		t.Fatalf("Reachable = true with no key configured")
	}
	if got.ErrorCode != "key_not_configured" {
		t.Fatalf("ErrorCode = %q, want stable code %q, not prose", got.ErrorCode, "key_not_configured")
	}
	if got.Lease != nil || got.Reprocess != nil {
		t.Fatalf("Lease/Reprocess = %+v/%+v, want both nil with no key configured", got.Lease, got.Reprocess)
	}
}
