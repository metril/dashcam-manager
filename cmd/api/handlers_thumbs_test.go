package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"dashcam/internal/media"
)

func TestSegmentThumbGetServesCachedFile(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DASHCAM_CACHE", cacheDir)
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	out := media.ThumbPath(cacheDir, frontSegID)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fakeJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'f', 'a', 'k', 'e'}
	if err := os.WriteFile(out, fakeJPEG, 0o644); err != nil {
		t.Fatalf("write fake thumb: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(frontSegID)+"/thumb.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != string(fakeJPEG) {
		t.Errorf("body = %q, want cached fixture bytes", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc == "" {
		t.Errorf("Cache-Control header missing")
	}
}

func TestSegmentThumbGetUnknownSegment404(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	mux, _ := newTestMuxWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/segments/999999/thumb.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestSegmentThumbGetGenerationFailureMarksFailed exercises the on-demand
// generation path when ffmpeg can't produce a thumbnail (here: pointed at
// a nonexistent binary, which is also what happens on a host with no
// ffmpeg installed) — it must 404 rather than hang or panic, and persist
// thumb_status='failed' rather than leaving it at the default 'pending'.
func TestSegmentThumbGetGenerationFailureMarksFailed(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	t.Setenv("DASHCAM_FFMPEG", "dashcam-manager-test-nonexistent-ffmpeg-binary")
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(frontSegID)+"/thumb.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body)
	}

	seg, err := store.SegmentThumbInfoGet(req.Context(), frontSegID)
	if err != nil {
		t.Fatalf("SegmentThumbInfoGet: %v", err)
	}
	_ = seg // thumb_status isn't exposed on ThumbSegment; check via raw query below.

	var status string
	if err := store.Read().QueryRow(`SELECT thumb_status FROM segments WHERE id = ?`, frontSegID).Scan(&status); err != nil {
		t.Fatalf("query thumb_status: %v", err)
	}
	if status != "failed" {
		t.Errorf("thumb_status = %q, want failed", status)
	}
}

func TestSegmentSpriteVTTServesCachedFileWithContentType(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DASHCAM_CACHE", cacheDir)
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	jpg, vtt := media.SpritePaths(cacheDir, frontSegID)
	if err := os.MkdirAll(filepath.Dir(vtt), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(jpg, []byte("jpg"), 0o644); err != nil {
		t.Fatalf("write fake sprite jpg: %v", err)
	}
	vttBody := "WEBVTT\n\n00:00:00.000 --> 00:00:03.000\nsprite.jpg#xywh=0,0,320,180\n\n"
	if err := os.WriteFile(vtt, []byte(vttBody), 0o644); err != nil {
		t.Fatalf("write fake sprite vtt: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(frontSegID)+"/sprite.vtt", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != vttBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), vttBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/vtt" {
		t.Errorf("Content-Type = %q, want text/vtt", ct)
	}
}
