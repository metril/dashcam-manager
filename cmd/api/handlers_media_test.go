package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestTripTimelineGetUnknownTripIs404(t *testing.T) {
	mux, _ := newTestMuxWithStore(t)
	rec := getJSON(t, mux, "/api/trips/999999/timeline", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
