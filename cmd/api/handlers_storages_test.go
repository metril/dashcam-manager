package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"dashcam/internal/db"
)

// newTestMuxWithLocalRoot is newTestMuxWithStore but with DASHCAM_LOCAL_ROOT
// set to a fresh temp dir, for tests that create kind=local storages under
// it.
func newTestMuxWithLocalRoot(t *testing.T) (*http.ServeMux, *db.DB, string) {
	t.Helper()
	localRoot := t.TempDir()
	t.Setenv("DASHCAM_LOCAL_ROOT", localRoot)
	mux, store := newTestMuxWithStore(t)
	return mux, store, localRoot
}

func postStorage(t *testing.T, mux *http.ServeMux, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/storages", bytes.NewReader(b)).WithContext(context.Background())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func getStorages(t *testing.T, mux *http.ServeMux) []storageView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/storages", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/storages = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var views []storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return views
}

// TestStoragesGetIncludesCachePseudoStorage covers the always-present
// cache row: even with zero shares configured, GET /api/storages must
// return exactly the cache pseudo-storage and never the SSD.
func TestStoragesGetIncludesCachePseudoStorage(t *testing.T) {
	mux, _, _ := newTestMuxWithLocalRoot(t)

	views := getStorages(t, mux)
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1 (cache only), got: %+v", len(views), views)
	}
	if views[0].ID != "cache" || views[0].Kind != "cache" {
		t.Errorf("views[0] = %+v, want the cache pseudo-storage", views[0])
	}
}

// TestStoragesPostLocalCreatesShare covers the happy path: a valid,
// existing directory under DASHCAM_LOCAL_ROOT becomes a kind=local storage.
func TestStoragesPostLocalCreatesShare(t *testing.T) {
	mux, _, localRoot := newTestMuxWithLocalRoot(t)
	dir := filepath.Join(localRoot, "backup-drive")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "Backup Drive", "path": dir})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/storages = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Kind != db.KindLocal {
		t.Errorf("Kind = %q, want %q", created.Kind, db.KindLocal)
	}
	if created.Path != dir {
		t.Errorf("Path = %q, want %q", created.Path, dir)
	}
	if created.Label != "Backup Drive" {
		t.Errorf("Label = %q, want %q", created.Label, "Backup Drive")
	}

	views := getStorages(t, mux)
	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2 (local + cache), got: %+v", len(views), views)
	}
}

// TestStoragesPostLocalValidatesPath table-drives every path-validation
// rejection the plan calls for: outside DASHCAM_LOCAL_ROOT, "..", "/"
// itself, a symlink escaping the root, and a nonexistent directory.
func TestStoragesPostLocalValidatesPath(t *testing.T) {
	mux, _, localRoot := newTestMuxWithLocalRoot(t)

	outsideDir := t.TempDir() // sibling temp dir, NOT under localRoot
	escapeTarget := t.TempDir()
	symlinkPath := filepath.Join(localRoot, "escape-link")
	if err := os.Symlink(escapeTarget, symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"outside local root", outsideDir},
		{"dot-dot traversal", filepath.Join(localRoot, "..", "etc")},
		{"root slash itself", "/"},
		{"symlink escapes root", symlinkPath},
		{"nonexistent directory", filepath.Join(localRoot, "does-not-exist")},
		{"empty path", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postStorage(t, mux, map[string]any{"kind": "local", "label": c.name, "path": c.path})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /api/storages path=%q = %d, want 400 (body: %s)", c.path, rec.Code, rec.Body.String())
			}
		})
	}

	// None of the rejected attempts should have created a storage.
	views := getStorages(t, mux)
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1 (cache only, no rejected storage should have landed), got: %+v", len(views), views)
	}
}

// TestStoragesPostLocalRejectsUnwritablePath covers ProbeLocal's write
// check: an existing, in-root, read-only directory is still rejected —
// unlike LocalDirReadable, a newly registered storage must be usable as a
// write target.
func TestStoragesPostLocalRejectsUnwritablePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root can write to a 0500 dir; see TestWriteProbeDetectsReadOnly's same caveat")
	}
	mux, _, localRoot := newTestMuxWithLocalRoot(t)
	dir := filepath.Join(localRoot, "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "ro", "path": dir})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/storages (read-only dir) = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestStoragesPostRejectsUnknownKind covers the kind allowlist.
func TestStoragesPostRejectsUnknownKind(t *testing.T) {
	mux, _, _ := newTestMuxWithLocalRoot(t)

	rec := postStorage(t, mux, map[string]any{"kind": "nfs", "label": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/storages kind=nfs = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestStoragesPostCIFSDelegatesToShares covers the cifs-kind (default)
// path: it's a thin wrapper over handleSharesPost, so both /api/storages
// and /api/shares must see the created row.
func TestStoragesPostCIFSDelegatesToShares(t *testing.T) {
	mux, _, _ := newTestMuxWithLocalRoot(t)

	rec := postStorage(t, mux, map[string]any{"name": "Garage NAS", "server": "nas.local", "share": "footage"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/storages (cifs, kind omitted) = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Kind != db.KindCIFS {
		t.Errorf("Kind = %q, want %q", created.Kind, db.KindCIFS)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	if !strings.Contains(getRec.Body.String(), "Garage NAS") {
		t.Errorf("expected the storage created via /api/storages to show up in GET /api/shares, got: %s", getRec.Body.String())
	}

	// /api/shares stays green: still requires server/share.
	badReq := httptest.NewRequest(http.MethodPost, "/api/shares", bytes.NewReader([]byte(`{"share":"footage"}`)))
	badRec := httptest.NewRecorder()
	mux.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/shares missing server = %d, want 400 (body: %s)", badRec.Code, badRec.Body.String())
	}
}

// TestStoragesPatchLocalUpdatesPath covers PATCH for a local storage: label
// and path can be changed, and the new path is re-validated.
func TestStoragesPatchLocalUpdatesPath(t *testing.T) {
	mux, _, localRoot := newTestMuxWithLocalRoot(t)
	dir1 := filepath.Join(localRoot, "dir1")
	dir2 := filepath.Join(localRoot, "dir2")
	for _, d := range []string{dir1, dir2} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "d1", "path": dir1})
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	patchBody, _ := json.Marshal(map[string]any{"path": dir2, "label": "d2 renamed"})
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/storages/"+created.ID, bytes.NewReader(patchBody))
	patchReq.SetPathValue("id", created.ID)
	patchRec := httptest.NewRecorder()
	mux.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/storages/%s = %d, want 200 (body: %s)", created.ID, patchRec.Code, patchRec.Body.String())
	}
	var updated storageView
	if err := json.Unmarshal(patchRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if updated.Path != dir2 {
		t.Errorf("Path = %q, want %q", updated.Path, dir2)
	}
	if updated.Label != "d2 renamed" {
		t.Errorf("Label = %q, want %q", updated.Label, "d2 renamed")
	}

	// Patching to a path outside the root is rejected, unchanged on disk.
	patchBadBody, _ := json.Marshal(map[string]any{"path": "/etc"})
	patchBadReq := httptest.NewRequest(http.MethodPatch, "/api/storages/"+created.ID, bytes.NewReader(patchBadBody))
	patchBadReq.SetPathValue("id", created.ID)
	patchBadRec := httptest.NewRecorder()
	mux.ServeHTTP(patchBadRec, patchBadReq)
	if patchBadRec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH /api/storages/%s path=/etc = %d, want 400 (body: %s)", created.ID, patchBadRec.Code, patchBadRec.Body.String())
	}
}

// TestStoragesDeleteLocalNeverRemovesDirectory is the critical safety
// check: unlike a cifs share's auto-generated mount point, a local
// storage's path is the operator's own directory — deleting the storage
// registration must never touch it on disk.
func TestStoragesDeleteLocalNeverRemovesDirectory(t *testing.T) {
	mux, _, localRoot := newTestMuxWithLocalRoot(t)
	dir := filepath.Join(localRoot, "precious-data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	marker := filepath.Join(dir, "dont-delete-me.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "precious", "path": dir})
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/storages/"+created.ID, nil)
	delReq.SetPathValue("id", created.ID)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/storages/%s = %d, want 204 (body: %s)", created.ID, delRec.Code, delRec.Body.String())
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected %s to survive storage deletion, got: %v", marker, err)
	}

	views := getStorages(t, mux)
	if len(views) != 1 {
		t.Fatalf("len(views) = %d, want 1 (cache only, storage gone), got: %+v", len(views), views)
	}
}

// TestStoragesDeleteRejectsCacheID and TestStoragesScanRejectsCacheID cover
// that the synthetic "cache" id can't be targeted by mutating endpoints.
func TestStoragesDeleteRejectsCacheID(t *testing.T) {
	mux, _, _ := newTestMuxWithLocalRoot(t)

	delReq := httptest.NewRequest(http.MethodDelete, "/api/storages/cache", nil)
	delReq.SetPathValue("id", "cache")
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE /api/storages/cache = %d, want 400 (body: %s)", delRec.Code, delRec.Body.String())
	}
}

func TestStoragesScanRejectsCacheID(t *testing.T) {
	mux, _, _ := newTestMuxWithLocalRoot(t)

	scanReq := httptest.NewRequest(http.MethodPost, "/api/storages/cache/scan", nil)
	scanReq.SetPathValue("id", "cache")
	scanRec := httptest.NewRecorder()
	mux.ServeHTTP(scanRec, scanReq)
	if scanRec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/storages/cache/scan = %d, want 400 (body: %s)", scanRec.Code, scanRec.Body.String())
	}
}

// TestStoragesScanEnqueuesJobForLocalStorage covers the scan endpoint's
// happy path: the linked source (created atomically alongside the share)
// is found and a "scan" job is enqueued for it.
func TestStoragesScanEnqueuesJobForLocalStorage(t *testing.T) {
	mux, _, localRoot := newTestMuxWithLocalRoot(t)
	dir := filepath.Join(localRoot, "scan-me")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "scan-me", "path": dir})
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	scanReq := httptest.NewRequest(http.MethodPost, "/api/storages/"+created.ID+"/scan", nil)
	scanReq.SetPathValue("id", created.ID)
	scanRec := httptest.NewRecorder()
	mux.ServeHTTP(scanRec, scanReq)
	if scanRec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/storages/%s/scan = %d, want 202 (body: %s)", created.ID, scanRec.Code, scanRec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(scanRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "queued" && resp["status"] != "already_running" {
		t.Errorf("status = %v, want queued or already_running", resp["status"])
	}
}

// TestStoragesGetUsageCountsSegmentsAndExports covers the aggregation
// wiring end to end through the HTTP layer: a segment on a local storage's
// linked source and an export targeting it both show up in its usage
// totals, and the SSD is never listed.
func TestStoragesGetUsageCountsSegmentsAndExports(t *testing.T) {
	mux, store, localRoot := newTestMuxWithLocalRoot(t)
	dir := filepath.Join(localRoot, "usage-test")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "usage-test", "path": dir})
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, getReq)
	var shareViews []shareView
	if err := json.Unmarshal(getRec.Body.Bytes(), &shareViews); err != nil {
		t.Fatalf("unmarshal shares: %v", err)
	}
	if len(shareViews) != 1 || shareViews[0].SourceID == nil {
		t.Fatalf("expected linked source_id, got: %s", getRec.Body.String())
	}
	sourceID := *shareViews[0].SourceID

	ctx := context.Background()
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, bytes, has_detections, has_gps)
		 VALUES (?, 'front', 'front/x/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', 12345, 0, 0)`,
		sourceID); err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	shareIDInt, err := store.ShareGet(ctx, mustParseInt64(t, created.ID))
	if err != nil {
		t.Fatalf("ShareGet: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO exports (name, share_id, bytes) VALUES ('e1', ?, 555)`, shareIDInt.ID); err != nil {
		t.Fatalf("seed export: %v", err)
	}

	views := getStorages(t, mux)
	var found *storageView
	for i := range views {
		if views[i].ID == created.ID {
			found = &views[i]
		}
	}
	if found == nil {
		t.Fatalf("storage %s not found in %+v", created.ID, views)
	}
	if found.SegmentCount != 1 || found.SegmentBytes != 12345 {
		t.Errorf("segments = (%d, %d), want (1, 12345)", found.SegmentCount, found.SegmentBytes)
	}
	if found.ExportCount != 1 || found.ExportBytes != 555 {
		t.Errorf("exports = (%d, %d), want (1, 555)", found.ExportCount, found.ExportBytes)
	}
	if found.HashPending != 0 {
		t.Errorf("HashPending = %d, want 0 (task 6 fills this in)", found.HashPending)
	}
}

// TestStoragesHashPostEnqueuesJob covers task 6's manual trigger endpoint:
// an empty body enqueues an unscoped hash_backfill job, and a share_id
// body scopes it — both accepted with 202. An unknown share_id is
// rejected with 404 rather than silently enqueuing a job that will find
// nothing to do.
func TestStoragesHashPostEnqueuesJob(t *testing.T) {
	mux, store, localRoot := newTestMuxWithLocalRoot(t)
	dir := filepath.Join(localRoot, "hash-post-test")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := postStorage(t, mux, map[string]any{"kind": "local", "label": "hash-post-test", "path": dir})
	var created storageView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	shareID := mustParseInt64(t, created.ID)
	_ = store

	postHash := func(body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(http.MethodPost, "/api/storages/hash", nil)
		} else {
			req = httptest.NewRequest(http.MethodPost, "/api/storages/hash", strings.NewReader(body))
		}
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, req)
		return r
	}

	rec = postHash("")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("empty body: status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}

	rec = postHash(`{"share_id": ` + created.ID + `}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scoped: status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Job    struct {
			Type   string          `json:"type"`
			Params json.RawMessage `json:"params"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "queued" || resp.Job.Type != "hash_backfill" {
		t.Fatalf("got status=%q job.type=%q, want queued/hash_backfill", resp.Status, resp.Job.Type)
	}
	var params hashBackfillJobParams
	if err := json.Unmarshal(resp.Job.Params, &params); err != nil {
		t.Fatalf("decode job params: %v", err)
	}
	if params.ShareID != shareID {
		t.Errorf("enqueued job ShareID = %d, want %d", params.ShareID, shareID)
	}

	rec = postHash(`{"share_id": 999999}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown share_id: status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func mustParseInt64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse int64 %q: %v", s, err)
	}
	return n
}
