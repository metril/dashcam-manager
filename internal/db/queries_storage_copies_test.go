package db

import (
	"strconv"
	"testing"
)

// TestSegmentCopiesOwnStorageAndIngest covers the member-list counterpart
// to SegmentCopyCounts: an SSD-only segment has no copies at all, a
// share-backed segment reports its own storage, and a verified ingest to
// a second share adds a second copy.
func TestSegmentCopiesOwnStorageAndIngest(t *testing.T) {
	f := newDupFixture(t)

	ssdOnly := f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 1000, nil)
	shareBacked := f.segment(t, f.sourceA, "front/x/segment_00001.mp4", 1000, strp("deadbeef"))

	shareC, err := f.d.ShareCreate(f.ctx, ShareCreateParams{Name: "c", Kind: KindLocal, MountPoint: "/mnt/shares/c", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate c: %v", err)
	}
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'clip.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, shareBacked, shareC)

	copies, err := f.d.SegmentCopies(f.ctx, []int64{ssdOnly, shareBacked})
	if err != nil {
		t.Fatalf("SegmentCopies: %v", err)
	}
	if len(copies[ssdOnly]) != 0 {
		t.Errorf("ssdOnly copies = %+v, want empty", copies[ssdOnly])
	}
	got := copies[shareBacked]
	if len(got) != 2 {
		t.Fatalf("shareBacked copies = %+v, want 2", got)
	}
	byStorage := map[string]CopyRef{}
	for _, c := range got {
		byStorage[c.StorageID] = c
	}
	own, ok := byStorage[itoaTest(f.shareA)]
	if !ok || own.Kind != KindLocal || own.RelPath != "front/x/segment_00001.mp4" || !own.Verified {
		t.Errorf("own storage copy = %+v (map=%+v)", own, byStorage)
	}
	ingested, ok := byStorage[itoaTest(shareC)]
	if !ok || ingested.RelPath != "clip.mp4" || !ingested.Verified {
		t.Errorf("ingest copy = %+v (map=%+v)", ingested, byStorage)
	}
}

// TestSegmentCopiesTwoLocalStorages covers a segment ingested to two local
// shares (own storage counts as neither — the segment here is SSD-sourced
// so its "own" copy doesn't count, only the two verified ingests do).
func TestSegmentCopiesTwoLocalStorages(t *testing.T) {
	f := newDupFixture(t)
	seg := f.segment(t, f.ssdSource, "front/x/segment_00002.mp4", 1000, strp("cafe"))

	shareC, err := f.d.ShareCreate(f.ctx, ShareCreateParams{Name: "c", Kind: KindLocal, MountPoint: "/mnt/shares/c", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate c: %v", err)
	}
	shareD, err := f.d.ShareCreate(f.ctx, ShareCreateParams{Name: "d", Kind: KindLocal, MountPoint: "/mnt/shares/d", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate d: %v", err)
	}
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'c.mp4', 1000, 'cafe', '2026-01-01T00:00:00Z')`, seg, shareC)
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'd.mp4', 1000, 'cafe', '2026-01-01T00:00:00Z')`, seg, shareD)

	copies, err := f.d.SegmentCopies(f.ctx, []int64{seg})
	if err != nil {
		t.Fatalf("SegmentCopies: %v", err)
	}
	if len(copies[seg]) != 2 {
		t.Fatalf("copies = %+v, want 2", copies[seg])
	}
}

// TestSegmentCopiesCrossStorageDuplicateViaGroup covers the live bug: a
// segment's own file sits on storage B (share/local-backed) and has no
// ingest/pull provenance FK to any other storage at all, but a *different*
// segment's verified dashcam_pulls row landed on storage A with the exact
// same sha256 (a verified duplicate group once refreshed). copies for the
// storage-B segment must include storage A too — SegmentCopies can't rely
// on provenance rows alone; it has to fall back to the precomputed
// duplicate group like ExportCopies already does.
func TestSegmentCopiesCrossStorageDuplicateViaGroup(t *testing.T) {
	f := newDupFixture(t)

	// The segment whose copies we're asking about: its own file is on
	// storage A (sourceA), sha256 "shared".
	segOnA := f.segment(t, f.sourceA, "front/x/segment_39051.mp4", 1000, strp("shared"))

	// A different, unrelated segment (indexed via a dashcam pull) whose
	// pull landed on storage B with the identical content.
	otherSeg := f.segment(t, f.ssdSource, "front/x/segment_00099.mp4", 1000, strp("shared"))
	f.exec(t, `INSERT INTO dashcam_pulls (dashcam_source_id, remote_rel_path, indexed_segment_id, share_id, dest_rel_path, bytes, sha256, pulled_at, verified_at)
		VALUES (?, 'remote/x.mp4', ?, ?, 'pulled.mp4', 1000, 'shared', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, f.ssdSource, otherSeg, f.shareB)

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	copies, err := f.d.SegmentCopies(f.ctx, []int64{segOnA})
	if err != nil {
		t.Fatalf("SegmentCopies: %v", err)
	}
	got := copies[segOnA]
	byStorage := map[string]CopyRef{}
	for _, c := range got {
		byStorage[c.StorageID] = c
	}
	if len(got) != 2 {
		t.Fatalf("copies = %+v, want 2 (own storage A + duplicate group member on storage B)", got)
	}
	own, ok := byStorage[itoaTest(f.shareA)]
	if !ok || !own.Verified {
		t.Errorf("own storage A copy = %+v (map=%+v)", own, byStorage)
	}
	other, ok := byStorage[itoaTest(f.shareB)]
	if !ok || other.RelPath != "pulled.mp4" || !other.Verified {
		t.Errorf("cross-storage B copy (via group) = %+v (map=%+v)", other, byStorage)
	}
}

// TestSegmentCopiesEmptyInput covers the zero-length shortcut.
func TestSegmentCopiesEmptyInput(t *testing.T) {
	f := newDupFixture(t)
	copies, err := f.d.SegmentCopies(f.ctx, nil)
	if err != nil {
		t.Fatalf("SegmentCopies: %v", err)
	}
	if len(copies) != 0 {
		t.Errorf("copies = %+v, want empty", copies)
	}
}

// TestSegmentsOnSSDOwnSourceAndArchived covers on_ssd's core rule: a
// segment whose own source is kind=ssd and not archived is on_ssd; an
// archived one (a re-scan found its SSD file gone, backed by a verified
// ingest) is not; a share-backed segment with no SSD row at all is not.
func TestSegmentsOnSSDOwnSourceAndArchived(t *testing.T) {
	f := newDupFixture(t)

	onSSD := f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 1000, nil)
	archived := f.segment(t, f.ssdSource, "front/x/segment_00001.mp4", 1000, nil)
	f.exec(t, `UPDATE segments SET archived_at = '2026-01-02T00:00:00Z' WHERE id = ?`, archived)
	shareOnly := f.segment(t, f.sourceA, "front/x/segment_00002.mp4", 1000, strp("beef"))

	onSSDMap, err := f.d.SegmentsOnSSD(f.ctx, []int64{onSSD, archived, shareOnly})
	if err != nil {
		t.Fatalf("SegmentsOnSSD: %v", err)
	}
	if !onSSDMap[onSSD] {
		t.Errorf("onSSD = %v, want true", onSSDMap[onSSD])
	}
	if onSSDMap[archived] {
		t.Errorf("archived = %v, want false", onSSDMap[archived])
	}
	if onSSDMap[shareOnly] {
		t.Errorf("shareOnly = %v, want false", onSSDMap[shareOnly])
	}
}

// TestSegmentsOnSSDSameContentMatch covers "or a same-content segment": a
// share-backed segment whose sha256 matches a different, non-archived
// SSD-sourced segment's sha256 is on_ssd, even though its own source isn't
// the SSD.
func TestSegmentsOnSSDSameContentMatch(t *testing.T) {
	f := newDupFixture(t)
	f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 1000, strp("shared"))
	shareBacked := f.segment(t, f.sourceA, "front/x/segment_00001.mp4", 1000, strp("shared"))

	onSSDMap, err := f.d.SegmentsOnSSD(f.ctx, []int64{shareBacked})
	if err != nil {
		t.Fatalf("SegmentsOnSSD: %v", err)
	}
	if !onSSDMap[shareBacked] {
		t.Errorf("shareBacked = %v, want true (same-content SSD match)", onSSDMap[shareBacked])
	}
}

// TestExportCopiesCacheAndShare covers ExportCopies' base case: a
// cache-target export and a share-target export each report their own
// storage as a copy even with no cross-storage duplicate.
func TestExportCopiesCacheAndShare(t *testing.T) {
	f := newDupFixture(t)
	cacheExport := f.exec(t, `INSERT INTO exports (name, path, bytes) VALUES ('e1', '/data/cache/exports/e1.mp4', 500)`)
	shareExport := f.exec(t, `INSERT INTO exports (name, path, share_id, bytes) VALUES ('e2', '/mnt/shares/a/e2.mp4', ?, 700)`, f.shareA)

	copies, err := f.d.ExportCopies(f.ctx, []int64{cacheExport, shareExport})
	if err != nil {
		t.Fatalf("ExportCopies: %v", err)
	}
	if len(copies[cacheExport]) != 1 || copies[cacheExport][0].StorageID != "cache" || copies[cacheExport][0].Kind != "cache" {
		t.Errorf("cacheExport copies = %+v", copies[cacheExport])
	}
	if len(copies[shareExport]) != 1 || copies[shareExport][0].StorageID != itoaTest(f.shareA) || copies[shareExport][0].Kind != KindLocal {
		t.Errorf("shareExport copies = %+v", copies[shareExport])
	}
}

// TestExportCopiesCrossStorageDuplicate covers the cross-storage case: an
// export whose file also shows up (via a scanned storage_file with a
// matching sha256) on a different share is reported as having a copy
// there too, once DuplicateReportRefresh has run.
func TestExportCopiesCrossStorageDuplicate(t *testing.T) {
	f := newDupFixture(t)
	shareExport := f.exec(t, `INSERT INTO exports (name, path, share_id, bytes) VALUES ('e1', '/mnt/shares/a/clip.mp4', ?, 1000)`, f.shareA)
	f.storageFile(t, f.shareB, "clip.mp4", 1000, strp("deadbeef"))
	// Give the export's own file the same hash by hashing it as a
	// storage_file row is not possible (exports aren't storage_files), so
	// instead this test relies on the size_name probable path: same
	// bytes+basename as the shareB storage_file, unhashed.
	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	copies, err := f.d.ExportCopies(f.ctx, []int64{shareExport})
	if err != nil {
		t.Fatalf("ExportCopies: %v", err)
	}
	got := copies[shareExport]
	if len(got) != 2 {
		t.Fatalf("copies = %+v, want 2 (own share + shareB duplicate)", got)
	}
	var sawB bool
	for _, c := range got {
		if c.StorageID == itoaTest(f.shareB) {
			sawB = true
			if c.Verified {
				t.Errorf("shareB copy verified = true, want false (size_name, unhashed)")
			}
		}
	}
	if !sawB {
		t.Errorf("copies = %+v, want a shareB member", got)
	}
}

// TestExportCopiesEmptyInput covers the zero-length shortcut.
func TestExportCopiesEmptyInput(t *testing.T) {
	f := newDupFixture(t)
	copies, err := f.d.ExportCopies(f.ctx, nil)
	if err != nil {
		t.Fatalf("ExportCopies: %v", err)
	}
	if len(copies) != 0 {
		t.Errorf("copies = %+v, want empty", copies)
	}
}

func itoaTest(id int64) string {
	return strconv.FormatInt(id, 10)
}
