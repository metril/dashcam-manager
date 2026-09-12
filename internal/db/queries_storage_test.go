package db

import (
	"context"
	"testing"
)

// TestStoragesListAggregatesUsagePerShareAndCache covers StoragesList's
// core contract: one row per share (any kind) plus a trailing cache row,
// each with segment/export totals scoped correctly — and, critically, that
// an SSD-sourced segment (source.share_id NULL) is never counted anywhere,
// since the SSD is never a storage (see the plan's Context section).
func TestStoragesListAggregatesUsagePerShareAndCache(t *testing.T) {
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

	cifsShareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "nas", Server: "10.0.0.5", ShareName: "footage", MountPoint: "/mnt/shares/nas", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate cifs: %v", err)
	}
	localShareID, err := d.ShareCreate(ctx, ShareCreateParams{Name: "local dir", Kind: KindLocal, MountPoint: "/mnt/shares/local-test", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate local: %v", err)
	}

	// SSD source: share_id NULL. Its segment must never be counted.
	ssdSourceID := must(`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)
	must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 999999)`, ssdSourceID)

	// A share-backed source for the cifs share, with two segments.
	cifsSourceID := must(`INSERT INTO sources (kind, share_id, root_path) VALUES ('share', ?, '/mnt/shares/nas')`, cifsShareID)
	must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 1000)`, cifsSourceID)
	must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'rear', 'rear/x/segment_00000.mp4', '2026-01-01T00:00:02Z', 2000)`, cifsSourceID)

	// A share-backed source for the local share, with one segment.
	localSourceID := must(`INSERT INTO sources (kind, share_id, root_path) VALUES ('share', ?, '/mnt/shares/local-test')`, localShareID)
	must(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes)
		VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 500)`, localSourceID)

	// Exports: one on each share, one to the cache (share_id NULL).
	must(`INSERT INTO exports (name, share_id, bytes) VALUES ('e1', ?, 100)`, cifsShareID)
	must(`INSERT INTO exports (name, share_id, bytes) VALUES ('e2', ?, 200)`, localShareID)
	must(`INSERT INTO exports (name, share_id, bytes) VALUES ('e2b', ?, 300)`, localShareID)
	must(`INSERT INTO exports (name, share_id, bytes) VALUES ('e3', NULL, 400)`)

	rows, err := d.StoragesList(ctx)
	if err != nil {
		t.Fatalf("StoragesList: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3 (cifs share, local share, cache); rows=%+v", len(rows), rows)
	}

	byShareID := map[int64]StorageRow{}
	var cache *StorageRow
	for i := range rows {
		r := rows[i]
		if r.ShareID == nil {
			cache = &rows[i]
			continue
		}
		byShareID[*r.ShareID] = r
	}
	if cache == nil {
		t.Fatalf("no cache row found in %+v", rows)
	}

	cifsRow, ok := byShareID[cifsShareID]
	if !ok {
		t.Fatalf("no row for cifs share %d in %+v", cifsShareID, rows)
	}
	if cifsRow.Kind != KindCIFS {
		t.Errorf("cifs row Kind = %q, want %q", cifsRow.Kind, KindCIFS)
	}
	if cifsRow.SegmentCount != 2 || cifsRow.SegmentBytes != 3000 {
		t.Errorf("cifs row segments = (%d, %d), want (2, 3000)", cifsRow.SegmentCount, cifsRow.SegmentBytes)
	}
	if cifsRow.ExportCount != 1 || cifsRow.ExportBytes != 100 {
		t.Errorf("cifs row exports = (%d, %d), want (1, 100)", cifsRow.ExportCount, cifsRow.ExportBytes)
	}

	localRow, ok := byShareID[localShareID]
	if !ok {
		t.Fatalf("no row for local share %d in %+v", localShareID, rows)
	}
	if localRow.Kind != KindLocal {
		t.Errorf("local row Kind = %q, want %q", localRow.Kind, KindLocal)
	}
	if localRow.SegmentCount != 1 || localRow.SegmentBytes != 500 {
		t.Errorf("local row segments = (%d, %d), want (1, 500)", localRow.SegmentCount, localRow.SegmentBytes)
	}
	if localRow.ExportCount != 2 || localRow.ExportBytes != 500 {
		t.Errorf("local row exports = (%d, %d), want (2, 500)", localRow.ExportCount, localRow.ExportBytes)
	}

	if cache.Kind != "cache" {
		t.Errorf("cache row Kind = %q, want %q", cache.Kind, "cache")
	}
	if cache.ExportCount != 1 || cache.ExportBytes != 400 {
		t.Errorf("cache row exports = (%d, %d), want (1, 400)", cache.ExportCount, cache.ExportBytes)
	}
	if cache.SegmentCount != 0 || cache.SegmentBytes != 0 {
		t.Errorf("cache row segments = (%d, %d), want (0, 0)", cache.SegmentCount, cache.SegmentBytes)
	}
}

// TestStoragesListEmpty covers the no-shares-configured case: only the
// cache pseudo-storage, with zero usage.
func TestStoragesListEmpty(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	rows, err := d.StoragesList(ctx)
	if err != nil {
		t.Fatalf("StoragesList: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (cache only); rows=%+v", len(rows), rows)
	}
	if rows[0].ShareID != nil || rows[0].Kind != "cache" {
		t.Errorf("rows[0] = %+v, want cache pseudo-storage", rows[0])
	}
}
