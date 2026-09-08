package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestSourcesPostValidatesRootPath covers M4 review finding #5: root_path
// must be absolute, cleaned, and an existing directory.
func TestSourcesPostValidatesRootPath(t *testing.T) {
	mux := newTestMux(t)

	post := func(rootPath string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"kind": "ssd", "root_path": rootPath, "label": "test"})
		req := httptest.NewRequest(http.MethodPost, "/api/sources", bytes.NewReader(body)).WithContext(context.Background())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"relative path", "mnt/dashcam", http.StatusBadRequest},
		{"not cleaned (trailing slash)", "/mnt/dashcam/", http.StatusBadRequest},
		{"not cleaned (dot segment)", "/mnt/./dashcam", http.StatusBadRequest},
		{"nonexistent directory", "/does/not/exist/at/all", http.StatusBadRequest},
		{"existing directory", t.TempDir(), http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(c.path)
			if rec.Code != c.wantCode {
				t.Errorf("POST /api/sources {root_path:%q} = %d, want %d (body: %s)", c.path, rec.Code, c.wantCode, rec.Body.String())
			}
		})
	}
}

// TestSourcesPostRejectsFile covers the "existing directory" half of
// finding #5: an existing regular file must be rejected too, not just a
// missing path.
func TestSourcesPostRejectsFile(t *testing.T) {
	mux := newTestMux(t)
	filePath := filepath.Join(t.TempDir(), "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"kind": "ssd", "root_path": filePath, "label": "test"})
	req := httptest.NewRequest(http.MethodPost, "/api/sources", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/sources with a file as root_path = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}
