package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dashcam/internal/db"
)

// seedNeedsTracksBackfill inserts a minimal source/segment/detection fixture
// that db.SegmentsNeedingTracksBackfill reports as needing backfill
// (tracks_built_at NULL, one det_objects row with a NULL track_id).
func seedNeedsTracksBackfill(t *testing.T, store *db.DB) (segID int64) {
	t.Helper()
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := store.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	segID = must(`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, has_detections)
		VALUES (?, 'front', 'front/a/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', 1)`, sourceID)
	detID := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 0)`, segID)
	must(`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2) VALUES (?, 'car', 0.9, 0, 0, 1, 1)`, detID)
	return segID
}

// TestTracksBackfillPostEnqueuesWithoutSegmentIDs covers fix round 1:
// handleTracksBackfillPost used to resolve the full needing-backfill segment
// list and embed it in the enqueued job's params, duplicating work
// tracksBackfillJobHandler already does (it re-resolves whenever
// SegmentIDs is empty — see jobs_runtime.go). The handler's pre-check
// should only be used for the response's count, not for what's actually
// enqueued.
func TestTracksBackfillPostEnqueuesWithoutSegmentIDs(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedNeedsTracksBackfill(t, store)

	req := httptest.NewRequest(http.MethodPost, "/api/tracks/backfill", nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/tracks/backfill = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
		Job    struct {
			Params json.RawMessage `json:"params"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "queued" || resp.Count != 1 {
		t.Fatalf("got status=%q count=%d, want queued/1", resp.Status, resp.Count)
	}

	var params tracksBackfillJobParams
	if len(resp.Job.Params) > 0 {
		if err := json.Unmarshal(resp.Job.Params, &params); err != nil {
			t.Fatalf("decode job params: %v", err)
		}
	}
	if len(params.SegmentIDs) != 0 {
		t.Errorf("enqueued job params.SegmentIDs = %v, want empty (job handler resolves it itself)", params.SegmentIDs)
	}
}

// TestTracksBackfillPostNoopWhenNothingNeeded covers the zero-segments
// short-circuit: no job should be enqueued, and the response should say so
// with a 200.
func TestTracksBackfillPostNoopWhenNothingNeeded(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)

	req := httptest.NewRequest(http.MethodPost, "/api/tracks/backfill", nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/tracks/backfill (nothing to do) = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "noop" || resp.Count != 0 {
		t.Fatalf("got status=%q count=%d, want noop/0", resp.Status, resp.Count)
	}
}
