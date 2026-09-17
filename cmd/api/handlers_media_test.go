package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dashcam/internal/db"
)

func TestResolveMediaPathRejectsTraversal(t *testing.T) {
	cases := []struct {
		name    string
		root    string
		rel     string
		wantErr bool
	}{
		{"normal", "/mnt/dashcam", "front/2026/01/01/recording_x/segment_00000.mp4", false},
		{"dotdot escapes root", "/mnt/dashcam", "../etc/passwd", true},
		{"dotdot buried mid-path", "/mnt/dashcam", "front/../../etc/passwd", true},
		{"absolute-looking rel resolves under root", "/mnt/dashcam", "/etc/passwd", false}, // filepath.Join treats it as relative
		{"empty root", "", "front/x.mp4", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMediaPath(tc.root, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveMediaPath(%q,%q) = %q, want error", tc.root, tc.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMediaPath(%q,%q) unexpected error: %v", tc.root, tc.rel, err)
			}
			cleanRoot := filepath.Clean(tc.root)
			if got != cleanRoot && got[:len(cleanRoot)+1] != cleanRoot+string(filepath.Separator) {
				t.Fatalf("resolveMediaPath(%q,%q) = %q, escapes root", tc.root, tc.rel, got)
			}
		})
	}
}

// seedMediaSegment seeds a source rooted at dir plus one segment pointing
// at relPath (a real file under dir, written by the caller) and returns
// its segment id.
func seedMediaSegment(t *testing.T, store *db.DB, dir, relPath string) int64 {
	t.Helper()
	ctx := context.Background()
	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', ?, 'SSD')`, dir)
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	res, err = store.Write().ExecContext(ctx, `
		INSERT INTO segments (source_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, 'front', ?, 0, '2026-03-01T10:00:00Z', '2026-03-01T10:00:30Z', 30, 5, 0, 0)
	`, sourceID, relPath)
	if err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	segID, _ := res.LastInsertId()
	return segID
}

func TestSegmentMediaGetServesRangeRequest(t *testing.T) {
	mux, store := newTestMuxWithStore(t)

	dir := t.TempDir()
	relPath := filepath.Join("front", "x", "segment_00000.mp4")
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := []byte("0123456789ABCDEFGHIJ")
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("write fixture mp4: %v", err)
	}

	segID := seedMediaSegment(t, store, dir, relPath)

	// Full GET.
	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/media", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", rec.Code, rec.Body)
	}
	if rec.Body.String() != string(content) {
		t.Fatalf("GET body = %q, want %q", rec.Body.String(), content)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", ct)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", rec.Header().Get("Accept-Ranges"))
	}

	// Range GET.
	req = httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/media", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206, body=%s", rec.Code, rec.Body)
	}
	if want := string(content[2:6]); rec.Body.String() != want {
		t.Fatalf("Range body = %q, want %q", rec.Body.String(), want)
	}
	if cr := rec.Header().Get("Content-Range"); cr == "" {
		t.Fatalf("missing Content-Range header")
	}
}

func TestSegmentMediaGetUnknownSegmentIs404(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/segments/999999/media", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSegmentMediaGetMissingFileOnDiskIs404(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	dir := t.TempDir()
	segID := seedMediaSegment(t, store, dir, filepath.Join("front", "x", "gone.mp4"))

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/media", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body)
	}
}

// TestSegmentMediaGetArchivedSegmentReturns404WithShareName covers
// final-fix-wave item 7's playback-exclusion half: an archived segment
// (archived_at set — its file is gone but a verified ingests row backs it)
// must 404 with a message naming the share it was archived to, not a
// generic "not found on disk".
func TestSegmentMediaGetArchivedSegmentReturns404WithShareName(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/api/segments/"+itoa(segID)+"/media", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "backup-nas") {
		t.Errorf("expected the 404 body to name the archive share, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "archived") {
		t.Errorf("expected the 404 body to mention 'archived', got: %s", rec.Body.String())
	}
}

func TestTripTimelineGet(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	tripID, _, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	var timeline db.TripTimeline
	rec := getJSON(t, mux, "/api/trips/"+itoa(tripID)+"/timeline", &timeline)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if timeline.TripID != tripID {
		t.Fatalf("trip_id = %d, want %d", timeline.TripID, tripID)
	}
	if len(timeline.Segments) != 2 {
		t.Fatalf("segments = %+v, want 2", timeline.Segments)
	}
	// front segment in seedTrip has n_car=2, n_person=1 -> count 3; rear has none.
	var frontCount, rearCount int
	for _, s := range timeline.Segments {
		if s.Camera == "front" {
			frontCount = s.Count
		} else {
			rearCount = s.Count
		}
		if s.OffsetS < -0.001 {
			t.Fatalf("segment %+v has negative offset_s", s)
		}
	}
	if frontCount != 3 {
		t.Fatalf("front count = %d, want 3", frontCount)
	}
	if rearCount != 0 {
		t.Fatalf("rear count = %d, want 0", rearCount)
	}
}

func TestTripTimelineGetHasOBD(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	tripID, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")
	ctx := context.Background()
	if _, err := store.Write().ExecContext(ctx, `UPDATE segments SET has_obd = 1 WHERE id = ?`, frontSegID); err != nil {
		t.Fatalf("set has_obd: %v", err)
	}

	var timeline db.TripTimeline
	rec := getJSON(t, mux, "/api/trips/"+itoa(tripID)+"/timeline", &timeline)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var frontHasOBD, rearHasOBD bool
	for _, s := range timeline.Segments {
		if s.Camera == "front" {
			frontHasOBD = s.HasOBD
		} else {
			rearHasOBD = s.HasOBD
		}
	}
	if !frontHasOBD {
		t.Errorf("front segment has_obd = false, want true")
	}
	if rearHasOBD {
		t.Errorf("rear segment has_obd = true, want false")
	}
}

func TestTripTimelineGetUnknownTripIs404(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/trips/999999/timeline", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
