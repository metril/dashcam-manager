package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dashcam/internal/config"
)

func TestHealthEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	registerRoutes(mux, config.Load())

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

func TestStaticSPAFallback(t *testing.T) {
	mux := http.NewServeMux()
	registerRoutes(mux, config.Load())

	req := httptest.NewRequest(http.MethodGet, "/some/client/route", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestUnknownAPIPathIs404(t *testing.T) {
	mux := http.NewServeMux()
	registerRoutes(mux, config.Load())

	req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
