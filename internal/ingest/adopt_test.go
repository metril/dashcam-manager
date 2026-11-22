package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func prePlace(t *testing.T, srcRoot, mount, rel string, mutate func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(srcRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		b = mutate(b)
	}
	d := filepath.Join(mount, rel)
	if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIngestAdoptsIdenticalExistingDest(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	shareID, mount := setupTargetShare(t, store)
	ctx := context.Background()
	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	for _, s := range segs {
		prePlace(t, root, mount, s.RelPath, nil)
	}
	// Sidecar absent on dest for segment 0 must be copied during adoption.
	st, err := Run(ctx, store, mount, Opts{SourceID: sourceID, TargetShareID: shareID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.SegmentsAdopted != 2 || st.SegmentsCopied != 0 || st.BytesCopied != 0 || st.SegmentsFailed != 0 {
		t.Fatalf("stats = %+v, want 2 adopted", st)
	}
	if len(st.VerifiedSegmentIDs) != 2 {
		t.Fatalf("verified ids = %v", st.VerifiedSegmentIDs)
	}
	for _, s := range segs {
		row, _ := store.IngestGet(ctx, s.ID, shareID)
		if row == nil || row.VerifiedAt == nil || row.SHA256 == "" {
			t.Fatalf("row = %+v", row)
		}
		ts := filepath.Join(mount, s.RelPath[:len(s.RelPath)-len(".mp4")]+".timestamp.json")
		if _, err := os.Stat(ts); err != nil {
			t.Fatalf("sidecar not copied on adopt: %v", err)
		}
	}
	// Rerun dedupes.
	st2, _ := Run(ctx, store, mount, Opts{SourceID: sourceID, TargetShareID: shareID}, nil)
	if st2.SegmentsSkipped != 2 || st2.SegmentsAdopted != 0 {
		t.Fatalf("rerun = %+v", st2)
	}
}

func TestIngestSameSizeDifferentContentIsRecopied(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	shareID, mount := setupTargetShare(t, store)
	ctx := context.Background()
	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	prePlace(t, root, mount, segs[0].RelPath, func(b []byte) []byte {
		o := append([]byte(nil), b...)
		o[0] ^= 0xFF
		return o
	})
	st, err := Run(ctx, store, mount, Opts{SourceID: sourceID, TargetShareID: shareID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.SegmentsAdopted != 0 || st.SegmentsCopied != 2 {
		t.Fatalf("stats = %+v, want 2 copied", st)
	}
	a, _ := os.ReadFile(filepath.Join(root, segs[0].RelPath))
	b, _ := os.ReadFile(filepath.Join(mount, segs[0].RelPath))
	if string(a) != string(b) {
		t.Fatal("dest must be overwritten with source content")
	}
}

func TestIngestStalePullHashDoesNotAdoptCorruptedDest(t *testing.T) {
	store := openTestDB(t)
	sourceID, root := setupSource(t, store)
	shareID, mount := setupTargetShare(t, store)
	ctx := context.Background()
	segs, _ := store.IngestCandidates(ctx, sourceID, nil)
	seg := segs[0]
	src, _ := os.ReadFile(filepath.Join(root, seg.RelPath))
	sum := sha256.Sum256(src)
	// Same-size corrupted dest plus a stale pull row carrying the source's
	// hash: the dest must be hashed itself, so it is recopied, not adopted.
	prePlace(t, root, mount, seg.RelPath, func(b []byte) []byte {
		o := append([]byte(nil), b...)
		o[0] ^= 0xFF
		return o
	})
	if _, err := store.Write().ExecContext(ctx, `INSERT INTO dashcam_pulls (dashcam_source_id, remote_rel_path, share_id, dest_rel_path, bytes, sha256, pulled_at, verified_at) VALUES (?, 'r', ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		sourceID, shareID, seg.RelPath, seg.Bytes, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	st, err := Run(ctx, store, mount, Opts{SourceID: sourceID, TargetShareID: shareID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.SegmentsAdopted != 0 || st.SegmentsCopied != 2 {
		t.Fatalf("stats = %+v, want 2 copied, 0 adopted", st)
	}
	b, _ := os.ReadFile(filepath.Join(mount, seg.RelPath))
	if string(b) != string(src) {
		t.Fatal("dest must be overwritten with source content")
	}
}
