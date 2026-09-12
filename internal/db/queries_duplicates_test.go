package db

import (
	"context"
	"fmt"
	"testing"
)

// dupFixture is a small helper shared by the duplicate-detection tests:
// two shares (both kind=local for simplicity — the view doesn't care about
// kind), each with a share-backed source, so tests can put "copies" of a
// file on two distinct persistent storages.
type dupFixture struct {
	d         *DB
	ctx       context.Context
	shareA    int64
	shareB    int64
	sourceA   int64
	sourceB   int64
	ssdSource int64
}

func newDupFixture(t *testing.T) *dupFixture {
	d := openTest(t)
	ctx := context.Background()

	shareA, err := d.ShareCreate(ctx, ShareCreateParams{Name: "a", Kind: KindLocal, MountPoint: "/mnt/shares/a", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate a: %v", err)
	}
	shareB, err := d.ShareCreate(ctx, ShareCreateParams{Name: "b", Kind: KindLocal, MountPoint: "/mnt/shares/b", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate b: %v", err)
	}

	exec := func(query string, args ...any) int64 {
		res, err := d.Write().ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	sourceA := exec(`INSERT INTO sources (kind, share_id, root_path) VALUES ('share', ?, '/mnt/shares/a')`, shareA)
	sourceB := exec(`INSERT INTO sources (kind, share_id, root_path) VALUES ('share', ?, '/mnt/shares/b')`, shareB)
	ssdSource := exec(`INSERT INTO sources (kind, root_path) VALUES ('ssd', '/mnt/dashcam')`)

	return &dupFixture{d: d, ctx: ctx, shareA: shareA, shareB: shareB, sourceA: sourceA, sourceB: sourceB, ssdSource: ssdSource}
}

func (f *dupFixture) exec(t *testing.T, query string, args ...any) int64 {
	res, err := f.d.Write().ExecContext(f.ctx, query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *dupFixture) segment(t *testing.T, sourceID int64, relPath string, bytes int64, sha256 *string) int64 {
	return f.exec(t, `INSERT INTO segments (source_id, camera, rel_path, start_time, bytes, sha256) VALUES (?, 'front', ?, '2026-01-01T00:00:00Z', ?, ?)`,
		sourceID, relPath, bytes, sha256)
}

func (f *dupFixture) storageFile(t *testing.T, shareID int64, relPath string, bytes int64, sha256 *string) int64 {
	return f.exec(t, `INSERT INTO storage_files (share_id, rel_path, bytes, mtime, sha256, hashed_at, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, 0, ?, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, shareID, relPath, bytes, sha256)
}

func strp(s string) *string { return &s }

// TestDuplicateReportRefreshVerifiedSHA256Group covers the core "verified"
// case: two file_copies rows (here, storage_files on two distinct shares)
// with the same non-empty sha256 form a sha256-kind group.
func TestDuplicateReportRefreshVerifiedSHA256Group(t *testing.T) {
	f := newDupFixture(t)
	f.storageFile(t, f.shareA, "clip.mp4", 1000, strp("deadbeef"))
	f.storageFile(t, f.shareB, "clip.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	g := groups[0]
	if g.Kind != DuplicateGroupKindSHA256 {
		t.Errorf("kind = %q, want sha256", g.Kind)
	}
	if g.Copies != 2 || g.Storages != 2 || g.Bytes != 1000 {
		t.Errorf("copies/storages/bytes = %d/%d/%d, want 2/2/1000", g.Copies, g.Storages, g.Bytes)
	}
	if len(g.Members) != 2 {
		t.Fatalf("members = %d, want 2: %+v", len(g.Members), g.Members)
	}
}

// TestDuplicateReportRefreshProbableSizeNameGroup covers the "probable"
// case: same bytes + basename, on two distinct storages, at least one
// member unhashed.
func TestDuplicateReportRefreshProbableSizeNameGroup(t *testing.T) {
	f := newDupFixture(t)
	f.storageFile(t, f.shareA, "clip.mp4", 500, nil)
	f.storageFile(t, f.shareB, "clip.mp4", 500, nil)

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	if groups[0].Kind != DuplicateGroupKindSizeName {
		t.Errorf("kind = %q, want size_name", groups[0].Kind)
	}
	if groups[0].Copies != 2 || groups[0].Storages != 2 {
		t.Errorf("copies/storages = %d/%d, want 2/2", groups[0].Copies, groups[0].Storages)
	}
}

// TestDuplicateReportListMemberHashedIsPerMember covers fix round 1,
// Important 3: a "size_name" (probable) group's members aren't uniformly
// hashed or not (that's exactly why it's probable, not verified), so each
// DuplicateGroupMember.Hashed must reflect its own file_copies.sha256
// presence rather than the group's Kind.
func TestDuplicateReportListMemberHashedIsPerMember(t *testing.T) {
	f := newDupFixture(t)
	f.storageFile(t, f.shareA, "clip.mp4", 500, strp("deadbeef"))
	f.storageFile(t, f.shareB, "clip.mp4", 500, nil)

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, _, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(groups) != 1 || groups[0].Kind != DuplicateGroupKindSizeName {
		t.Fatalf("groups = %+v, want one size_name group", groups)
	}
	if len(groups[0].Members) != 2 {
		t.Fatalf("members = %+v, want 2", groups[0].Members)
	}
	hashedByShare := map[int64]bool{}
	for _, m := range groups[0].Members {
		if m.ShareID == nil {
			t.Fatalf("member %+v: want ShareID set", m)
		}
		hashedByShare[*m.ShareID] = m.Hashed
	}
	if !hashedByShare[f.shareA] {
		t.Errorf("shareA member Hashed = false, want true (it has a sha256)")
	}
	if hashedByShare[f.shareB] {
		t.Errorf("shareB member Hashed = true, want false (no sha256)")
	}
}

// TestDuplicateReportRefreshHashedDifferentDropped covers "hashed and
// different -> dropped": same bytes+basename, both hashed, but the hashes
// disagree — no group of either kind should be reported.
func TestDuplicateReportRefreshHashedDifferentDropped(t *testing.T) {
	f := newDupFixture(t)
	f.storageFile(t, f.shareA, "clip.mp4", 500, strp("hash1"))
	f.storageFile(t, f.shareB, "clip.mp4", 500, strp("hash2"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 || len(groups) != 0 {
		t.Fatalf("total=%d groups=%d, want 0/0: %+v", total, len(groups), groups)
	}
}

// TestDuplicateReportRefreshSameStorageNotDuplicate covers "a group needs
// >=2 members on DISTINCT storage keys": two same-hash rows on the SAME
// share never form a group, even though there are two of them.
func TestDuplicateReportRefreshSameStorageNotDuplicate(t *testing.T) {
	f := newDupFixture(t)
	f.storageFile(t, f.shareA, "clip.mp4", 1000, strp("deadbeef"))
	f.storageFile(t, f.shareA, "clip_copy.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	_, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 {
		t.Fatalf("total = %d, want 0 (same-storage rows never form a cross-storage group)", total)
	}
}

// TestDuplicateReportRefreshSSDSegmentNeverACopy covers "the SSD is never
// a storage": an SSD-sourced segment (source.share_id NULL) sharing a
// sha256 with a persistent-storage copy must not appear in file_copies at
// all, so it can't inflate a group's copies/storages count.
func TestDuplicateReportRefreshSSDSegmentNeverACopy(t *testing.T) {
	f := newDupFixture(t)
	f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 1000, strp("deadbeef"))
	f.storageFile(t, f.shareA, "clip.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	_, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Only one persistent-storage copy exists (shareA); the SSD segment
	// contributes nothing, so there's no group at all (need >= 2).
	if total != 0 {
		t.Fatalf("total = %d, want 0 (SSD segment must not count as a copy)", total)
	}
}

// TestDuplicateReportRefreshCacheExportVsShareExport covers the cache
// pseudo-storage's identity: a share_id-NULL export (cache) and a
// share-target export with the same bytes+basename, both unhashed, form a
// probable group whose members show distinct storage_key values ("cache"
// vs "share:<id>").
func TestDuplicateReportRefreshCacheExportVsShareExport(t *testing.T) {
	f := newDupFixture(t)
	f.exec(t, `INSERT INTO exports (name, share_id, bytes, path) VALUES ('cache-export', NULL, 700, '/data/cache/exports/clip.mp4')`)
	f.exec(t, `INSERT INTO exports (name, share_id, bytes, path) VALUES ('share-export', ?, 700, '/mnt/shares/a/clip.mp4')`, f.shareA)

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	keys := map[string]bool{}
	for _, m := range groups[0].Members {
		keys[m.StorageKey] = true
	}
	if !keys["cache"] {
		t.Errorf("members storage keys = %+v, want one to be %q", keys, "cache")
	}
	wantShareKey := fmt.Sprintf("share:%d", f.shareA)
	found := false
	for k := range keys {
		if k != "cache" {
			found = true
			if k != wantShareKey {
				t.Errorf("share member storage key = %q, want %q", k, wantShareKey)
			}
		}
	}
	if !found {
		t.Errorf("expected a non-cache member, got %+v", keys)
	}
}

// TestDuplicateReportRefreshExportInheritsStorageFileHash covers fix round
// 1, Important 3: an export landed on a share target is also picked up by
// the scan walker into storage_files at the very same path — the export
// must pick up that storage_files row's sha256 (once hash_backfill hashes
// it) rather than staying permanently unhashed and stuck in the probable
// (size_name) bucket forever.
func TestDuplicateReportRefreshExportInheritsStorageFileHash(t *testing.T) {
	f := newDupFixture(t)

	// Export landed on shareA at "clip.mp4" (absolute path under the
	// share's mount_point, matching how ExportFinish stores it).
	f.exec(t, `INSERT INTO exports (name, share_id, bytes, path) VALUES ('e1', ?, 1000, '/mnt/shares/a/clip.mp4')`, f.shareA)
	// The scan walker's own storage_files row for that identical file,
	// now hashed by hash_backfill.
	f.storageFile(t, f.shareA, "clip.mp4", 1000, strp("deadbeef"))
	// A second, independent copy elsewhere so a group is possible.
	f.storageFile(t, f.shareB, "clip.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	g := groups[0]
	if g.Kind != DuplicateGroupKindSHA256 {
		t.Fatalf("kind = %q, want sha256 (export should inherit the storage_files hash, not stay probable): %+v", g.Kind, g)
	}
	// Exactly 2 copies (shareA's file, shareB's file) — the export and its
	// own storage_files row must have been deduped to one member.
	if g.Copies != 2 || g.Storages != 2 {
		t.Fatalf("copies/storages = %d/%d, want 2/2: %+v", g.Copies, g.Storages, g.Members)
	}
	var sawExport bool
	for _, m := range g.Members {
		if m.RefKind == "export" {
			sawExport = true
		}
	}
	if !sawExport {
		t.Errorf("expected an export member (preferred over its storage_files twin per rank), got %+v", g.Members)
	}
}

// TestDuplicateReportRefreshIdempotent covers running the refresh twice in
// a row with no underlying data change: the result must be identical, and
// a second run must not error or accumulate stale rows.
func TestDuplicateReportRefreshIdempotent(t *testing.T) {
	f := newDupFixture(t)
	f.storageFile(t, f.shareA, "clip.mp4", 1000, strp("deadbeef"))
	f.storageFile(t, f.shareB, "clip.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	groups1, total1, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list 1: %v", err)
	}

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	groups2, total2, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}

	if total1 != total2 || len(groups1) != len(groups2) {
		t.Fatalf("refresh not idempotent: (%d,%d) vs (%d,%d)", total1, len(groups1), total2, len(groups2))
	}
	if groups1[0].GroupKey != groups2[0].GroupKey || groups1[0].Copies != groups2[0].Copies {
		t.Errorf("group contents changed across idempotent refresh: %+v vs %+v", groups1[0], groups2[0])
	}
}

// TestDuplicateReportRefreshIngestVerifiedByConstruction covers "ingest
// rows are verified members of their segment's group by construction": a
// segment with a stamped sha256, plus an unrelated storage_files row with
// the same sha256, plus an ingest of that segment onto a THIRD storage
// whose own ingests.sha256 column is NULL — the ingest must still land in
// the verified sha256 group (via its segment's hash), not be dropped or
// downgraded to probable.
func TestDuplicateReportRefreshIngestVerifiedByConstruction(t *testing.T) {
	f := newDupFixture(t)
	segID := f.segment(t, f.sourceA, "front/x/segment_00000.mp4", 1000, strp("deadbeef"))
	f.storageFile(t, f.shareB, "other.mp4", 1000, strp("deadbeef"))

	// A third share to ingest onto, with the ingest's own sha256 left
	// NULL (as if verified by size only) — verified_at is set, which is
	// what makes it a "copy" at all.
	shareC, err := f.d.ShareCreate(f.ctx, ShareCreateParams{Name: "c", Kind: KindLocal, MountPoint: "/mnt/shares/c", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate c: %v", err)
	}
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'clip.mp4', 1000, NULL, '2026-01-01T00:00:00Z')`, segID, shareC)

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	g := groups[0]
	if g.Kind != DuplicateGroupKindSHA256 {
		t.Fatalf("kind = %q, want sha256", g.Kind)
	}
	// segment (shareA), storage_file (shareB), ingest (shareC): 3 members
	// on 3 distinct storages.
	if g.Copies != 3 || g.Storages != 3 {
		t.Errorf("copies/storages = %d/%d, want 3/3: %+v", g.Copies, g.Storages, g.Members)
	}
	var sawIngest bool
	for _, m := range g.Members {
		if m.RefKind == "ingest" {
			sawIngest = true
		}
	}
	if !sawIngest {
		t.Errorf("expected an ingest member in %+v", g.Members)
	}
}

// TestDuplicateReportRefreshStorageFileExcludesSegmentOverlap covers the
// storage_files/segment overlap exclusion: a share-backed source's segment
// mp4 is ALSO walked into storage_files by the scan (task 5's walker
// covers the whole tree), but the SAME file must not count as two copies
// of itself on the same storage.
func TestDuplicateReportRefreshStorageFileExcludesSegmentOverlap(t *testing.T) {
	f := newDupFixture(t)
	f.segment(t, f.sourceA, "front/x/segment_00000.mp4", 1000, strp("deadbeef"))
	// Same file, same share, same rel_path — as if the scan walker also
	// upserted it into storage_files.
	f.storageFile(t, f.shareA, "front/x/segment_00000.mp4", 1000, strp("deadbeef"))
	// One real copy elsewhere, so a group is possible at all.
	f.storageFile(t, f.shareB, "other.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	// Exactly 2 copies (segment on shareA, storage_file on shareB) — the
	// duplicate storage_files row on shareA must have been excluded.
	if groups[0].Copies != 2 || groups[0].Storages != 2 {
		t.Errorf("copies/storages = %d/%d, want 2/2 (storage_file/segment overlap not excluded): %+v",
			groups[0].Copies, groups[0].Storages, groups[0].Members)
	}
}

// TestDuplicateReportRefreshDedupesSegmentAndIngestSamePath covers fix
// round 1, Critical 1: a segment ingested onto shareA, where shareA is
// ALSO scanned as its own kind='share' source, produces both a 'segment'
// file_copies row and an 'ingest' file_copies row for the exact same file
// on shareA (same storage_key, same rel_path) — these must collapse to
// ONE copy, not inflate the group to 3 copies for what is really only 2
// physical files (shareA's one file, shareB's one file).
func TestDuplicateReportRefreshDedupesSegmentAndIngestSamePath(t *testing.T) {
	f := newDupFixture(t)

	// The original SSD-sourced segment that gets ingested onto shareA.
	origSeg := f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 1000, strp("deadbeef"))
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at)
		VALUES (?, ?, 'clip.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, origSeg, f.shareA)

	// shareA is separately scanned as its own kind='share' source
	// (f.sourceA, from the fixture), producing a *second* segments row for
	// the exact same on-disk file the ingest above just wrote.
	f.segment(t, f.sourceA, "clip.mp4", 1000, strp("deadbeef"))

	// A genuinely different storage, so a group is possible at all.
	f.storageFile(t, f.shareB, "clip.mp4", 1000, strp("deadbeef"))

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	groups, total, err := f.d.DuplicateReportList(f.ctx, "", 0, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(groups) != 1 {
		t.Fatalf("total=%d groups=%d, want 1/1: %+v", total, len(groups), groups)
	}
	g := groups[0]
	// Exactly 2 physical files (shareA's clip.mp4, shareB's clip.mp4), not
	// 3 — the segment/ingest pair on shareA must have been deduped to one
	// member.
	if g.Copies != 2 || g.Storages != 2 {
		t.Fatalf("copies/storages = %d/%d, want 2/2 (segment+ingest same-path not deduped): %+v", g.Copies, g.Storages, g.Members)
	}
	if g.Bytes*(g.Storages-1) != g.Reclaimable {
		t.Errorf("reclaimable = %d, want bytes*(storages-1) = %d", g.Reclaimable, g.Bytes*(g.Storages-1))
	}
}

// TestDuplicateReportListFiltersByKindAndStorage covers the kind/storage_id
// query filters.
func TestDuplicateReportListFiltersByKindAndStorage(t *testing.T) {
	f := newDupFixture(t)
	// A verified group across shareA/shareB.
	f.storageFile(t, f.shareA, "verified.mp4", 1000, strp("hashA"))
	f.storageFile(t, f.shareB, "verified.mp4", 1000, strp("hashA"))
	// A probable group across shareA/shareB only (different name/size).
	f.storageFile(t, f.shareA, "probable.mp4", 2000, nil)
	f.storageFile(t, f.shareB, "probable.mp4", 2000, nil)

	if err := f.d.DuplicateReportRefresh(f.ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	shaGroups, shaTotal, err := f.d.DuplicateReportList(f.ctx, DuplicateGroupKindSHA256, 0, 50, 0)
	if err != nil {
		t.Fatalf("list sha256: %v", err)
	}
	if shaTotal != 1 || len(shaGroups) != 1 || shaGroups[0].Kind != DuplicateGroupKindSHA256 {
		t.Fatalf("sha256 filter: total=%d groups=%+v", shaTotal, shaGroups)
	}

	sizeGroups, sizeTotal, err := f.d.DuplicateReportList(f.ctx, DuplicateGroupKindSizeName, 0, 50, 0)
	if err != nil {
		t.Fatalf("list size_name: %v", err)
	}
	if sizeTotal != 1 || len(sizeGroups) != 1 || sizeGroups[0].Kind != DuplicateGroupKindSizeName {
		t.Fatalf("size_name filter: total=%d groups=%+v", sizeTotal, sizeGroups)
	}

	// Both groups touch shareA and shareB, so filtering by either share id
	// should still return both groups.
	_, allTotal, err := f.d.DuplicateReportList(f.ctx, "", f.shareA, 50, 0)
	if err != nil {
		t.Fatalf("list by storage: %v", err)
	}
	if allTotal != 2 {
		t.Fatalf("storage_id filter total = %d, want 2", allTotal)
	}

	// A share with no members at all filters everything out.
	shareC, err := f.d.ShareCreate(f.ctx, ShareCreateParams{Name: "c", Kind: KindLocal, MountPoint: "/mnt/shares/c", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate c: %v", err)
	}
	_, noneTotal, err := f.d.DuplicateReportList(f.ctx, "", shareC, 50, 0)
	if err != nil {
		t.Fatalf("list by empty storage: %v", err)
	}
	if noneTotal != 0 {
		t.Fatalf("storage_id filter (no members) total = %d, want 0", noneTotal)
	}
}

// TestSegmentCopyCounts covers the badge helper: an SSD-only segment (no
// persistent copy) reports zero storages; a share-backed segment reports
// its own storage plus any verified ingest; Verified reflects the
// segment's own sha256, independent of storage count.
func TestSegmentCopyCounts(t *testing.T) {
	f := newDupFixture(t)

	ssdOnly := f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 1000, nil)
	shareBacked := f.segment(t, f.sourceA, "front/x/segment_00001.mp4", 1000, strp("deadbeef"))

	shareC, err := f.d.ShareCreate(f.ctx, ShareCreateParams{Name: "c", Kind: KindLocal, MountPoint: "/mnt/shares/c", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate c: %v", err)
	}
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'clip.mp4', 1000, 'deadbeef', '2026-01-01T00:00:00Z')`, shareBacked, shareC)
	// An unverified ingest of the ssd-only segment must not count.
	f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'clip2.mp4', 1000, NULL, NULL)`, ssdOnly, shareC)

	counts, err := f.d.SegmentCopyCounts(f.ctx, []int64{ssdOnly, shareBacked})
	if err != nil {
		t.Fatalf("SegmentCopyCounts: %v", err)
	}

	if got := counts[ssdOnly]; got.Storages != 0 || got.Verified {
		t.Errorf("ssdOnly = %+v, want {0, false}", got)
	}
	if got := counts[shareBacked]; got.Storages != 2 || !got.Verified {
		t.Errorf("shareBacked = %+v, want {2, true} (own share + verified ingest)", got)
	}
}

// TestSegmentCopyCountsEmptyInput covers the zero-length input shortcut.
func TestSegmentCopyCountsEmptyInput(t *testing.T) {
	f := newDupFixture(t)
	counts, err := f.d.SegmentCopyCounts(f.ctx, nil)
	if err != nil {
		t.Fatalf("SegmentCopyCounts: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("counts = %+v, want empty", counts)
	}
}
