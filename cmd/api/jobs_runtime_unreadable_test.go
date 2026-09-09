package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/media"
)

// writeFakeFFmpegUnreadable writes a fake ffmpeg that always exits
// non-zero with a stderr line matching one of media's
// unreadableStderrPatterns ("moov atom not found") — standing in for a
// genuinely corrupt file, regardless of hw/software mode.
func writeFakeFFmpegUnreadable(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-ffmpeg-unreadable.sh")
	body := "#!/bin/sh\necho 'moov atom not found' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return script
}

// TestGenerateSegmentThumbnailSkipsMarkUnreadableWhenInputMissing covers
// final-findings item 1: an input file that has vanished (SSD unplugged
// mid-copy, ENOENT) must not be marked unreadable — that would
// permanently stop retrying a file that may well come back once the SSD
// is remounted — it should just come back as a normal 'failed' result.
func TestGenerateSegmentThumbnailSkipsMarkUnreadableWhenInputMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	t.Setenv("DASHCAM_FFMPEG", writeFakeFFmpegUnreadable(t, t.TempDir()))

	_, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	seg := db.ThumbSegment{ID: frontSegID, RootPath: t.TempDir(), RelPath: "does-not-exist.mp4", DurationS: floatPtr(30)}
	result := generateSegmentThumbnail(context.Background(), store, t.TempDir(), "", media.ModeSoftware, seg, nil)
	if result != thumbResultFailed {
		t.Fatalf("result = %v, want thumbResultFailed (missing input must not be marked unreadable)", result)
	}
	var reason *string
	var status string
	if err := store.Read().QueryRow(`SELECT unreadable_reason, thumb_status FROM segments WHERE id = ?`, frontSegID).Scan(&reason, &status); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if reason != nil {
		t.Errorf("unreadable_reason = %q, want NULL", *reason)
	}
	if status != "failed" {
		t.Errorf("thumb_status = %q, want failed", status)
	}
}

// TestGenerateSegmentThumbnailSkipsMarkUnreadableWhenFileFresh covers the
// "still copying" half of item 1: a 0-byte-or-fresh file that ffmpeg can't
// yet decode (still being written) must not be marked unreadable either.
func TestGenerateSegmentThumbnailSkipsMarkUnreadableWhenFileFresh(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	t.Setenv("DASHCAM_FFMPEG", writeFakeFFmpegUnreadable(t, t.TempDir()))

	_, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	dir := t.TempDir()
	in := filepath.Join(dir, "fresh.mp4")
	if err := os.WriteFile(in, []byte("partial-bytes"), 0o644); err != nil {
		t.Fatalf("write fresh input: %v", err)
	}

	seg := db.ThumbSegment{ID: frontSegID, RootPath: dir, RelPath: "fresh.mp4", DurationS: floatPtr(30)}
	result := generateSegmentThumbnail(context.Background(), store, t.TempDir(), "", media.ModeSoftware, seg, nil)
	if result != thumbResultFailed {
		t.Fatalf("result = %v, want thumbResultFailed (fresh/mid-copy input must not be marked unreadable)", result)
	}
	var reason *string
	if err := store.Read().QueryRow(`SELECT unreadable_reason FROM segments WHERE id = ?`, frontSegID).Scan(&reason); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if reason != nil {
		t.Errorf("unreadable_reason = %q, want NULL", *reason)
	}
}

// TestGenerateSegmentThumbnailMarksUnreadableWhenGenuine covers the
// still-working case: an old, non-empty, genuinely-corrupt file (mtime
// well outside the copy-in-progress window) must still be marked
// unreadable, same as before item 1's guard was added.
func TestGenerateSegmentThumbnailMarksUnreadableWhenGenuine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	t.Setenv("DASHCAM_FFMPEG", writeFakeFFmpegUnreadable(t, t.TempDir()))

	_, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	dir := t.TempDir()
	in := filepath.Join(dir, "old.mp4")
	if err := os.WriteFile(in, []byte("corrupt-bytes"), 0o644); err != nil {
		t.Fatalf("write old input: %v", err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(in, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	seg := db.ThumbSegment{ID: frontSegID, RootPath: dir, RelPath: "old.mp4", DurationS: floatPtr(30)}
	result := generateSegmentThumbnail(context.Background(), store, t.TempDir(), "", media.ModeSoftware, seg, nil)
	if result != thumbResultUnreadable {
		t.Fatalf("result = %v, want thumbResultUnreadable (genuinely corrupt, stable file)", result)
	}
	var reason *string
	if err := store.Read().QueryRow(`SELECT unreadable_reason FROM segments WHERE id = ?`, frontSegID).Scan(&reason); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if reason == nil || *reason != "Missing moov atom" {
		t.Errorf("unreadable_reason = %v, want \"Missing moov atom\"", reason)
	}
}

func floatPtr(f float64) *float64 { return &f }
