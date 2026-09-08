package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// newTestMuxWithSecretKey is newTestMux (main_test.go) but with an explicit
// DASHCAM_SECRET_KEY, so share-password tests can exercise both the
// "key configured" and "key empty, refuse to store a password" paths.
func newTestMuxWithSecretKey(t *testing.T, secretKey string) *http.ServeMux {
	t.Helper()
	t.Setenv("DASHCAM_SECRET_KEY", secretKey)

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
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, config.Load(), store, queue, b, ctx)
	return mux
}

func postShare(t *testing.T, mux *http.ServeMux, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/shares", bytes.NewReader(b)).WithContext(context.Background())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSharesPostValidatesRequiredFields(t *testing.T) {
	mux := newTestMux(t)

	cases := []struct {
		name     string
		body     map[string]any
		wantCode int
	}{
		{"missing server", map[string]any{"share": "footage"}, http.StatusBadRequest},
		{"missing share", map[string]any{"server": "nas.local"}, http.StatusBadRequest},
		{"missing both", map[string]any{}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postShare(t, mux, c.body)
			if rec.Code != c.wantCode {
				t.Errorf("POST /api/shares %v = %d, want %d (body: %s)", c.body, rec.Code, c.wantCode, rec.Body.String())
			}
		})
	}
}

func TestSharesPostRejectsPasswordWithoutSecretKey(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "")

	rec := postShare(t, mux, map[string]any{"server": "nas.local", "share": "footage", "password": "hunter2"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/shares with password and empty secret key = %d, want 503 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "DASHCAM_SECRET_KEY") {
		t.Errorf("expected error to mention DASHCAM_SECRET_KEY, got: %s", rec.Body.String())
	}

	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/shares", nil))
	if strings.Contains(getRec.Body.String(), "nas.local") {
		t.Errorf("expected no share row to be created, got: %s", getRec.Body.String())
	}
}

func TestSharesPostSucceedsWithoutPassword(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/shares = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestSharesPasswordNeverReturned(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage", "password": "hunter2"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/shares = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("response body leaks the plaintext password: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"has_password":true`) {
		t.Errorf("expected has_password:true, got: %s", rec.Body.String())
	}

	// GET should also never leak it.
	getReq := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if strings.Contains(getRec.Body.String(), "hunter2") {
		t.Fatalf("GET /api/shares leaks the plaintext password: %s", getRec.Body.String())
	}
}

func TestSharesPostCreatesLinkedSource(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/shares = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)

	var views []shareView
	if err := json.Unmarshal(getRec.Body.Bytes(), &views); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d shares, want 1", len(views))
	}
	if views[0].SourceID == nil {
		t.Error("expected the created share to have a linked source_id")
	}
}

// TestSharesDeleteKeepsEmptySourceOrphaned covers final-fix-wave item 2:
// sources.share_id is now ON DELETE SET NULL, not CASCADE, so deleting a
// share no longer silently destroys its linked source. With zero indexed
// segments there's nothing to lose, so the delete proceeds without
// purge_index and the source survives (orphaned, no longer tied to any
// share) rather than being deleted.
func TestSharesDeleteKeepsEmptySourceOrphaned(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/shares/"+itoa(created.ID), nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/shares/%d = %d, want 204 (body: %s)", created.ID, delRec.Code, delRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "Garage NAS") {
		t.Errorf("expected the linked (empty) source to survive share deletion, got: %s", getRec.Body.String())
	}
}

// TestSharesDeleteRefusesWhenSourceHasSegmentsUnlessPurge covers the 409
// guard: a share whose source has indexed segments can't be deleted
// without ?purge_index=1, and with it the source (and its segments) is
// explicitly removed.
func TestSharesDeleteRefusesWhenSourceHasSegmentsUnlessPurge(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// handleSharesPost's response doesn't populate source_id (only
	// handleSharesGet's does the lookup) — fetch it via GET.
	getReq0 := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	getRec0 := httptest.NewRecorder()
	mux.ServeHTTP(getRec0, getReq0)
	var views []shareView
	if err := json.Unmarshal(getRec0.Body.Bytes(), &views); err != nil {
		t.Fatalf("unmarshal shares list: %v", err)
	}
	if len(views) != 1 || views[0].SourceID == nil {
		t.Fatalf("expected linked source_id, got: %s", getRec0.Body.String())
	}
	created.SourceID = views[0].SourceID

	if _, err := store.Write().ExecContext(context.Background(),
		`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, bytes, has_detections, has_gps)
		 VALUES (?, 'front', 'front/x/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', 1000, 0, 0)`,
		*created.SourceID); err != nil {
		t.Fatalf("seed segment: %v", err)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/shares/"+itoa(created.ID), nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusConflict {
		t.Fatalf("DELETE /api/shares/%d (segments present, no purge_index) = %d, want 409 (body: %s)",
			created.ID, delRec.Code, delRec.Body.String())
	}

	delReq2 := httptest.NewRequest(http.MethodDelete, "/api/shares/"+itoa(created.ID)+"?purge_index=1", nil)
	delRec2 := httptest.NewRecorder()
	mux.ServeHTTP(delRec2, delReq2)
	if delRec2.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/shares/%d?purge_index=1 = %d, want 204 (body: %s)", created.ID, delRec2.Code, delRec2.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/sources", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if strings.Contains(getRec.Body.String(), "Garage NAS") {
		t.Errorf("expected the source to be purged along with the share, got: %s", getRec.Body.String())
	}
}

func TestSharesPatchKeepsPasswordWhenOmitted(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage", "password": "hunter2"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !created.HasPassword {
		t.Fatal("expected has_password true after create")
	}

	patchBody, _ := json.Marshal(map[string]any{"name": "Garage NAS 2", "server": "nas.local", "share": "footage"})
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/shares/"+itoa(created.ID), bytes.NewReader(patchBody))
	patchRec := httptest.NewRecorder()
	mux.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (body: %s)", patchRec.Code, patchRec.Body.String())
	}

	var patched shareView
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !patched.HasPassword {
		t.Error("expected has_password to remain true after a PATCH that omitted password")
	}
}

func TestSharesTestEndpointReturnsCleanErrorOnBogusCredentials(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Bogus NAS", "server": "nonexistent.invalid", "share": "footage", "username": "nope", "password": "wrong"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	testReq := httptest.NewRequest(http.MethodPost, "/api/shares/"+itoa(created.ID)+"/test", nil)
	testRec := httptest.NewRecorder()
	mux.ServeHTTP(testRec, testReq)

	// The test environment has no mount.cifs / CAP_SYS_ADMIN, so this must
	// fail cleanly (HTTP 200 with ok:false), never panic or hang.
	if testRec.Code != http.StatusOK {
		t.Fatalf("POST .../test = %d, want 200 (body: %s)", testRec.Code, testRec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(testRec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ok, _ := result["ok"].(bool); ok {
		t.Error("expected ok:false against an unmountable share in the test environment")
	}
	if _, hasErr := result["error"]; !hasErr {
		t.Error("expected an error message in the response")
	}
}

func TestSharesPostRejectsDeniedOptions(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"server": "nas.local", "share": "footage", "options": "ro,password=hunter2"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/shares with password= in options = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestSharesGetOmitsFreeSpaceWhenUnmounted(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// A freshly created share's mount point doesn't exist / isn't mounted
	// in the test environment, so free/total space must be omitted rather
	// than reporting stats for some unrelated filesystem.
	if created.FreeBytes != nil || created.TotalBytes != nil {
		t.Errorf("expected nil free/total bytes for an unmounted share, got free=%v total=%v", created.FreeBytes, created.TotalBytes)
	}
}

func TestSharesPatchDisablingSetsStatusDisabled(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	patchBody, _ := json.Marshal(map[string]any{"server": "nas.local", "share": "footage", "enabled": false})
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/shares/"+itoa(created.ID), bytes.NewReader(patchBody))
	patchRec := httptest.NewRecorder()
	mux.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (body: %s)", patchRec.Code, patchRec.Body.String())
	}

	var patched shareView
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if patched.Status != "disabled" {
		t.Errorf("status after disabling PATCH = %q, want %q", patched.Status, "disabled")
	}
}

func TestSharesPatchChangingServerSetsStatusUnmounted(t *testing.T) {
	mux := newTestMuxWithSecretKey(t, "test-secret-key")

	rec := postShare(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	var created shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	patchBody, _ := json.Marshal(map[string]any{"server": "other-nas.local", "share": "footage"})
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/shares/"+itoa(created.ID), bytes.NewReader(patchBody))
	patchRec := httptest.NewRecorder()
	mux.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (body: %s)", patchRec.Code, patchRec.Body.String())
	}

	var patched shareView
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if patched.Status != "unmounted" {
		t.Errorf("status after server-changing PATCH = %q, want %q", patched.Status, "unmounted")
	}
	// mount_point must stay stable across the rename/retarget (scoped to
	// share id, not name/server) so any already-indexed source/segments
	// aren't orphaned.
	if patched.MountPoint != created.MountPoint {
		t.Errorf("mount_point changed on PATCH: %q -> %q", created.MountPoint, patched.MountPoint)
	}
}
