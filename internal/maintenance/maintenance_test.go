package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dashcam/internal/db"
)

// openTest mirrors internal/db's own openTest helper (unexported, so not
// reusable across packages) — a fresh temp-file sqlite database with the
// schema applied. Returns the DB and the sqlite file's path, since Verify/
// Prune need the path directly (it's not exposed on *db.DB).
func openTest(t *testing.T) (*db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dashcam.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return d, path
}

// seeded holds every id/path a test needs to assert against, built by
// seedFixture.
type seeded struct {
	cacheDir          string
	sourceID          int64
	okSegmentID       int64 // real file on disk, has_detections + detections_raw + detections/objects/tracks, thumbnail on disk
	missingMediaSegID int64 // db row present, file absent, archived_at NULL -> flagged/pruned
	archivedSegID     int64 // db row present, file absent, archived_at set -> must NOT be flagged/pruned
	rawMissingSegID   int64 // has_detections=1 but no detections_raw row -> reported, never pruned
}

func seedFixture(t *testing.T, d *db.DB) seeded {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	cacheDir := t.TempDir()

	var s seeded
	s.cacheDir = cacheDir

	res, err := d.Write().ExecContext(ctx, `INSERT INTO sources (kind, root_path) VALUES ('ssd', ?)`, root)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	s.sourceID, _ = res.LastInsertId()

	insertSegment := func(relPath string, hasDetections bool, archived bool) int64 {
		var archivedAt any
		if archived {
			archivedAt = "2026-01-01T00:00:00Z"
		}
		res, err := d.Write().ExecContext(ctx, `
			INSERT INTO segments (source_id, camera, rel_path, start_time, has_detections, archived_at)
			VALUES (?, 'front', ?, '2026-01-01T00:00:00Z', ?, ?)`,
			s.sourceID, relPath, boolToInt(hasDetections), archivedAt)
		if err != nil {
			t.Fatalf("insert segment %s: %v", relPath, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	s.okSegmentID = insertSegment("ok.mp4", true, false)
	if err := os.WriteFile(filepath.Join(root, "ok.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write ok.mp4: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO detections_raw (segment_id, gz) VALUES (?, x'1f8b')`, s.okSegmentID); err != nil {
		t.Fatalf("insert detections_raw: %v", err)
	}
	detRes, err := d.Write().ExecContext(ctx, `INSERT INTO detections (segment_id, pts_offset) VALUES (?, 0)`, s.okSegmentID)
	if err != nil {
		t.Fatalf("insert detections: %v", err)
	}
	detID, _ := detRes.LastInsertId()
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO det_objects (det_id, class, confidence, x1,y1,x2,y2) VALUES (?, 'car', 0.9, 0,0,1,1)`, detID); err != nil {
		t.Fatalf("insert det_objects: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames) VALUES (?, 'object', 'car', 0, 1, 2)`, s.okSegmentID); err != nil {
		t.Fatalf("insert det_tracks: %v", err)
	}
	// A cached thumbnail for the ok segment — must NOT be reported/removed
	// as orphaned.
	thumbDir := filepath.Join(cacheDir, "thumbs", "0")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		t.Fatalf("mkdir thumbs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(thumbDir, fmt.Sprintf("%d.jpg", s.okSegmentID)), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("write ok thumb: %v", err)
	}

	s.missingMediaSegID = insertSegment("gone.mp4", false, false)
	s.archivedSegID = insertSegment("archived-gone.mp4", false, true)
	s.rawMissingSegID = insertSegment("no-raw.mp4", true, false)
	if err := os.WriteFile(filepath.Join(root, "no-raw.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write no-raw.mp4: %v", err)
	}

	// Orphan det rows: FK enforcement is normally on, so seed these with
	// it off, to model rows left over from a bug/migration rather than
	// something InitSchema's cascades would ever normally allow.
	if _, err := d.Write().ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("pragma fk off: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO detections (id, segment_id, pts_offset) VALUES (9001, 999999, 0)`); err != nil {
		t.Fatalf("insert orphan detection: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO det_objects (det_id, class, confidence, x1,y1,x2,y2) VALUES (999999, 'car', 0.9,0,0,1,1)`); err != nil {
		t.Fatalf("insert orphan det_objects: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO det_plates (det_id, text, confidence, x1,y1,x2,y2) VALUES (999999, 'ABC123', 0.9,0,0,1,1)`); err != nil {
		t.Fatalf("insert orphan det_plates: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO det_vehicles (det_id, type, confidence, x1,y1,x2,y2) VALUES (999999, 'sedan', 0.9,0,0,1,1)`); err != nil {
		t.Fatalf("insert orphan det_vehicles: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, `INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames) VALUES (999999, 'object', 'car', 0, 1, 2)`); err != nil {
		t.Fatalf("insert orphan det_tracks: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("pragma fk on: %v", err)
	}

	// An orphan thumbnail file: no segment 424242.
	orphanDir := filepath.Join(cacheDir, "thumbs", "242")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan thumbs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "424242.jpg"), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("write orphan thumb: %v", err)
	}

	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
