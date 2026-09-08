package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dashcam/internal/media"
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

// TestSegmentHLSCacheControlHeaders covers residual-findings item 3's
// cache-control half: the playlist must be short-cached (max-age=60, so a
// player re-fetching it can notice a regenerated/fixed playlist relatively
// soon) while a media segment (.m4s/init.mp4) gets a long max-age but
// without "immutable" (a cache-clear + regeneration under the same URL
// could, in principle, produce different bytes; "immutable" would tell a
// browser never to revalidate even on a user-initiated refresh). Pre-seeds
// the HLS cache directory directly (matching what a real GenerateHLS run
// would produce) so this doesn't need real ffmpeg — ensureHLS's
// fileExists(masterPath) short-circuit skips generation entirely once the
// master playlist is already present.
func TestSegmentHLSCacheControlHeaders(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DASHCAM_CACHE", cacheDir)
	mux, store := newTestMuxWithStore(t)
	dir := t.TempDir()
	segID := seedMediaSegment(t, store, dir, filepath.Join("front", "x", "segment_00000.mp4"))

	hlsDir := media.HLSDir(cacheDir, segID)
	if err := os.MkdirAll(hlsDir, 0o755); err != nil {
		t.Fatalf("mkdir hls dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, media.HLSMasterName), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatalf("write master.m3u8: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, media.HLSInitName), []byte("fake-init"), 0o644); err != nil {
		t.Fatalf("write init.mp4: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, "seg000.m4s"), []byte("fake-segment"), 0o644); err != nil {
		t.Fatalf("write seg000.m4s: %v", err)
	}

	masterReq := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/hls/master.m3u8", nil)
	masterRec := httptest.NewRecorder()
	mux.ServeHTTP(masterRec, masterReq)
	if masterRec.Code != http.StatusOK {
		t.Fatalf("master.m3u8 status = %d, body=%s", masterRec.Code, masterRec.Body.String())
	}
	if cc := masterRec.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("master.m3u8 Cache-Control = %q, want %q", cc, "public, max-age=60")
	}

	segReq := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/hls/seg000.m4s", nil)
	segRec := httptest.NewRecorder()
	mux.ServeHTTP(segRec, segReq)
	if segRec.Code != http.StatusOK {
		t.Fatalf("seg000.m4s status = %d, body=%s", segRec.Code, segRec.Body.String())
	}
	segCC := segRec.Header().Get("Cache-Control")
	if !strings.Contains(segCC, "max-age=31536000") {
		t.Errorf("seg000.m4s Cache-Control = %q, want it to contain max-age=31536000", segCC)
	}
	if strings.Contains(segCC, "immutable") {
		t.Errorf("seg000.m4s Cache-Control = %q, must not contain immutable", segCC)
	}
}
