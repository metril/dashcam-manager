package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dashcam/internal/db"
)

// seedDuplicatesFixture inserts two storage_files rows sharing a sha256 on
// two distinct (local) shares — a verified duplicate group once refreshed.
func seedDuplicatesFixture(t *testing.T, store *db.DB) (shareA, shareB int64) {
	t.Helper()
	ctx := context.Background()

	shareA, err := store.ShareCreate(ctx, db.ShareCreateParams{Name: "a", Kind: db.KindLocal, MountPoint: "/mnt/shares/a", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate a: %v", err)
	}
	shareB, err = store.ShareCreate(ctx, db.ShareCreateParams{Name: "b", Kind: db.KindLocal, MountPoint: "/mnt/shares/b", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate b: %v", err)
	}

	must := func(query string, args ...any) {
		if _, err := store.Write().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
	}
	must(`INSERT INTO storage_files (share_id, rel_path, bytes, mtime, sha256, hashed_at, first_seen_at, last_seen_at)
		VALUES (?, 'clip.mp4', 1000, 0, 'deadbeef', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, shareA)
	must(`INSERT INTO storage_files (share_id, rel_path, bytes, mtime, sha256, hashed_at, first_seen_at, last_seen_at)
		VALUES (?, 'clip.mp4', 1000, 0, 'deadbeef', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, shareB)
	return shareA, shareB
}

func TestHandleDuplicatesGetReturnsRefreshedGroups(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedDuplicatesFixture(t, store)

	if err := store.DuplicateReportRefresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/duplicates", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Groups []db.DuplicateGroup `json:"groups"`
		Total  int64               `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || len(body.Groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %s", body.Total, len(body.Groups), rec.Body.String())
	}
	if body.Groups[0].Kind != db.DuplicateGroupKindSHA256 {
		t.Errorf("kind = %q, want sha256", body.Groups[0].Kind)
	}
}

func TestHandleDuplicatesGetFiltersByKindAndStorage(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	shareA, _ := seedDuplicatesFixture(t, store)
	if err := store.DuplicateReportRefresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/duplicates?kind=size_name", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if empty.Total != 0 {
		t.Errorf("kind=size_name total = %d, want 0 (fixture is a verified sha256 group)", empty.Total)
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/duplicates?kind=sha256", nil))
	var got struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 {
		t.Errorf("kind=sha256 total = %d, want 1", got.Total)
	}

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/api/duplicates?storage_id="+itoa(shareA), nil)
	mux.ServeHTTP(rec3, req3)
	var byStorage struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &byStorage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if byStorage.Total != 1 {
		t.Errorf("storage_id filter total = %d, want 1", byStorage.Total)
	}
}

func TestHandleDuplicatesGetRejectsInvalidParams(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)

	cases := []string{
		"/api/duplicates?kind=bogus",
		"/api/duplicates?storage_id=abc",
		"/api/duplicates?storage_id=0",
		"/api/duplicates?limit=0",
		"/api/duplicates?offset=-1",
	}
	for _, path := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleDuplicatesRefreshPostRebuildsReport(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedDuplicatesFixture(t, store)

	// No refresh yet — the report should be empty.
	rec0 := httptest.NewRecorder()
	mux.ServeHTTP(rec0, httptest.NewRequest(http.MethodGet, "/api/duplicates", nil))
	var before struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec0.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.Total != 0 {
		t.Fatalf("total before refresh = %d, want 0", before.Total)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/duplicates/refresh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/duplicates", nil))
	var after struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Total != 1 {
		t.Fatalf("total after refresh = %d, want 1", after.Total)
	}
}
