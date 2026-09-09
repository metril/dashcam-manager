package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/sys/unix"
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

// TestIngestEjectRefusedWhileCleanupJobActive exercises the busy guard
// generalized in activeJobsBySourceOfType: previously POST
// /api/ingest/eject only refused a forced eject (EBUSY) when a *scan* job
// was active on the SSD source, ignoring an in-flight ingest or cleanup
// job that's equally unsafe to yank the device out from under. This seeds
// a queued "cleanup" job for the SSD source (workers aren't started, so
// it can't race to completion), stubs ssdEjectFn to report EBUSY as if
// something still had the mount open, and asserts eject is refused with
// 409 rather than falling through to a force-eject.
func TestIngestEjectRefusedWhileCleanupJobActive(t *testing.T) {
	mux, store, queue := newTestMuxWithStoreAndQueueNoWorkers(t)

	root := t.TempDir()
	srcID, err := store.SourceCreate(context.Background(), "ssd", root, "SSD")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}

	if _, err := queue.Enqueue(context.Background(), "cleanup", cleanupJobParams{SourceID: srcID}, 0); err != nil {
		t.Fatalf("enqueue cleanup job: %v", err)
	}

	origEject, origForce := ssdEjectFn, ssdForceEjectFn
	ssdEjectFn = func(string) error { return unix.EBUSY }
	ssdForceEjectFn = func(string) error {
		t.Fatal("ForceEject must not be called while a cleanup job is active")
		return nil
	}
	t.Cleanup(func() { ssdEjectFn, ssdForceEjectFn = origEject, origForce })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/ingest/eject", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /api/ingest/eject = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
}
