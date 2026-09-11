package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

// TestSegmentThumbGetGenerationFailureIsServerErrorWhenSourceExists covers
// M8 review finding #5: a 404 on generation failure should only mean
// "there's genuinely nothing to show" (unknown segment, or the source
// video is gone from disk) — an existing source file that ffmpeg simply
// failed to encode (missing/broken ffmpeg here; a corrupt file in
// production) is a server-side problem and must 500, not 404 (which
// would otherwise be indistinguishable from "this thumbnail will never
// exist").
func TestSegmentThumbGetGenerationFailureIsServerErrorWhenSourceExists(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DASHCAM_CACHE", cacheDir)
	t.Setenv("DASHCAM_FFMPEG", "dashcam-manager-test-nonexistent-ffmpeg-binary")
	mux, store := newTestMuxWithStore(t)

	// Unlike seedTrip (which points at the real, read-only /mnt/dashcam —
	// this test must never write there), seed a source rooted at a temp
	// dir with a real (if fake-content) file, so it exercises the "source
	// exists but ffmpeg failed" branch rather than the "source missing"
	// branch already covered by TestSegmentThumbGetGenerationFailureMarksFailed.
	dir := t.TempDir()
	relPath := filepath.Join("front", "x", "segment_00000.mp4")
	if err := os.MkdirAll(filepath.Join(dir, "front", "x"), 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, relPath), []byte("not a real video"), 0o644); err != nil {
		t.Fatalf("write fake source file: %v", err)
	}
	frontSegID := seedMediaSegment(t, store, dir, relPath)

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(frontSegID)+"/thumb.jpg", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body)
	}

	var status string
	if err := store.Read().QueryRow(`SELECT thumb_status FROM segments WHERE id = ?`, frontSegID).Scan(&status); err != nil {
		t.Fatalf("query thumb_status: %v", err)
	}
	if status != "failed" {
		t.Errorf("thumb_status = %q, want failed", status)
	}
}

// TestSegmentThumbGetConcurrentRequestsCoalesce covers M8 review finding
// #2: concurrent on-demand requests for the same missing thumbnail must
// not race to write the same temp file / duplicate ffmpeg work — they
// should coalesce (via genGroup) into a single generation, all seeing the
// same (successful) result.
func TestSegmentThumbGetConcurrentRequestsCoalesce(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DASHCAM_CACHE", cacheDir)
	fakeDir := t.TempDir()
	t.Setenv("DASHCAM_FFMPEG", writeFakeFFmpegAPI(t, fakeDir))
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(frontSegID)+"/thumb.jpg", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}

	out := media.ThumbPath(cacheDir, frontSegID)
	if !fileExists(out) {
		t.Errorf("thumbnail was not written to %q", out)
	}
	// No leftover temp files: every generation's tempOutputPath either got
	// renamed into place or cleaned up by its defer os.Remove.
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(out) {
			t.Errorf("unexpected leftover file in cache dir: %q", e.Name())
		}
	}
}

// writeFakeFFmpegAPI is the cmd/api-package twin of internal/media's
// writeFakeFFmpeg test helper: a script that writes a non-empty file at
// its last argument instead of actually encoding anything. Non-empty
// matters: media.runFFmpegExpectOutput treats a 0-byte output as a
// failure (real ffmpeg can exit 0 while encoding nothing), so a bare
// "touch" would make every caller of this helper look like that failure
// mode instead of a clean success.
func writeFakeFFmpegAPI(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ffmpeg.sh")
	// Only write when the last arg looks like a path: capability probes
	// (`ffmpeg -encoders`, `ffmpeg -hwaccels`) would otherwise leave a
	// file literally named "-encoders" in the package directory.
	script := "#!/bin/sh\neval out=\\${$#}\ncase \"$out\" in\n */*) echo fake-jpeg-bytes > \"$out\" ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
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
