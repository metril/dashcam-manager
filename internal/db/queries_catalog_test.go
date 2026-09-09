package db

import (
	"context"
	"testing"
	"time"
)

// seedCatalogFixture inserts two segments each with one detection: seg1
// carries two Toyota sedans (two separate det_vehicles rows so make="Toyota"
// has count 2), seg2 carries one Ford truck. Returns (seg1ID, seg2ID).
func seedCatalogFixture(t *testing.T, d *DB) (seg1, seg2 int64) {
	t.Helper()
	ctx := context.Background()
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	trip1 := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260301_100000', '2026-03-01T10:00:00Z', 'recording_20260301_100000', 1, 1000)`, sourceID)
	seg1 = must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/01/recording_20260301_100000/segment_00000.mp4', 0, '2026-03-01T10:00:00Z', '2026-03-01T10:00:30Z', 30, 1000, 1, 0)`, sourceID, trip1)
	det1a := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, seg1)
	det1b := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 2.0)`, seg1)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.9)`, det1a)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'sedan', 'Toyota', 0.8)`, det1b)
	must(`INSERT INTO det_plates (det_id, text, confidence) VALUES (?, 'ABC123', 0.7)`, det1a)

	trip2 := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260305_090000', '2026-03-05T09:00:00Z', 'recording_20260305_090000', 1, 1000)`, sourceID)
	seg2 = must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/05/recording_20260305_090000/segment_00000.mp4', 0, '2026-03-05T09:00:00Z', '2026-03-05T09:00:30Z', 30, 1000, 1, 0)`, sourceID, trip2)
	det2 := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, seg2)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'truck', 'Ford', 0.6)`, det2)
	must(`INSERT INTO det_objects (det_id, class, confidence) VALUES (?, 'person', 0.5)`, det2)

	return seg1, seg2
}

func TestCatalogListMakeCountsAndOrdering(t *testing.T) {
	d := openTest(t)
	_, _ = seedCatalogFixture(t, d)
	ctx := context.Background()
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	items, cursor, err := d.CatalogList(ctx, CatalogFilter{Kind: "make"})
	if err != nil {
		t.Fatalf("CatalogList: %v", err)
	}
	if cursor != "" {
		t.Fatalf("unexpected next cursor: %q", cursor)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}
	// Toyota has count 2, Ford has count 1 — ordered count desc.
	if items[0].Value != "Toyota" || items[0].Count != 2 {
		t.Errorf("item 0 = %+v, want Toyota/2", items[0])
	}
	if items[1].Value != "Ford" || items[1].Count != 1 {
		t.Errorf("item 1 = %+v, want Ford/1", items[1])
	}
	if items[0].FirstSeen == "" || items[0].LastSeen == "" || items[0].SampleSegID == 0 {
		t.Errorf("item 0 missing first/last seen or sample: %+v", items[0])
	}
}

func TestCatalogListMinCount(t *testing.T) {
	d := openTest(t)
	seg1, _ := seedCatalogFixture(t, d)
	ctx := context.Background()
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	items, _, err := d.CatalogList(ctx, CatalogFilter{Kind: "make", MinCount: 2})
	if err != nil {
		t.Fatalf("CatalogList: %v", err)
	}
	if len(items) != 1 || items[0].Value != "Toyota" {
		t.Fatalf("got %+v, want just Toyota", items)
	}
	if items[0].SampleSegID != seg1 {
		t.Errorf("sample segment = %d, want %d", items[0].SampleSegID, seg1)
	}
}

func TestCatalogListQFilter(t *testing.T) {
	d := openTest(t)
	seedCatalogFixture(t, d)
	ctx := context.Background()
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	items, _, err := d.CatalogList(ctx, CatalogFilter{Kind: "make", Q: "for"})
	if err != nil {
		t.Fatalf("CatalogList: %v", err)
	}
	if len(items) != 1 || items[0].Value != "Ford" {
		t.Fatalf("got %+v, want just Ford", items)
	}
}

func TestCatalogListPlateAndClassKinds(t *testing.T) {
	d := openTest(t)
	seedCatalogFixture(t, d)
	ctx := context.Background()
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	plates, _, err := d.CatalogList(ctx, CatalogFilter{Kind: "plate"})
	if err != nil {
		t.Fatalf("CatalogList plate: %v", err)
	}
	if len(plates) != 1 || plates[0].Value != "ABC123" {
		t.Fatalf("got %+v, want just ABC123", plates)
	}

	classes, _, err := d.CatalogList(ctx, CatalogFilter{Kind: "class"})
	if err != nil {
		t.Fatalf("CatalogList class: %v", err)
	}
	if len(classes) != 1 || classes[0].Value != "person" {
		t.Fatalf("got %+v, want just person", classes)
	}
}

func TestCatalogIsEmptyAndRefreshedAt(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	empty, err := d.CatalogIsEmpty(ctx)
	if err != nil {
		t.Fatalf("CatalogIsEmpty: %v", err)
	}
	if !empty {
		t.Fatal("expected catalog_terms to start empty")
	}
	refreshedAt, err := d.CatalogRefreshedAt(ctx)
	if err != nil {
		t.Fatalf("CatalogRefreshedAt: %v", err)
	}
	if refreshedAt != "" {
		t.Fatalf("expected empty refreshed_at before any refresh, got %q", refreshedAt)
	}

	seedCatalogFixture(t, d)
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	empty, err = d.CatalogIsEmpty(ctx)
	if err != nil {
		t.Fatalf("CatalogIsEmpty after refresh: %v", err)
	}
	if empty {
		t.Fatal("expected catalog_terms to be populated after RefreshCatalog")
	}
	refreshedAt, err = d.CatalogRefreshedAt(ctx)
	if err != nil {
		t.Fatalf("CatalogRefreshedAt after refresh: %v", err)
	}
	if refreshedAt == "" {
		t.Fatal("expected a non-empty refreshed_at after RefreshCatalog")
	}
	if _, err := time.Parse(time.RFC3339, refreshedAt); err != nil {
		t.Fatalf("refreshed_at %q is not RFC3339: %v", refreshedAt, err)
	}
}

// TestCatalogListReflectsRefreshNotLiveState: CatalogList reads from
// catalog_terms as of the last RefreshCatalog, not live det_vehicles rows —
// this is the whole point of the precomputed-table fix (a live GROUP BY
// over a multi-million-row det_vehicles table measured at ~24s per
// request). New detections inserted after a refresh must not show up until
// RefreshCatalog runs again.
func TestCatalogListReflectsRefreshNotLiveState(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seedCatalogFixture(t, d)
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	// Insert a third make directly, bypassing RefreshCatalog.
	must := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("seed exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	sourceID := must(`INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam2', 'SSD2')`)
	trip := must(`INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes)
		VALUES (?, 'recording_20260310_100000', '2026-03-10T10:00:00Z', 'recording_20260310_100000', 1, 1000)`, sourceID)
	seg := must(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps)
		VALUES (?, ?, 'front', 'front/2026/03/10/recording_20260310_100000/segment_00000.mp4', 0, '2026-03-10T10:00:00Z', '2026-03-10T10:00:30Z', 30, 1000, 1, 0)`, sourceID, trip)
	det := must(`INSERT INTO detections (segment_id, pts_offset) VALUES (?, 1.0)`, seg)
	must(`INSERT INTO det_vehicles (det_id, type, make, confidence) VALUES (?, 'suv', 'Honda', 0.9)`, det)

	items, _, err := d.CatalogList(ctx, CatalogFilter{Kind: "make", Q: "honda"})
	if err != nil {
		t.Fatalf("CatalogList: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected Honda to be absent before a refresh, got %+v", items)
	}

	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog #2: %v", err)
	}
	items, _, err = d.CatalogList(ctx, CatalogFilter{Kind: "make", Q: "honda"})
	if err != nil {
		t.Fatalf("CatalogList after refresh: %v", err)
	}
	if len(items) != 1 || items[0].Value != "Honda" {
		t.Fatalf("expected Honda after refresh, got %+v", items)
	}
}

func TestCatalogListUnknownKind(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, _, err := d.CatalogList(ctx, CatalogFilter{Kind: "bogus"}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestCatalogListPagination(t *testing.T) {
	d := openTest(t)
	seedCatalogFixture(t, d)
	ctx := context.Background()
	if err := d.RefreshCatalog(ctx); err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}

	items, cursor, err := d.CatalogList(ctx, CatalogFilter{Kind: "make", Limit: 1})
	if err != nil {
		t.Fatalf("CatalogList: %v", err)
	}
	if len(items) != 1 || cursor == "" {
		t.Fatalf("got %d items, cursor %q, want 1 item and a next cursor", len(items), cursor)
	}

	items2, cursor2, err := d.CatalogList(ctx, CatalogFilter{Kind: "make", Limit: 1, Cursor: cursor})
	if err != nil {
		t.Fatalf("CatalogList page 2: %v", err)
	}
	if len(items2) != 1 || cursor2 != "" {
		t.Fatalf("got %d items, cursor %q, want 1 item and no next cursor", len(items2), cursor2)
	}
	if items[0].Value == items2[0].Value {
		t.Fatalf("page 1 and page 2 returned the same value %q", items[0].Value)
	}
}
