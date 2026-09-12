package main

import (
	"context"
	"net/http"
	"testing"

	"dashcam/internal/db"
)

// handlerCopyFixture seeds a small local-storage setup shared by the
// task 9 handler tests: an SSD source, one local share with a share-backed
// source, and a second local share for cross-storage cases.
type handlerCopyFixture struct {
	t         *testing.T
	store     *db.DB
	ssdSource int64
	shareA    int64
	sourceA   int64
	shareB    int64
}

func newHandlerCopyFixture(t *testing.T, store *db.DB) *handlerCopyFixture {
	t.Helper()
	shareA, err := store.ShareCreate(context.Background(), db.ShareCreateParams{Name: "a", Kind: db.KindLocal, MountPoint: "/mnt/shares/a", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate a: %v", err)
	}
	shareB, err := store.ShareCreate(context.Background(), db.ShareCreateParams{Name: "b", Kind: db.KindLocal, MountPoint: "/mnt/shares/b", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate b: %v", err)
	}
	must := func(query string, args ...any) int64 {
		res, err := store.Write().ExecContext(context.Background(), query, args...)
		if err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	ssdSource := must(`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)
	sourceA := must(`INSERT INTO sources (kind, share_id, root_path) VALUES ('share', ?, '/mnt/shares/a')`, shareA)
	return &handlerCopyFixture{t: t, store: store, ssdSource: ssdSource, shareA: shareA, sourceA: sourceA, shareB: shareB}
}

func (f *handlerCopyFixture) exec(query string, args ...any) int64 {
	res, err := f.store.Write().ExecContext(context.Background(), query, args...)
	if err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestSegmentGetCopiesSSDOnly covers an SSD-only segment: copies is empty,
// on_ssd is true.
func TestSegmentGetCopiesSSDOnly(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	f := newHandlerCopyFixture(t, store)
	segID := f.exec(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes) VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 1000)`, f.ssdSource)

	var meta db.SegmentMeta
	rec := getJSON(t, mux, "/api/segments/"+itoa(segID), &meta)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(meta.Copies) != 0 {
		t.Errorf("copies = %+v, want empty", meta.Copies)
	}
	if !meta.OnSSD {
		t.Errorf("on_ssd = false, want true")
	}
}

// TestSegmentGetCopiesIngested covers an ingested segment: one copy
// (the destination share), on_ssd still true (SSD file not archived).
func TestSegmentGetCopiesIngested(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	f := newHandlerCopyFixture(t, store)
	segID := f.exec(`INSERT INTO segments (source_id, camera, rel_path, start_time, bytes, sha256) VALUES (?, 'front', 'front/x/segment_00000.mp4', '2026-01-01T00:00:00Z', 1000, 'deadbeef')`, f.ssdSource)
	f.exec(`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'clip.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, segID, f.shareA)

	var meta db.SegmentMeta
	rec := getJSON(t, mux, "/api/segments/"+itoa(segID), &meta)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(meta.Copies) != 1 {
		t.Fatalf("copies = %+v, want 1", meta.Copies)
	}
	if meta.Copies[0].StorageID != itoa(f.shareA) || meta.Copies[0].RelPath != "clip.mp4" || !meta.Copies[0].Verified {
		t.Errorf("copies[0] = %+v", meta.Copies[0])
	}
	if !meta.OnSSD {
		t.Errorf("on_ssd = false, want true")
	}
}

// TestTripGetCopiesTwoLocalStorages covers a segment ingested to two local
// storages, via the trip detail endpoint (batched, no N+1).
func TestTripGetCopiesTwoLocalStorages(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	f := newHandlerCopyFixture(t, store)
	tripID := f.exec(`INSERT INTO trips (source_id, trip_key, started_at, segment_count, bytes) VALUES (?, 'trip1', '2026-01-01T00:00:00Z', 1, 1000)`, f.ssdSource)
	segID := f.exec(`INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, bytes, sha256) VALUES (?, ?, 'front', 'front/x/segment_00000.mp4', 0, '2026-01-01T00:00:00Z', 1000, 'deadbeef')`, f.ssdSource, tripID)
	f.exec(`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'a.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, segID, f.shareA)
	f.exec(`INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'b.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, segID, f.shareB)

	var trip db.TripDetail
	rec := getJSON(t, mux, "/api/trips/"+itoa(tripID), &trip)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(trip.Segments) != 1 {
		t.Fatalf("segments = %+v", trip.Segments)
	}
	if len(trip.Segments[0].Copies) != 2 {
		t.Fatalf("copies = %+v, want 2", trip.Segments[0].Copies)
	}
}

// TestExportsGetCacheExportCopies covers a cache-target export: one copy,
// its own cache storage.
func TestExportsGetCacheExportCopies(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	f := newHandlerCopyFixture(t, store)
	f.exec(`INSERT INTO exports (name, path, bytes) VALUES ('e1', '/data/cache/exports/e1.mp4', 500)`)

	var list []db.Export
	rec := getJSON(t, mux, "/api/exports", &list)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v, want 1", list)
	}
	if len(list[0].Copies) != 1 || list[0].Copies[0].StorageID != "cache" || list[0].Copies[0].Kind != "cache" {
		t.Errorf("copies = %+v", list[0].Copies)
	}
}
