package db

import (
	"context"
	"testing"
)

// TestCatalogBackupSummariesMixedBackedUpAndSSDOnly covers the core fix
// round 1 case: a single catalog value ("Toyota") spans two segments —
// one ingested to a persistent share (backed up), one still only on the
// SSD (ssd_only) — and the aggregate must count both, not just whichever
// one happens to be catalog_terms' sample_segment_id.
func TestCatalogBackupSummariesMixedBackedUpAndSSDOnly(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	shareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "a", Kind: KindLocal, MountPoint: "/mnt/shares/a", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}

	ssdSource := must(`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)

	backedUpSeg := must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 1000)`, ssdSource)
	det1 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, backedUpSeg)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.9)`, det1)
	must(`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'clip.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, backedUpSeg, shareID)

	ssdOnlySeg := must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'front', 'front/x/segment_00001.mp4', '2026-01-02T00:00:00Z', 1000)`, ssdSource)
	det2 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, ssdOnlySeg)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.9)`, det2)

	// A third segment, archived off the SSD with no ingest — neither
	// backed up nor ssd_only, and must not be double-counted into either
	// bucket.
	archivedSeg := must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes, archived_at)
		VALUES (?, 'front', 'front/x/segment_00002.mp4', '2026-01-03T00:00:00Z', 1000, '2026-01-04T00:00:00Z')`, ssdSource)
	det3 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, archivedSeg)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.9)`, det3)

	summaries, err := d.CatalogBackupSummaries(ctx, "make", []string{"Toyota"})
	if err != nil {
		t.Fatalf("CatalogBackupSummaries: %v", err)
	}
	got, ok := summaries["Toyota"]
	if !ok {
		t.Fatalf("no summary for Toyota: %+v", summaries)
	}
	if got.Segments != 3 {
		t.Errorf("segments = %d, want 3", got.Segments)
	}
	if got.BackedUp != 1 {
		t.Errorf("backed_up = %d, want 1", got.BackedUp)
	}
	if got.SSDOnly != 1 {
		t.Errorf("ssd_only = %d, want 1", got.SSDOnly)
	}
}

// TestCatalogBackupSummariesMultipleValues covers batching over several
// values in one call (the "no N+1" requirement) and that an unmatched
// value is simply absent from the result.
func TestCatalogBackupSummariesMultipleValues(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	ssdSource := must(`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)
	seg := must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 1000)`, ssdSource)
	det := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, seg)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Honda', 0.9)`, det)

	summaries, err := d.CatalogBackupSummaries(ctx, "make", []string{"Honda", "Nonexistent"})
	if err != nil {
		t.Fatalf("CatalogBackupSummaries: %v", err)
	}
	if got, ok := summaries["Honda"]; !ok || got.Segments != 1 || got.SSDOnly != 1 || got.BackedUp != 0 {
		t.Errorf("Honda = %+v (ok=%v)", got, ok)
	}
	if _, ok := summaries["Nonexistent"]; ok {
		t.Errorf("Nonexistent should not be present: %+v", summaries)
	}
}

// TestCatalogBackupSummariesEmptyInput covers the zero-length shortcut.
func TestCatalogBackupSummariesEmptyInput(t *testing.T) {
	d := openTest(t)
	summaries, err := d.CatalogBackupSummaries(context.Background(), "make", nil)
	if err != nil {
		t.Fatalf("CatalogBackupSummaries: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("summaries = %+v, want empty", summaries)
	}
}

// TestCatalogBackupSummariesUnknownKind covers the invalid-kind error.
func TestCatalogBackupSummariesUnknownKind(t *testing.T) {
	d := openTest(t)
	if _, err := d.CatalogBackupSummaries(context.Background(), "bogus", []string{"x"}); err == nil {
		t.Fatalf("expected an error for unknown kind")
	}
}
