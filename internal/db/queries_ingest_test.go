package db

import "testing"

func TestSSDIngestStatsSplitsArchived(t *testing.T) {
	f := newDupFixture(t)
	notIng := f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 10, nil)
	_ = notIng
	onSSD := f.segment(t, f.ssdSource, "front/x/segment_00001.mp4", 10, nil)
	arch := f.segment(t, f.ssdSource, "front/x/segment_00002.mp4", 10, nil)
	archNoIng := f.segment(t, f.ssdSource, "front/x/segment_00003.mp4", 10, nil)
	for _, id := range []int64{onSSD, arch} {
		f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'x', 10, 'aa', '2026-01-01T00:00:00Z')`, id, f.shareA)
	}
	f.exec(t, `UPDATE segments SET archived_at = '2026-01-02T00:00:00Z' WHERE id IN (?, ?)`, arch, archNoIng)

	st, err := f.d.SSDIngestStatsGet(f.ctx, f.ssdSource)
	if err != nil {
		t.Fatal(err)
	}
	if st.SegmentsTotal != 2 || st.SegmentsNotIngested != 1 || st.SegmentsIngestedOnSSD != 1 || st.SegmentsArchived != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestCleanupCandidatesExcludesArchived(t *testing.T) {
	f := newDupFixture(t)
	live := f.segment(t, f.ssdSource, "front/x/segment_00000.mp4", 10, nil)
	arch := f.segment(t, f.ssdSource, "front/x/segment_00001.mp4", 10, nil)
	for _, id := range []int64{live, arch} {
		f.exec(t, `INSERT INTO ingests (segment_id, share_id, dest_rel_path, bytes, sha256, verified_at) VALUES (?, ?, 'x', 10, 'aa', '2026-01-01T00:00:00Z')`, id, f.shareA)
	}
	f.exec(t, `UPDATE segments SET archived_at = '2026-01-02T00:00:00Z' WHERE id = ?`, arch)
	got, err := f.d.CleanupCandidates(f.ctx, f.ssdSource, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SegmentID != live {
		t.Fatalf("candidates = %+v, want only %d", got, live)
	}
	ing, _ := f.d.IngestCandidates(f.ctx, f.ssdSource, nil)
	if len(ing) != 1 {
		t.Fatalf("ingest candidates = %d, want 1 (archived excluded)", len(ing))
	}
}
