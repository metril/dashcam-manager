package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/maintenance"
)

// seedMaintenanceFixture inserts one 'ssd'-kind source (root_path a real
// temp dir, so os.Stat succeeds, but never actually mounted — checkSources
// reports it unavailable unless a test calls makeSourceAvailable) with one
// segment whose file exists (segID) and one whose file is missing with an
// old mtime (missingSegID, confirmed-missing by Prune's recency filter).
func seedMaintenanceFixture(t *testing.T, store *db.DB) (root string, segID, missingSegID int64) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()

	must := func(query string, args ...any) int64 {
		res, err := store.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', ?, 'SSD')`, root)
	segID = must(`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, mtime)
		VALUES (?, 'front', 'ok.mp4', 0, '2026-03-01T10:00:00Z', 1600000000)`, sourceID)
	if err := os.WriteFile(filepath.Join(root, "ok.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write ok.mp4: %v", err)
	}
	missingSegID = must(`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, mtime)
		VALUES (?, 'front', 'gone.mp4', 1, '2026-03-01T10:00:00Z', 1600000000)`, sourceID)

	return root, segID, missingSegID
}

// makeSourceAvailable stubs internal/maintenance's mount-detection hook
// (SetIsMountedFnForTest, an exported test-only seam — isMountedFn itself
// is unexported and only stubbable from within that package) to report
// the fixture's 'ssd'-kind source as mounted, restored via t.Cleanup. Kept
// as the default availability helper so these HTTP-level tests exercise
// the ordinary ssd-source path; see makeLocalShareSourceAvailable for the
// kind=local half of the same guard.
func makeSourceAvailable(t *testing.T) {
	t.Helper()
	t.Cleanup(maintenance.SetIsMountedFnForTest(func(string) (bool, error) { return true, nil }))
}

// makeLocalShareSourceAvailable rewires the fixture's single seeded source
// onto a freshly-created kind=local share (db.KindLocal) whose mount_point
// is the source's own root_path. checkSources (internal/maintenance/sources.go)
// treats a share-kind source backed by a kind=local share as available
// without requiring a real mount or write probe (share.LocalDirReadable) —
// exactly what the removed hidden allow_local_share_paths setting used to
// fake, wholesale, for every source before schema v9 made local storage
// first-class.
func makeLocalShareSourceAvailable(t *testing.T, store *db.DB) {
	t.Helper()
	ctx := context.Background()

	var sourceID int64
	var rootPath string
	if err := store.Read().QueryRowContext(ctx, `SELECT id, root_path FROM sources`).Scan(&sourceID, &rootPath); err != nil {
		t.Fatalf("query seeded source: %v", err)
	}

	var shareID int64
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share, mount_point, kind) VALUES ('local-test', '', '', ?, 'local') RETURNING id`,
		rootPath).Scan(&shareID); err != nil {
		t.Fatalf("insert local share: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`UPDATE sources SET kind = 'share', share_id = ? WHERE id = ?`, shareID, sourceID); err != nil {
		t.Fatalf("rewire source onto local share: %v", err)
	}
}

// --- POST /api/maintenance/thumbs/regenerate --------------------------------

func TestMaintenanceThumbsRegenerateAllForcesJob(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store, _ := newTestMuxWithStoreAndQueueNoWorkers(t)
	seedMaintenanceFixture(t, store)

	body, _ := json.Marshal(map[string]any{"scope": "all"})
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/thumbs/regenerate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}

	var resp struct {
		Job struct {
			Type   string          `json:"type"`
			Params json.RawMessage `json:"params"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Job.Type != "thumbs" {
		t.Fatalf("job type = %q, want thumbs", resp.Job.Type)
	}
	var params thumbsJobParams
	if err := json.Unmarshal(resp.Job.Params, &params); err != nil {
		t.Fatalf("decode job params: %v", err)
	}
	if !params.Force {
		t.Errorf("Force = false, want true for a regenerate-triggered thumbs job")
	}
}

func TestMaintenanceThumbsRegenerateSegmentScope(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store, _ := newTestMuxWithStoreAndQueueNoWorkers(t)
	_, segID, _ := seedMaintenanceFixture(t, store)

	body, _ := json.Marshal(map[string]any{"scope": "segment", "id": itoa(segID)})
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/thumbs/regenerate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}

	var resp struct {
		Job struct {
			Params json.RawMessage `json:"params"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var params thumbsJobParams
	if err := json.Unmarshal(resp.Job.Params, &params); err != nil {
		t.Fatalf("decode job params: %v", err)
	}
	if len(params.SegmentIDs) != 1 || params.SegmentIDs[0] != segID {
		t.Errorf("SegmentIDs = %v, want [%d]", params.SegmentIDs, segID)
	}
}

func TestMaintenanceThumbsRegenerateInvalidScope(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store, _ := newTestMuxWithStoreAndQueueNoWorkers(t)
	seedMaintenanceFixture(t, store)

	body, _ := json.Marshal(map[string]any{"scope": "bogus"})
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/thumbs/regenerate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

// --- POST /api/maintenance/db/verify + GET /api/maintenance/report ---------

func TestMaintenanceReportNotFoundBeforeVerify(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/maintenance/report", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body)
	}
}

func TestMaintenanceDBVerifyRunsAndReportIsServed(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store := newTestMuxWithStore(t)
	_, _, missingSegID := seedMaintenanceFixture(t, store)
	makeSourceAvailable(t)

	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/db/verify", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	final := pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)
	if final.State != jobs.StateDone {
		t.Fatalf("db_verify job state = %q, want done, error=%q", final.State, final.Error)
	}

	var report struct {
		MissingMediaSegmentIDs []int64  `json:"missing_media_segment_ids"`
		SourcesUnavailable     []string `json:"sources_unavailable"`
	}
	getRec := getJSON(t, mux, "/api/maintenance/report", &report)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET report status = %d, want 200, body=%s", getRec.Code, getRec.Body)
	}
	if len(report.SourcesUnavailable) != 0 {
		t.Errorf("SourcesUnavailable = %v, want empty (isMountedFn stubbed to report the ssd source mounted)", report.SourcesUnavailable)
	}
	found := false
	for _, id := range report.MissingMediaSegmentIDs {
		if id == missingSegID {
			found = true
		}
	}
	if !found {
		t.Errorf("MissingMediaSegmentIDs = %v, want to include %d", report.MissingMediaSegmentIDs, missingSegID)
	}
}

// TestMaintenanceDBVerifyRunsWithLocalKindShareSource is the kind=local
// counterpart to TestMaintenanceDBVerifyRunsAndReportIsServed above: a
// "share"-kind source backed by a kind=local share must also be reported
// available (via share.LocalDirReadable, not a real mount or isMountedFn
// stub), covering checkSources' other availability path at the HTTP/job
// layer.
func TestMaintenanceDBVerifyRunsWithLocalKindShareSource(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store := newTestMuxWithStore(t)
	_, _, missingSegID := seedMaintenanceFixture(t, store)
	makeLocalShareSourceAvailable(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/db/verify", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	final := pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)
	if final.State != jobs.StateDone {
		t.Fatalf("db_verify job state = %q, want done, error=%q", final.State, final.Error)
	}

	var report struct {
		MissingMediaSegmentIDs []int64  `json:"missing_media_segment_ids"`
		SourcesUnavailable     []string `json:"sources_unavailable"`
	}
	getRec := getJSON(t, mux, "/api/maintenance/report", &report)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET report status = %d, want 200, body=%s", getRec.Code, getRec.Body)
	}
	if len(report.SourcesUnavailable) != 0 {
		t.Errorf("SourcesUnavailable = %v, want empty (source is share-backed by a kind=local share)", report.SourcesUnavailable)
	}
	found := false
	for _, id := range report.MissingMediaSegmentIDs {
		if id == missingSegID {
			found = true
		}
	}
	if !found {
		t.Errorf("MissingMediaSegmentIDs = %v, want to include %d", report.MissingMediaSegmentIDs, missingSegID)
	}
}

// --- POST /api/maintenance/db/prune -----------------------------------------

func TestMaintenanceDBPruneEmptyBodyIsDryRun(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store := newTestMuxWithStore(t)
	_, _, missingSegID := seedMaintenanceFixture(t, store)
	makeSourceAvailable(t)

	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/db/prune", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	final := pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)
	if final.State != jobs.StateDone {
		t.Fatalf("db_prune job state = %q, want done, error=%q", final.State, final.Error)
	}
	var result struct {
		DryRun          bool  `json:"dry_run"`
		SegmentsRemoved int64 `json:"segments_removed"`
	}
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.DryRun {
		t.Errorf("DryRun = false for an empty request body, want true (CRITICAL 2: empty body must default to a dry run)")
	}

	var n int
	if err := store.Read().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM segments WHERE id = ?`, missingSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 1 {
		t.Errorf("empty-body prune deleted segment %d", missingSegID)
	}
}

func TestMaintenanceDBPruneApplyTrueDeletes(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store := newTestMuxWithStore(t)
	_, _, missingSegID := seedMaintenanceFixture(t, store)
	makeSourceAvailable(t)

	body, _ := json.Marshal(map[string]any{"apply": true, "force": true})
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/db/prune", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	final := pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)
	if final.State != jobs.StateDone {
		t.Fatalf("db_prune job state = %q, want done, error=%q", final.State, final.Error)
	}

	var n int
	if err := store.Read().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM segments WHERE id = ?`, missingSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 0 {
		t.Errorf("apply:true,force:true prune left segment %d in place", missingSegID)
	}
}

// TestMaintenanceDBPruneRefusesWhenSourceUnavailable is CRITICAL 1's
// coverage at the HTTP/job layer: without makeSourceAvailable, the
// fixture's plain-temp-dir 'ssd' source has no /proc/mounts entry, so Prune
// must refuse (the job fails, nothing is deleted) even with force:true.
func TestMaintenanceDBPruneRefusesWhenSourceUnavailable(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, store := newTestMuxWithStore(t)
	_, _, missingSegID := seedMaintenanceFixture(t, store)

	body, _ := json.Marshal(map[string]any{"apply": true, "force": true})
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/db/prune", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	final := pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)
	if final.State != jobs.StateFailed {
		t.Fatalf("db_prune job state = %q, want failed (source not mounted)", final.State)
	}
	if !strings.Contains(final.Error, "not mounted") && !strings.Contains(final.Error, "unavailable") {
		t.Errorf("job error = %q, want it to mention the source being unmounted/unavailable", final.Error)
	}

	var n int
	if err := store.Read().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM segments WHERE id = ?`, missingSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 1 {
		t.Errorf("segment %d was deleted despite the source being unavailable", missingSegID)
	}
}
