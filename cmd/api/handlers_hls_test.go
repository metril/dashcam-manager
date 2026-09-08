package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSegmentHLSFileGetRejectsInvalidFileName(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	dir := t.TempDir()
	segID := seedMediaSegment(t, store, dir, filepath.Join("front", "x", "segment_00000.mp4"))

	for _, file := range []string{"..%2F..%2Fetc%2Fpasswd", "seg1.m4s", "notreal.txt"} {
		req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/hls/"+file, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET .../hls/%s = %d, want 400 (body: %s)", file, rec.Code, rec.Body.String())
		}
	}
}

func TestSegmentHLSMasterGetUnknownSegmentIs404(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/segments/999999/hls/master.m3u8", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestSegmentHLSGetArchivedSegmentIs404 mirrors
// TestSegmentMediaGetArchivedSegmentReturns404WithShareName
// (handlers_media_test.go): an archived segment (final-fix-wave item 7)
// has no file left to transcode, so HLS generation must also refuse with
// 404 rather than trying (and failing confusingly against a missing
// source file).
func TestSegmentHLSGetArchivedSegmentIs404(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	segID := seedMediaSegment(t, store, dir, filepath.Join("front", "x", "gone.mp4"))

	var shareID int64
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('backup-nas', '10.0.0.5', 'dashcam') RETURNING id`,
	).Scan(&shareID); err != nil {
		t.Fatalf("insert share: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'front/x/gone.mp4', 5, 'deadbeef', '2026-03-01T10:05:00Z')`,
		segID, shareID); err != nil {
		t.Fatalf("insert ingests row: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`UPDATE segments SET archived_at = '2026-03-01T10:06:00Z' WHERE id = ?`, segID); err != nil {
		t.Fatalf("archive segment: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/hls/master.m3u8", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}
