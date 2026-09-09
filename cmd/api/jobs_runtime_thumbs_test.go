package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// TestThumbsJobRunsAndReportsWithoutFFmpeg exercises the "thumbs" job
// end-to-end through the real queue (no ffmpeg on this host, so every
// segment fails — the point of this test is that the job still completes
// cleanly, marks each segment 'failed' rather than leaving it 'pending',
// and reports a sane total/done/failed result, matching the "no ffmpeg
// available" branch of the M8 brief's fallback requirement).
func TestThumbsJobRunsAndReportsWithoutFFmpeg(t *testing.T) {
	t.Setenv("DASHCAM_CACHE", t.TempDir())
	t.Setenv("DASHCAM_FFMPEG", "dashcam-manager-test-nonexistent-ffmpeg-binary")

	mux, store := newTestMuxWithStore(t)
	_ = mux
	_, frontSegID, rearSegID := seedTrip(t, store, "2026-03-01T10:00:00Z")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	sourceID := mustSourceIDForSegment(t, store, frontSegID)

	job, err := queue.Enqueue(ctx, "thumbs", map[string]any{"source_id": sourceID}, 0)
	if err != nil {
		t.Fatalf("enqueue thumbs: %v", err)
	}

	final := waitForJobTerminal(t, queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (job itself succeeds even if every segment's thumbnail failed); error=%s", final.State, final.Error)
	}

	var result map[string]int
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", final.Result, err)
	}
	if result["total"] != 2 {
		t.Errorf("total = %d, want 2", result["total"])
	}
	if result["failed"] != 2 {
		t.Errorf("failed = %d, want 2 (no ffmpeg on test host): %+v", result["failed"], result)
	}

	for _, segID := range []int64{frontSegID, rearSegID} {
		var status string
		if err := store.Read().QueryRow(`SELECT thumb_status FROM segments WHERE id = ?`, segID).Scan(&status); err != nil {
			t.Fatalf("query thumb_status %d: %v", segID, err)
		}
		if status != "failed" {
			t.Errorf("segment %d thumb_status = %q, want failed", segID, status)
		}
	}
}

// TestThumbsJobRunsThumbnailsBeforeSprites covers the WP2 brief's two-pass
// job structure: every segment's thumbnail must be generated (so the UI
// grid, which only needs thumbnails, can populate immediately) before any
// segment's sprite generation starts, even though both run under the same
// acquireFFmpegSlot concurrency limit.
func TestThumbsJobRunsThumbnailsBeforeSprites(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	t.Setenv("DASHCAM_CACHE", t.TempDir())

	scriptDir := t.TempDir()
	callLog := filepath.Join(scriptDir, "calls.log")
	fakeFFmpeg := filepath.Join(scriptDir, "fake-ffmpeg.sh")
	// Logs every invocation's args (one per line) then, if the last arg
	// looks like an output path (contains "/"), writes a non-empty file
	// there so Thumbnail/Sprite see a "successful" run — non-empty
	// matters, since media.runFFmpegExpectOutput now treats a 0-byte
	// output as a failure (real ffmpeg can exit 0 while encoding
	// nothing). The "-hwaccels" probe call (single arg, no slash) is
	// skipped so it doesn't touch a stray file.
	body := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nif [ $# -gt 0 ]; then\n eval out=\\${$#}\n case \"$out\" in\n  */*) echo fake-jpeg-bytes > \"$out\" ;;\n esac\nfi\n", callLog)
	if err := os.WriteFile(fakeFFmpeg, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)
	// No real ffprobe available: the sprite pass's keyframe-count probe
	// fails and falls back to seek-extracts, which still go through the
	// fake ffmpeg above — irrelevant to the ordering this test checks.
	t.Setenv("DASHCAM_FFPROBE", "dashcam-manager-test-nonexistent-ffprobe-binary")

	mux, store := newTestMuxWithStore(t)
	_ = mux
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	sourceID := mustSourceIDForSegment(t, store, frontSegID)
	job, err := queue.Enqueue(ctx, "thumbs", map[string]any{"source_id": sourceID}, 0)
	if err != nil {
		t.Fatalf("enqueue thumbs: %v", err)
	}

	final := waitForJobTerminal(t, queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}
	var result map[string]int
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", final.Result, err)
	}
	if result["done"] != 2 {
		t.Fatalf("done = %d, want 2 (fake ffmpeg always succeeds): %+v", result["done"], result)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) == 0 {
		t.Fatal("no ffmpeg invocations logged")
	}

	lastThumbIdx, firstSpriteIdx := -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "/thumbs/"):
			lastThumbIdx = i
		case strings.Contains(l, "/sprites/") && firstSpriteIdx == -1:
			firstSpriteIdx = i
		}
	}
	if lastThumbIdx == -1 {
		t.Fatal("no thumbnail invocations logged")
	}
	if firstSpriteIdx == -1 {
		t.Fatal("no sprite invocations logged")
	}
	if firstSpriteIdx < lastThumbIdx {
		t.Errorf("a sprite invocation (line %d) ran before the last thumbnail invocation (line %d): two-pass ordering broken\n%s", firstSpriteIdx, lastThumbIdx, calls)
	}
}

// TestThumbsJobFinalMessageDoneTotalFailedOrder covers final-findings item
// 3: the final report message's Sprintf args were misordered
// ("%d/%d done, %d failed" fed doneCount, failedCount, total instead of
// doneCount, total, failedCount). Uses 3 segments with a done/failed/
// total split (1/3 done, 2 failed) chosen so failedCount != total —
// otherwise the swapped-args bug would print an identical string by
// coincidence and this test wouldn't catch it.
func TestThumbsJobFinalMessageDoneTotalFailedOrder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	t.Setenv("DASHCAM_CACHE", t.TempDir())

	scriptDir := t.TempDir()
	fakeFFmpeg := filepath.Join(scriptDir, "fake-ffmpeg.sh")
	// Succeeds (writes a real output file) only for the segment whose
	// rel_path contains "good"; every other invocation fails outright
	// with a non-"unreadable" stderr line, so it lands in thumbResultFailed
	// rather than thumbResultUnreadable.
	body := "#!/bin/sh\ncase \"$*\" in\n *good*) eval out=\\${$#}; echo fake-jpeg-bytes > \"$out\" ;;\n *) echo 'transient failure' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(fakeFFmpeg, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	t.Setenv("DASHCAM_FFMPEG", fakeFFmpeg)

	_, store := newTestMuxWithStore(t)
	sourceID, err := store.Write().ExecContext(context.Background(),
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	srcID, _ := sourceID.LastInsertId()

	insertSeg := func(relPath string) int64 {
		res, err := store.Write().ExecContext(context.Background(), `
			INSERT INTO segments (source_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
			VALUES (?, 'front', ?, 0, '2026-03-01T10:00:00Z', '2026-03-01T10:00:30Z', 30, 1000, 0, 0)
		`, srcID, relPath)
		if err != nil {
			t.Fatalf("insert segment %s: %v", relPath, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	insertSeg("front/good/segment_00000.mp4")
	insertSeg("front/bad1/segment_00000.mp4")
	insertSeg("front/bad2/segment_00000.mp4")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := jobs.New(store)
	registerJobHandlers(queue, store)
	if err := queue.Start(ctx, 2); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}

	job, err := queue.Enqueue(ctx, "thumbs", map[string]any{"source_id": srcID}, 0)
	if err != nil {
		t.Fatalf("enqueue thumbs: %v", err)
	}
	final := waitForJobTerminal(t, queue, job.ID, 10*time.Second)
	if final.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done; error=%s", final.State, final.Error)
	}

	var result map[string]int
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", final.Result, err)
	}
	if result["done"] != 1 || result["total"] != 3 || result["failed"] != 2 {
		t.Fatalf("done/total/failed = %d/%d/%d, want 1/3/2: %+v", result["done"], result["total"], result["failed"], result)
	}

	if !strings.Contains(final.Message, "1/3 done") {
		t.Errorf("final message = %q, want to contain \"1/3 done\" (doneCount/total, not doneCount/failedCount)", final.Message)
	}
	if !strings.Contains(final.Message, "2 failed") {
		t.Errorf("final message = %q, want to contain \"2 failed\"", final.Message)
	}
}

func mustSourceIDForSegment(t *testing.T, store *db.DB, segID int64) int64 {
	t.Helper()
	var sourceID int64
	if err := store.Read().QueryRow(`SELECT source_id FROM segments WHERE id = ?`, segID).Scan(&sourceID); err != nil {
		t.Fatalf("query source_id: %v", err)
	}
	return sourceID
}

func waitForJobTerminal(t *testing.T, queue *jobs.Queue, id int64, timeout time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := queue.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get %d: %v", id, err)
		}
		switch job.State {
		case jobs.StateDone, jobs.StateFailed, jobs.StateCancelled:
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %d did not reach a terminal state within %s", id, timeout)
	return nil
}
