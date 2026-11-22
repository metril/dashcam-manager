package ingest

import (
	"context"
	"dashcam/internal/db"
	"os"
	"path/filepath"
	"testing"
)

func ingestAll(t *testing.T) (store *db.DB, sourceID int64, root string, mount string, shareID int64) {
	t.Helper()
	store = openTestDB(t)
	sourceID, root = setupSource(t, store)
	shareID, mount = setupTargetShare(t, store)
	if _, err := Run(context.Background(), store, mount, Opts{SourceID: sourceID, TargetShareID: shareID}, nil); err != nil {
		t.Fatal(err)
	}
	return
}

func TestCleanupDryRunDoesNotHash(t *testing.T) {
	store, sourceID, root, mount, _ := ingestAll(t)
	ctx := context.Background()
	cands, _ := store.CleanupCandidates(ctx, sourceID, nil, nil)
	// Same-size corruption: a hash check would refuse it, a size-only preview must not.
	d := filepath.Join(mount, cands[0].DestRelPath)
	b, _ := os.ReadFile(d)
	for i := range b {
		b[i] ^= 0xFF
	}
	_ = os.WriteFile(d, b, 0o644)

	prev, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prev.SegmentsDeleted != 2 || prev.DestMismatch != 0 || prev.Note == "" {
		t.Fatalf("preview = %+v, want 2 previewed, no hashing", prev)
	}
	real, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if real.SegmentsDeleted != 1 || real.DestMismatch != 1 || real.SegmentsSkipped != 1 {
		t.Fatalf("real = %+v, want 1 deleted, 1 dest_mismatch", real)
	}
	if _, err := os.Stat(filepath.Join(root, cands[0].RelPath)); err != nil {
		t.Fatalf("mismatched SSD original must remain: %v", err)
	}
}

func TestCleanupSkipReasonCounters(t *testing.T) {
	store, sourceID, root, mount, shareID := ingestAll(t)
	ctx := context.Background()
	cands, _ := store.CleanupCandidates(ctx, sourceID, nil, nil)
	if len(cands) != 2 {
		t.Fatalf("cands = %d", len(cands))
	}
	// 0: SSD file missing; 1: SSD size changed.
	_ = os.Remove(filepath.Join(root, cands[0].RelPath))
	_ = os.WriteFile(filepath.Join(root, cands[1].RelPath), []byte("x"), 0o644)
	st, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.MissingOnSSD != 1 || st.SizeMismatch != 1 || st.SegmentsDeleted != 0 {
		t.Fatalf("stats = %+v", st)
	}

	// dest missing -> dest_mismatch (even in preview, size/exists check).
	_ = os.WriteFile(filepath.Join(root, cands[1].RelPath), make([]byte, cands[1].IngestBytes), 0o644)
	_ = os.Remove(filepath.Join(mount, cands[1].DestRelPath))
	st, _ = Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: true}, nil)
	if st.DestMismatch != 1 {
		t.Fatalf("stats = %+v, want dest_mismatch 1", st)
	}

	// unresolved share / unsafe path.
	if _, err := store.Write().ExecContext(ctx, `UPDATE shares SET mount_point = '' WHERE id = ?`, shareID); err != nil {
		t.Fatal(err)
	}
	st, _ = Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: true}, nil)
	if st.DestUnresolved != 1 {
		t.Fatalf("stats = %+v, want dest_unresolved 1", st)
	}
	if _, err := store.Write().ExecContext(ctx, `UPDATE ingests SET dest_rel_path = '../evil' WHERE segment_id = ?`, cands[1].SegmentID); err != nil {
		t.Fatal(err)
	}
	st, _ = Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: true}, nil)
	if st.UnsafePath != 1 {
		t.Fatalf("stats = %+v, want unsafe_path 1", st)
	}
}
