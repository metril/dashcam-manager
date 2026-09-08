package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dashcam/internal/config"
	"dashcam/internal/db"
)

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

	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, context.Background())
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
