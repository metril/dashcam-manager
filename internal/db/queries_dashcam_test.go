package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDashcamCRUDRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)

	if got, err := d.DashcamList(ctx); err != nil || len(got) != 0 {
		t.Fatalf("DashcamList on empty db = (%v, %v), want (empty, nil)", got, err)
	}

	id, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	got, err := d.DashcamGet(ctx, id)
	if err != nil {
		t.Fatalf("DashcamGet: %v", err)
	}
	if got.Kind != "dashcam" || got.Label != "Front Cam" || got.Status != "unknown" || got.RootPath != "" {
		t.Fatalf("DashcamGet = %+v, want kind=dashcam label=Front Cam status=unknown root_path=''", got)
	}
	if got.BaseURL == nil || *got.BaseURL != "http://10.0.0.9" {
		t.Fatalf("DashcamGet BaseURL = %v, want http://10.0.0.9", got.BaseURL)
	}
	if got.TargetShareID == nil || *got.TargetShareID != shareID {
		t.Fatalf("DashcamGet TargetShareID = %v, want %d", got.TargetShareID, shareID)
	}

	list, err := d.DashcamList(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("DashcamList = (%v, %v), want [id=%d]", list, err, id)
	}

	otherShareRes, err := d.Write().ExecContext(ctx,
		`INSERT INTO shares (name, server, share) VALUES ('nas2', '10.0.0.6', 'archive')`)
	if err != nil {
		t.Fatalf("insert second share: %v", err)
	}
	otherShareID, _ := otherShareRes.LastInsertId()

	if err := d.DashcamUpdate(ctx, id, "Rear Cam", "http://10.0.0.10", otherShareID, false); err != nil {
		t.Fatalf("DashcamUpdate: %v", err)
	}
	got, err = d.DashcamGet(ctx, id)
	if err != nil {
		t.Fatalf("DashcamGet after update: %v", err)
	}
	if got.Label != "Rear Cam" || *got.BaseURL != "http://10.0.0.10" || *got.TargetShareID != otherShareID {
		t.Fatalf("DashcamGet after update = %+v", got)
	}

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := d.DashcamSetCursor(ctx, id, "cursor-123", now); err != nil {
		t.Fatalf("DashcamSetCursor: %v", err)
	}
	got, err = d.DashcamGet(ctx, id)
	if err != nil {
		t.Fatalf("DashcamGet after set cursor: %v", err)
	}
	if got.PullCursor == nil || *got.PullCursor != "cursor-123" {
		t.Fatalf("PullCursor = %v, want cursor-123", got.PullCursor)
	}
	if got.LastPullAt == nil || *got.LastPullAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("LastPullAt = %v, want %s", got.LastPullAt, now.Format(time.RFC3339Nano))
	}

	if err := d.DashcamDelete(ctx, id); err != nil {
		t.Fatalf("DashcamDelete: %v", err)
	}
	if _, err := d.DashcamGet(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DashcamGet after delete: err = %v, want ErrNotFound", err)
	}
	if err := d.DashcamDelete(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DashcamDelete on already-deleted id: err = %v, want ErrNotFound", err)
	}
}

// TestDashcamGetRejectsNonDashcamSource covers the "kind='dashcam'" scoping
// on Dashcam* funcs: an ordinary ssd/share source's id must not be
// reachable through them.
func TestDashcamGetRejectsNonDashcamSource(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	id, err := d.SourceCreate(ctx, "ssd", "/mnt/dashcam", "SSD")
	if err != nil {
		t.Fatalf("SourceCreate: %v", err)
	}
	if _, err := d.DashcamGet(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DashcamGet on an ssd source: err = %v, want ErrNotFound", err)
	}
	if err := d.DashcamUpdate(ctx, id, "x", "http://x", 0, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DashcamUpdate on an ssd source: err = %v, want ErrNotFound", err)
	}
	if err := d.DashcamDelete(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DashcamDelete on an ssd source: err = %v, want ErrNotFound", err)
	}
}

func strPtr(s string) *string { return &s }

func TestPullUpsertAndGetRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	if _, found, err := d.PullGet(ctx, dashcamID, "clips/a.mp4"); err != nil || found {
		t.Fatalf("PullGet before any pull = (found=%v, %v), want (false, nil)", found, err)
	}

	id, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID,
		RemoteRelPath:   "clips/a.mp4",
		ShareID:         shareID,
		DestRelPath:     "front/a.mp4",
		Bytes:           1000,
		SHA256:          strPtr("aaa"),
		PulledAt:        "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("PullUpsert (insert): %v", err)
	}

	got, found, err := d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil || !found {
		t.Fatalf("PullGet after insert = (found=%v, %v), want (true, nil)", found, err)
	}
	if got.ID != id || got.Bytes != 1000 || *got.SHA256 != "aaa" || got.DestRelPath != "front/a.mp4" {
		t.Fatalf("PullGet after insert = %+v", got)
	}

	if err := d.PullMarkVerified(ctx, id, "aaa", time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("PullMarkVerified: %v", err)
	}
	got, _, err = d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet after verify: %v", err)
	}
	if got.VerifiedAt == nil {
		t.Fatal("expected VerifiedAt to be set after PullMarkVerified")
	}

	// Re-upsert with identical bytes/sha256 must NOT clear verified_at.
	id2, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID,
		RemoteRelPath:   "clips/a.mp4",
		ShareID:         shareID,
		DestRelPath:     "front/a.mp4",
		Bytes:           1000,
		SHA256:          strPtr("aaa"),
		PulledAt:        "2026-01-02T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("PullUpsert (repeat, unchanged): %v", err)
	}
	if id2 != id {
		t.Fatalf("PullUpsert on the same (dashcam_source_id, remote_rel_path) returned a new id: %d != %d", id2, id)
	}
	got, _, err = d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet after unchanged re-upsert: %v", err)
	}
	if got.VerifiedAt == nil {
		t.Fatal("PullUpsert with unchanged bytes/sha256 must not clear verified_at")
	}
	if got.PulledAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("PulledAt after re-upsert = %q, want the new pulled_at to still be recorded", got.PulledAt)
	}

	// Re-upsert with a different sha256 (remote file changed) MUST clear
	// verified_at.
	if _, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID,
		RemoteRelPath:   "clips/a.mp4",
		ShareID:         shareID,
		DestRelPath:     "front/a.mp4",
		Bytes:           1000,
		SHA256:          strPtr("bbb"),
		PulledAt:        "2026-01-03T00:00:00Z",
	}); err != nil {
		t.Fatalf("PullUpsert (changed sha256): %v", err)
	}
	got, _, err = d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet after changed re-upsert: %v", err)
	}
	if got.VerifiedAt != nil {
		t.Fatal("PullUpsert with a changed sha256 must clear verified_at")
	}
	if *got.SHA256 != "bbb" {
		t.Fatalf("SHA256 after changed re-upsert = %v, want bbb", got.SHA256)
	}
}

func TestPullCleanupCandidatesAndMarkDeletedRemote(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	// A share-kind source and a segment under it, to link one pull to.
	shareSourceID, err := d.SourceCreate(ctx, "share", "/mnt/shares/nas", "NAS")
	if err != nil {
		t.Fatalf("SourceCreate (share): %v", err)
	}
	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time) VALUES (?, 'front', 'front/a.mp4', '2026-01-01T00:00:00Z')`,
		shareSourceID)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	segID, _ := res.LastInsertId()

	// Pull A: verified + linked to a segment -> cleanup candidate.
	idA, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/a.mp4", ShareID: shareID,
		DestRelPath: "front/a.mp4", Bytes: 100, PulledAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("PullUpsert A: %v", err)
	}
	if err := d.PullMarkVerified(ctx, idA, "aaa", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified A: %v", err)
	}

	// Pull B: verified but NOT linked to any segment -> not a candidate.
	idB, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/b.mp4", ShareID: shareID,
		DestRelPath: "front/b.mp4", Bytes: 200, PulledAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("PullUpsert B: %v", err)
	}
	if err := d.PullMarkVerified(ctx, idB, "bbb", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified B: %v", err)
	}

	// Pull C: not verified at all -> not a candidate.
	if _, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/c.mp4", ShareID: shareID,
		DestRelPath: "front/c.mp4", Bytes: 300, PulledAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("PullUpsert C: %v", err)
	}

	before, err := d.PullCleanupCandidates(ctx, dashcamID)
	if err != nil {
		t.Fatalf("PullCleanupCandidates before linking: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("PullCleanupCandidates before linking = %d rows, want 0 (nothing is indexed_segment_id-linked yet)", len(before))
	}

	n, err := d.PullsLinkSegments(ctx, dashcamID, shareSourceID)
	if err != nil {
		t.Fatalf("PullsLinkSegments: %v", err)
	}
	if n != 1 {
		t.Fatalf("PullsLinkSegments linked %d rows, want 1 (only pull A's dest_rel_path matches a segment)", n)
	}

	got, _, err := d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet A after link: %v", err)
	}
	if got.IndexedSegmentID == nil || *got.IndexedSegmentID != segID {
		t.Fatalf("pull A IndexedSegmentID = %v, want %d", got.IndexedSegmentID, segID)
	}

	// Re-running PullsLinkSegments must not re-link (0 more rows) — it only
	// targets indexed_segment_id IS NULL.
	n, err = d.PullsLinkSegments(ctx, dashcamID, shareSourceID)
	if err != nil {
		t.Fatalf("PullsLinkSegments (second run): %v", err)
	}
	if n != 0 {
		t.Fatalf("PullsLinkSegments second run linked %d rows, want 0", n)
	}

	candidates, err := d.PullCleanupCandidates(ctx, dashcamID)
	if err != nil {
		t.Fatalf("PullCleanupCandidates after linking: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != idA {
		t.Fatalf("PullCleanupCandidates after linking = %+v, want just pull A (id=%d)", candidates, idA)
	}

	if err := d.PullMarkDeletedRemote(ctx, []int64{idA}, time.Now()); err != nil {
		t.Fatalf("PullMarkDeletedRemote: %v", err)
	}
	got, _, err = d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet A after mark deleted: %v", err)
	}
	if got.DeletedRemoteAt == nil {
		t.Fatal("expected DeletedRemoteAt to be set after PullMarkDeletedRemote")
	}

	after, err := d.PullCleanupCandidates(ctx, dashcamID)
	if err != nil {
		t.Fatalf("PullCleanupCandidates after mark deleted: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("PullCleanupCandidates after mark deleted = %d rows, want 0", len(after))
	}
}

func TestPullMarkDeletedRemoteEmptyIDsNoOp(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.PullMarkDeletedRemote(ctx, nil, time.Now()); err != nil {
		t.Fatalf("PullMarkDeletedRemote(nil): %v", err)
	}
}
