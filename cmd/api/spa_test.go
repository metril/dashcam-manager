package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// fakeDist mimics an embedded dist/ tree with an index.html shell plus one
// content-hashed asset, matching what a real `vite build` produces.
func fakeDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            {Data: []byte("<html>app shell</html>")},
		"assets/index-abc1.js":  {Data: []byte("console.log('hi')")},
		"assets/index-abc1.css": {Data: []byte("body{}")},
	}
}

func TestSPAHandler_ServesIndexForClientRoute(t *testing.T) {
	h := spaHandler(fakeDist())

	req := httptest.NewRequest(http.MethodGet, "/some/client/route", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "<html>app shell</html>" {
		t.Errorf("body = %q, want the app shell", rec.Body.String())
	}
}

func TestSPAHandler_ServesRealAsset(t *testing.T) {
	h := spaHandler(fakeDist())

	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc1.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want immutable caching for fingerprinted assets", got)
	}
}

func TestSPAHandler_NoDirectoryListing(t *testing.T) {
	h := spaHandler(fakeDist())

	req := httptest.NewRequest(http.MethodGet, "/assets/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (app shell fallback)", rec.Code)
	}
	if rec.Body.String() != "<html>app shell</html>" {
		t.Errorf("body = %q, want the app shell (not a directory listing)", rec.Body.String())
	}
}

func TestSPAHandler_UnknownAPIPathIsJSON404(t *testing.T) {
	h := spaHandler(fakeDist())

	req := httptest.NewRequest(http.MethodGet, "/api/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "not found" {
		t.Errorf(`body["error"] = %q, want "not found"`, body["error"])
	}
}

func TestSPAHandler_MissingIndexIs503(t *testing.T) {
	// No index.html at all — e.g. a checkout where the frontend hasn't been
	// built yet (only cmd/api/dist/.gitkeep is tracked in git).
	h := spaHandler(fstest.MapFS{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestIsStaticAsset(t *testing.T) {
	root := fakeDist()
	for path, want := range map[string]bool{
		"/assets/index-abc1.js": true,
		"/assets":               false, // directory
		"/":                     false,
		"/some/client/route":    false,
		"/assets/missing.js":    false,
	} {
		if got := isStaticAsset(root, path); got != want {
			t.Errorf("isStaticAsset(%q) = %v, want %v", path, got, want)
		}
	}
}
