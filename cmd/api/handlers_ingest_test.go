package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSSDStatusReportsAbsentWhenUnmounted exercises the truthful-presence
// guard: an SSD source whose root_path exists as a directory but isn't
// actually mounted there (e.g. the drive was unplugged, leaving a stale
// empty mount point) must report present:false, not present:true with
// zeroed-out free space.
func TestSSDStatusReportsAbsentWhenUnmounted(t *testing.T) {
	mux, store := newTestMuxWithStore(t)

	root := t.TempDir() // exists, but ssdMountedFn is stubbed to say "not mounted"
	if _, err := store.SourceCreate(context.Background(), "ssd", root, "SSD"); err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	orig := ssdMountedFn
	ssdMountedFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { ssdMountedFn = orig })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ssd/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ssd/status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var view ssdStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Present {
		t.Errorf("Present = true, want false for an existing-but-unmounted root")
	}
	if view.FreeBytes != nil || view.TotalBytes != nil {
		t.Errorf("expected no free/total space for an absent SSD, got free=%v total=%v", view.FreeBytes, view.TotalBytes)
	}
}

// TestSSDStatusIsMountedErrorTreatedAsAbsent exercises the error path: if
// ssdMountedFn itself errors (e.g. /proc/mounts unreadable), the handler
// must log and treat the SSD as absent rather than fail the request.
func TestSSDStatusIsMountedErrorTreatedAsAbsent(t *testing.T) {
	mux, store := newTestMuxWithStore(t)

	root := t.TempDir()
	if _, err := store.SourceCreate(context.Background(), "ssd", root, "SSD"); err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	orig := ssdMountedFn
	ssdMountedFn = func(string) (bool, error) { return false, errors.New("boom") }
	t.Cleanup(func() { ssdMountedFn = orig })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ssd/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ssd/status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var view ssdStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Present {
		t.Errorf("Present = true, want false when ssdMountedFn errors")
	}
}
