package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/media"
)

// ---- exportRequest.validate() ----

func TestExportRequestValidate(t *testing.T) {
	valid := func() exportRequest {
		return exportRequest{
			Items:   []exportItemParams{{SegmentID: 1}},
			Camera:  "front",
			Quality: "source",
			Target:  "download",
		}
	}

	cases := []struct {
		name    string
		mutate  func(r *exportRequest)
		wantErr bool
	}{
		{"valid as-is", func(r *exportRequest) {}, false},
		{"empty items", func(r *exportRequest) { r.Items = nil }, true},
		{"bad camera", func(r *exportRequest) { r.Camera = "side" }, true},
		{"bad quality", func(r *exportRequest) { r.Quality = "4k" }, true},
		{"empty quality ok (default)", func(r *exportRequest) { r.Quality = "" }, false},
		{"empty target ok (default download)", func(r *exportRequest) { r.Target = "" }, false},
		{"non-numeric share target", func(r *exportRequest) { r.Target = "share-1" }, true},
		{"numeric share target ok", func(r *exportRequest) { r.Target = "3" }, false},
		{"zero segment id", func(r *exportRequest) { r.Items[0].SegmentID = 0 }, true},
		{"out_s before in_s", func(r *exportRequest) {
			in, out := 5.0, 2.0
			r.Items[0].InS, r.Items[0].OutS = &in, &out
		}, true},
		{"out_s after in_s ok", func(r *exportRequest) {
			in, out := 2.0, 5.0
			r.Items[0].InS, r.Items[0].OutS = &in, &out
		}, false},
		{"exactly maxExportItems ok", func(r *exportRequest) {
			r.Items = make([]exportItemParams, maxExportItems)
			for i := range r.Items {
				r.Items[i] = exportItemParams{SegmentID: int64(i + 1)}
			}
		}, false},
		{"over maxExportItems rejected", func(r *exportRequest) {
			r.Items = make([]exportItemParams, maxExportItems+1)
			for i := range r.Items {
				r.Items[i] = exportItemParams{SegmentID: int64(i + 1)}
			}
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)
			err := r.validate()
			if tc.wantErr && err == nil {
				t.Errorf("validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validate() = %v, want nil", err)
			}
		})
	}
}

// ---- sanitizeExportFilename ----

func TestSanitizeExportFilename(t *testing.T) {
	cases := map[string]string{
		"my clip":          "my-clip",
		"../../etc/passwd": "etc-passwd",
		"a/b\\c":           "a-b-c",
		"":                 "export",
		"...":              "export",
		"already-safe_1.2": "already-safe_1.2",
	}
	for in, want := range cases {
		if got := sanitizeExportFilename(in); got != want {
			t.Errorf("sanitizeExportFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- resolveRearPlacements: critical M11 review regression ----

// seedExportTrip creats a source + trip (with pair_offset_s) + one front
// segment and two rear segments on a shared timeline, returning the trip
// id and the front segment's SegmentExportInfo-shaped id.
func seedExportTrip(t *testing.T, store *db.DB) (tripID, frontSegID int64) {
	t.Helper()
	ctx := context.Background()

	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	tripStart := "2026-01-01T00:00:00.000Z"
	// pair_offset_s is deliberately nonzero and NOT equal to the rear
	// segments' actual offset from the front timeline — this is the
	// crux of the regression: resolveRearPlacements must ignore it and
	// match purely by each segment's own recorded start_time.
	res, err = store.Write().ExecContext(ctx,
		`INSERT INTO trips (source_id, trip_key, started_at, pair_offset_s) VALUES (?, 'trip1', ?, 5.0)`,
		sourceID, tripStart)
	if err != nil {
		t.Fatalf("seed trip: %v", err)
	}
	tripID, _ = res.LastInsertId()

	insertSeg := func(camera, startTime string, durationS float64) int64 {
		res, err := store.Write().ExecContext(ctx, `
			INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, duration_s)
			VALUES (?, ?, ?, ?, 1, ?, ?)`,
			sourceID, tripID, camera, camera+"/"+startTime+".mp4", startTime, durationS)
		if err != nil {
			t.Fatalf("seed %s segment: %v", camera, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	// Front segment covers trip-offset [0,30).
	frontSegID = insertSeg("front", tripStart, 30)
	// rear0 covers trip-offset [0,30) (same start as front — offset 0).
	insertSeg("rear", tripStart, 30)
	// rear1 covers trip-offset [30,60).
	insertSeg("rear", "2026-01-01T00:00:30.000Z", 30)

	return tripID, frontSegID
}

func TestResolveRearPlacementsMatchesAbsoluteTimeNotPairOffset(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	tripID, frontSegID := seedExportTrip(t, store)

	// Front item trimmed to [24,30) of its segment -> trip-offset mid =
	// (24+30)/2 = 27, which falls in rear0's [0,30) window. The old,
	// buggy code added pair_offset_s (5) to the target, landing on 32 —
	// inside rear1's [30,60) window instead. Asserting rear0 is picked
	// (and that its local offset is exactly 27, not 32-30=2) proves the
	// fix, not just "some" rear segment was found.
	fronts := []resolvedItem{{
		info:      db.SegmentExportInfo{ID: frontSegID},
		inS:       24,
		outS:      30,
		durationS: 30,
	}}

	rears, err := resolveRearPlacements(context.Background(), store, fronts, tripID)
	if err != nil {
		t.Fatalf("resolveRearPlacements: %v", err)
	}
	if len(rears) != 1 {
		t.Fatalf("resolveRearPlacements returned %d items, want 1", len(rears))
	}

	// rear0 was inserted first (id 2 relative to front's id 1, in
	// insertion order) — confirm by its resolved local offset instead of
	// assuming ids, since that's what actually matters for correctness.
	got := rears[0]
	wantLocalMid := 27.0 // == mid, since rear0's OffsetS is 0
	gotLocalMid := (got.inS + got.outS) / 2
	if diff := gotLocalMid - wantLocalMid; diff > 0.01 || diff < -0.01 {
		t.Errorf("resolved rear local offset = %v, want %v (pair_offset_s must not be re-applied)", gotLocalMid, wantLocalMid)
	}
}

// ---- download / delete / expiry ----

func newDownloadTestStore(t *testing.T) *db.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return store
}

func TestHandleExportDownloadNotFound(t *testing.T) {
	store := newDownloadTestStore(t)
	req := httptest.NewRequest(http.MethodGet, "/api/exports/999/download", nil)
	req.SetPathValue("id", "999")
	w := httptest.NewRecorder()
	handleExportDownloadGet(store)(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleExportDownloadNotReady(t *testing.T) {
	store := newDownloadTestStore(t)
	exp, err := store.ExportCreate(context.Background(), "clip")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/exports/1/download", nil)
	req.SetPathValue("id", strconv.FormatInt(exp.ID, 10))
	w := httptest.NewRecorder()
	handleExportDownloadGet(store)(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestHandleExportDownloadRefusesShareTarget(t *testing.T) {
	store := newDownloadTestStore(t)
	exp, err := store.ExportCreate(context.Background(), "clip")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	shareID := insertTestShareForExport(t, store)
	if err := store.ExportFinish(context.Background(), exp.ID, "/mnt/shares/x/exports/clip.mp4", &shareID, 100, nil); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/exports/1/download", nil)
	req.SetPathValue("id", strconv.FormatInt(exp.ID, 10))
	w := httptest.NewRecorder()
	handleExportDownloadGet(store)(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (share-target exports aren't downloadable here)", w.Code)
	}
}

func TestHandleExportDownloadStreamsFile(t *testing.T) {
	store := newDownloadTestStore(t)
	exp, err := store.ExportCreate(context.Background(), "my clip")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "1.mp4")
	if err := os.WriteFile(path, []byte("fake mp4 bytes"), 0o644); err != nil {
		t.Fatalf("write fake export file: %v", err)
	}
	if err := store.ExportFinish(context.Background(), exp.ID, path, nil, 14, nil); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/exports/1/download", nil)
	req.SetPathValue("id", strconv.FormatInt(exp.ID, 10))
	w := httptest.NewRecorder()
	handleExportDownloadGet(store)(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "fake mp4 bytes" {
		t.Errorf("body = %q, want the export file's contents", w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); cd == "" {
		t.Errorf("missing Content-Disposition header")
	}
}

func TestCleanupExpiredExportsRemovesFileAndRow(t *testing.T) {
	store := newDownloadTestStore(t)
	ctx := context.Background()

	dir := t.TempDir()
	path := filepath.Join(dir, "old.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fake file: %v", err)
	}
	exp, err := store.ExportCreate(ctx, "old")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	pastExpiry := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if err := store.ExportFinish(ctx, exp.ID, path, nil, 1, &pastExpiry); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}

	n, err := CleanupExpiredExports(ctx, store)
	if err != nil {
		t.Fatalf("CleanupExpiredExports: %v", err)
	}
	if n != 1 {
		t.Errorf("CleanupExpiredExports removed %d, want 1", n)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expired export file still exists at %s", path)
	}
	if _, getErr := store.ExportGet(ctx, exp.ID); getErr != db.ErrNotFound {
		t.Errorf("ExportGet after cleanup err = %v, want ErrNotFound", getErr)
	}
}

// ---- finalizeExportOutput share-target gating (final-fix-wave item 3) ----

// TestFinalizeExportOutputUsesShareMountPointDirectly covers a bug where
// destDir was computed as
// share.MountPointFor(share.DefaultMountBase, sh.MountPoint) — re-running
// the already-real mount path back through the slug-to-path helper — which
// produced a bogus nested path instead of "<mount_point>/exports". It must
// join "exports" onto sh.MountPoint directly.
func TestFinalizeExportOutputUsesShareMountPointDirectly(t *testing.T) {
	store := newDownloadTestStore(t)
	ctx := context.Background()
	mount := t.TempDir()

	var shareID int64
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share, mount_point) VALUES ('test', 'nas.local', 'footage', ?) RETURNING id`,
		mount).Scan(&shareID); err != nil {
		t.Fatalf("insert test share: %v", err)
	}
	// allow_local_share_paths lets ResolveTargetMount accept a plain temp
	// dir as "mounted" without a real CIFS mount, matching internal/ingest's
	// own test pattern.
	if err := store.SettingsSet(ctx, "allow_local_share_paths", []byte("true")); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "out.mp4")
	if err := os.WriteFile(srcFile, []byte("video"), 0o644); err != nil {
		t.Fatalf("write fake export output: %v", err)
	}

	finalPath, gotShareID, expiresAt, err := finalizeExportOutput(ctx, store, srcFile, exportJobParams{
		Name: "my-export", Target: strconv.FormatInt(shareID, 10),
	})
	if err != nil {
		t.Fatalf("finalizeExportOutput: %v", err)
	}
	wantPath := filepath.Join(mount, "exports", "my-export.mp4")
	if finalPath != wantPath {
		t.Errorf("finalPath = %q, want %q", finalPath, wantPath)
	}
	if gotShareID == nil || *gotShareID != shareID {
		t.Errorf("shareID = %v, want %d", gotShareID, shareID)
	}
	if expiresAt != nil {
		t.Errorf("expiresAt = %v, want nil for a share target (no expiry)", expiresAt)
	}
	if _, statErr := os.Stat(wantPath); statErr != nil {
		t.Errorf("expected exported file at %s: %v", wantPath, statErr)
	}
}

// TestFinalizeExportOutputRefusesUnmountedShare covers the mount-gate half
// of item 3: without allow_local_share_paths (or a real mount),
// finalizeExportOutput must refuse rather than writing into whatever
// (possibly-empty, possibly some unrelated directory) happens to sit at
// the share's stored mount_point.
func TestFinalizeExportOutputRefusesUnmountedShare(t *testing.T) {
	store := newDownloadTestStore(t)
	ctx := context.Background()
	mount := t.TempDir() // exists on disk, but never actually "mounted"

	var shareID int64
	if err := store.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share, mount_point) VALUES ('test', 'nas.local', 'footage', ?) RETURNING id`,
		mount).Scan(&shareID); err != nil {
		t.Fatalf("insert test share: %v", err)
	}

	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "out.mp4")
	if err := os.WriteFile(srcFile, []byte("video"), 0o644); err != nil {
		t.Fatalf("write fake export output: %v", err)
	}

	_, _, _, err := finalizeExportOutput(ctx, store, srcFile, exportJobParams{
		Name: "my-export", Target: strconv.FormatInt(shareID, 10),
	})
	if err == nil {
		t.Fatal("finalizeExportOutput with an unmounted share target: want error, got nil")
	}
	if _, statErr := os.Stat(srcFile); statErr != nil {
		t.Errorf("source file should be left in place on failure, but: %v", statErr)
	}
}

// TestHandleExportPostRejectsUnreadableSegment covers final-findings item
// 4: POST /api/export must reject (400) an item whose segment has
// unreadable_reason set, naming the segment in the error, rather than
// enqueueing an export job doomed to fail on a file ffmpeg can never
// decode.
func TestHandleExportPostRejectsUnreadableSegment(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")
	if err := store.SegmentMarkUnreadable(context.Background(), frontSegID, "Missing moov atom"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	body, _ := json.Marshal(exportRequest{
		Items:  []exportItemParams{{SegmentID: frontSegID}},
		Camera: "front",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/export", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v, body=%s", err, w.Body)
	}
	wantSubstr := strconv.FormatInt(frontSegID, 10)
	if !containsAll(errResp.Error, wantSubstr, "unreadable") {
		t.Errorf("error message = %q, want it to name segment %d and say unreadable", errResp.Error, frontSegID)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !bytes.Contains([]byte(s), []byte(sub)) {
			return false
		}
	}
	return true
}

// ---- test helpers ----

func insertTestShareForExport(t *testing.T, store *db.DB) int64 {
	t.Helper()
	var id int64
	err := store.Write().QueryRowContext(context.Background(),
		`INSERT INTO shares (name, server, share) VALUES ('test', 'nas.local', 'footage') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert test share: %v", err)
	}
	return id
}

// ---- loadOverlayFrames / DetectionFilter burn-in filtering ----

// TestLoadOverlayFramesDropsBoxesByFilter is Task 3's brief-mandated
// verification: "Go test that filter-script output omits filtered boxes."
// A nil filter keeps everything (pre-Task-3 behavior); a filter drops
// low-confidence and disallowed-class object boxes, low-confidence plate
// boxes (the plate-label gate), and everything of a kind switched off.
func TestLoadOverlayFramesDropsBoxesByFilter(t *testing.T) {
	store := newDownloadTestStore(t)
	ctx := context.Background()

	res, err := store.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()
	res, err = store.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, seq, start_time, has_detections)
		 VALUES (?, 'front', 'front/x/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', 1)`, sourceID)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	segID, _ := res.LastInsertId()

	raw := []byte(`{"segment":"segment_00000.mp4","camera":"front","frame_width":640,"frame_height":640,
		"detections":[{"timestamp":0.5,"frame_index":0,"pts_offset":0.5,
			"objects":[
				{"class":"car","confidence":0.9,"bbox":[64,64,320,320]},
				{"class":"car","confidence":0.2,"bbox":[64,64,320,320]},
				{"class":"person","confidence":0.9,"bbox":[64,64,320,320]}
			],
			"plates":[
				{"text":"ABC123","confidence":0.8,"bbox":[64,64,320,320],"vehicle_bbox":[0,0,640,640]},
				{"text":"LOWCONF","confidence":0.1,"bbox":[64,64,320,320],"vehicle_bbox":[0,0,640,640]}
			]}]}`)
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if _, err := store.Write().ExecContext(ctx,
		`INSERT INTO detections_raw (segment_id, gz) VALUES (?, ?)`, segID, gzBuf.Bytes()); err != nil {
		t.Fatalf("insert detections_raw: %v", err)
	}

	// No filter: every box (3 objects + 2 plates) survives.
	frames, err := loadOverlayFrames(ctx, store, segID, 0, 60, nil)
	if err != nil {
		t.Fatalf("loadOverlayFrames (nil filter): %v", err)
	}
	if got := media.CountBoxes(frames); got != 5 {
		t.Fatalf("CountBoxes (nil filter) = %d, want 5", got)
	}

	// min_conf 0.5, classes restricted to car, plates on: only the
	// high-confidence car object and the high-confidence plate survive
	// (low-conf car dropped by MinConf, person dropped by Classes,
	// low-conf plate dropped by the plate-label gate).
	filter := &DetectionFilter{MinConf: 0.5, Objects: true, Plates: true, Vehicles: true, Classes: []string{"car"}}
	frames, err = loadOverlayFrames(ctx, store, segID, 0, 60, filter)
	if err != nil {
		t.Fatalf("loadOverlayFrames (filtered): %v", err)
	}
	if got := media.CountBoxes(frames); got != 2 {
		t.Fatalf("CountBoxes (filtered) = %d, want 2 (1 car + 1 plate); frames=%+v", got, frames)
	}
	var labels []string
	for _, f := range frames {
		for _, b := range f.Boxes {
			labels = append(labels, b.Label)
		}
	}
	wantLabels := map[string]bool{"car": true, "ABC123": true}
	for _, l := range labels {
		if !wantLabels[l] {
			t.Errorf("unexpected surviving label %q, want only %v", l, wantLabels)
		}
	}

	// Objects switched off entirely: only the high-confidence plate
	// survives.
	filter = &DetectionFilter{MinConf: 0.5, Objects: false, Plates: true, Vehicles: true}
	frames, err = loadOverlayFrames(ctx, store, segID, 0, 60, filter)
	if err != nil {
		t.Fatalf("loadOverlayFrames (objects off): %v", err)
	}
	if got := media.CountBoxes(frames); got != 1 {
		t.Fatalf("CountBoxes (objects off) = %d, want 1 (plate only)", got)
	}
}

func TestExportRequestValidateOverlayFilter(t *testing.T) {
	r := exportRequest{
		Items:   []exportItemParams{{SegmentID: 1}},
		Camera:  "front",
		Quality: "source",
		Target:  "download",
	}

	// Unknown class rejected.
	r.OverlayFilter = &DetectionFilter{Classes: []string{"spaceship"}}
	if err := r.validate(); err == nil {
		t.Fatal("validate() = nil, want error for unknown overlay_filter class")
	}

	// Out-of-range min_conf is clamped, not rejected.
	r.OverlayFilter = &DetectionFilter{MinConf: 5}
	if err := r.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil (min_conf should clamp)", err)
	}
	if r.OverlayFilter.MinConf != 1 {
		t.Errorf("MinConf = %v, want clamped to 1", r.OverlayFilter.MinConf)
	}

	// Nil overlay_filter is fine (means "unfiltered").
	r.OverlayFilter = nil
	if err := r.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil for absent overlay_filter", err)
	}
}
