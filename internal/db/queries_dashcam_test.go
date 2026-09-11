package db

import (
	"context"
	"errors"
	"fmt"
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
	wantRootPath := fmt.Sprintf("dashcam:%d", id)
	if got.Kind != "dashcam" || got.Label != "Front Cam" || got.Status != "unknown" || got.RootPath != wantRootPath {
		t.Fatalf("DashcamGet = %+v, want kind=dashcam label=Front Cam status=unknown root_path=%q", got, wantRootPath)
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
	if got.LastPullAt == nil || *got.LastPullAt != now.Format(DashcamTimestampLayout) {
		t.Fatalf("LastPullAt = %v, want %s", got.LastPullAt, now.Format(DashcamTimestampLayout))
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

// TestDashcamCreateTwiceRootPathUnique covers fix round 1 item 1:
// DashcamCreate used to insert root_path ” unconditionally, which
// satisfied sources' UNIQUE(root_path) for exactly one dashcam and then
// failed for every subsequent one.
func TestDashcamCreateTwiceRootPathUnique(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)

	id1, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate (first): %v", err)
	}
	id2, err := d.DashcamCreate(ctx, "Rear Cam", "http://10.0.0.10", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate (second): %v", err)
	}
	if id1 == id2 {
		t.Fatalf("DashcamCreate returned the same id twice: %d", id1)
	}

	got1, err := d.DashcamGet(ctx, id1)
	if err != nil {
		t.Fatalf("DashcamGet id1: %v", err)
	}
	got2, err := d.DashcamGet(ctx, id2)
	if err != nil {
		t.Fatalf("DashcamGet id2: %v", err)
	}
	if got1.RootPath == got2.RootPath {
		t.Fatalf("both dashcams got the same root_path %q", got1.RootPath)
	}
	if got1.RootPath == "" || got2.RootPath == "" {
		t.Fatalf("root_path left empty: %q, %q", got1.RootPath, got2.RootPath)
	}

	list, err := d.DashcamList(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("DashcamList = (%v, %v), want 2 rows", list, err)
	}
}

// TestSourcesListExcludesDashcam covers fix round 1 item 3:
// GET /api/sources (via SourcesList) must not surface kind='dashcam' rows
// as if they were scannable ssd/share sources — DashcamList is the
// dedicated listing for those.
func TestSourcesListExcludesDashcam(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)

	ssdID, err := d.SourceCreate(ctx, "ssd", "/mnt/dashcam", "SSD")
	if err != nil {
		t.Fatalf("SourceCreate (ssd): %v", err)
	}
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	list, err := d.SourcesList(ctx)
	if err != nil {
		t.Fatalf("SourcesList: %v", err)
	}
	for _, s := range list {
		if s.ID == dashcamID {
			t.Fatalf("SourcesList included the dashcam source (id=%d, kind=%s)", s.ID, s.Kind)
		}
	}
	found := false
	for _, s := range list {
		if s.ID == ssdID {
			found = true
		}
	}
	if !found {
		t.Fatal("SourcesList dropped the ssd source too — expected it to still be scoped to non-dashcam kinds only")
	}

	dashcamList, err := d.DashcamList(ctx)
	if err != nil || len(dashcamList) != 1 || dashcamList[0].ID != dashcamID {
		t.Fatalf("DashcamList = (%v, %v), want [id=%d]", dashcamList, err, dashcamID)
	}
}

func strPtr(s string) *string { return &s }

// TestPullCleanupCandidates_RequiresSettled is final-fix-wave item 1's
// (second-pass) DB-level covering test for the settle-based cleanup
// guard: PullCleanupCandidates must exclude an otherwise perfectly normal
// verified+linked+undeleted pull until settled_at is actually set, and
// PullMarkSettled must be the only thing that sets it — gated on
// remote_start_time <= the given cutoff and last_seen_at matching exactly.
func TestPullCleanupCandidates_RequiresSettled(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	shareSourceID, err := d.SourceCreate(ctx, "share", "/mnt/shares/nas", "NAS")
	if err != nil {
		t.Fatalf("SourceCreate (share): %v", err)
	}

	insertSegment := func(relPath string) {
		t.Helper()
		if _, err := d.Write().ExecContext(ctx,
			`INSERT INTO segments (source_id, camera, rel_path, start_time) VALUES (?, 'front', ?, '2026-01-01T00:00:00Z')`,
			shareSourceID, relPath); err != nil {
			t.Fatalf("insert segment %s: %v", relPath, err)
		}
	}

	const seenAt = "2026-01-05T00:00:00.000000000Z"

	insertSegment("front/old.mp4")
	idOld, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/old.mp4", ShareID: shareID,
		DestRelPath: "front/old.mp4", Bytes: 100, PulledAt: "2026-01-05T00:00:00Z",
		LastSeenAt: strPtr(seenAt), RemoteStartTime: strPtr("2026-01-01T10:00:00"),
	})
	if err != nil {
		t.Fatalf("PullUpsert old: %v", err)
	}
	if err := d.PullMarkVerified(ctx, idOld, "aaa", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified old: %v", err)
	}

	insertSegment("front/newest.mp4")
	idNewest, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/newest.mp4", ShareID: shareID,
		DestRelPath: "front/newest.mp4", Bytes: 100, PulledAt: "2026-01-05T00:00:00Z",
		LastSeenAt: strPtr(seenAt), RemoteStartTime: strPtr("2026-01-01T12:00:00"),
	})
	if err != nil {
		t.Fatalf("PullUpsert newest: %v", err)
	}
	if err := d.PullMarkVerified(ctx, idNewest, "bbb", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified newest: %v", err)
	}

	if n, err := d.PullsLinkSegments(ctx, dashcamID, shareSourceID); err != nil || n != 2 {
		t.Fatalf("PullsLinkSegments = (%d, %v), want (2, nil)", n, err)
	}

	// Before PullMarkSettled runs, neither is a candidate.
	if got, err := d.PullCleanupCandidates(ctx, dashcamID); err != nil || len(got) != 0 {
		t.Fatalf("PullCleanupCandidates before settling = (%v, %v), want (empty, nil)", got, err)
	}

	// Cutoff = newest.mp4's start_time (12:00) minus 1h = 11:00 — old.mp4
	// (10:00) is <= that and settles; newest.mp4 (12:00) is not.
	n, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2026-01-01T11:00:00")
	if err != nil {
		t.Fatalf("PullMarkSettled: %v", err)
	}
	if n != 1 {
		t.Fatalf("PullMarkSettled settled %d rows, want 1 (just old.mp4)", n)
	}

	candidates, err := d.PullCleanupCandidates(ctx, dashcamID)
	if err != nil {
		t.Fatalf("PullCleanupCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != idOld {
		t.Fatalf("PullCleanupCandidates = %+v, want just old.mp4 (id=%d)", candidates, idOld)
	}

	// Re-running PullMarkSettled with the same seenAt is a no-op (already
	// settled rows are excluded by "settled_at IS NULL").
	if n, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2026-01-01T11:00:00"); err != nil || n != 0 {
		t.Fatalf("PullMarkSettled (2nd run) = (%d, %v), want (0, nil)", n, err)
	}

	// A row seen by a *different* run (different last_seen_at stamp) is
	// never touched by a PullMarkSettled call for this run's seenAt, even
	// if its remote_start_time is well under the cutoff and the cutoff
	// would easily satisfy it — PullMarkSettled only settles what the
	// matching run itself observed.
	const otherRunSeenAt = "2026-02-01T00:00:00.000000000Z"
	insertSegment("front/other-run.mp4")
	idOtherRun, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/other-run.mp4", ShareID: shareID,
		DestRelPath: "front/other-run.mp4", Bytes: 100, PulledAt: "2026-01-01T00:00:00Z",
		LastSeenAt: strPtr(otherRunSeenAt), RemoteStartTime: strPtr("2026-01-01T00:00:00"),
	})
	if err != nil {
		t.Fatalf("PullUpsert other-run: %v", err)
	}
	if err := d.PullMarkVerified(ctx, idOtherRun, "ccc", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified other-run: %v", err)
	}
	if _, err := d.PullsLinkSegments(ctx, dashcamID, shareSourceID); err != nil {
		t.Fatalf("PullsLinkSegments: %v", err)
	}
	// Calling PullMarkSettled with the *original* run's seenAt (not
	// otherRunSeenAt) and a cutoff that would easily satisfy
	// other-run.mp4's remote_start_time (2026-01-01) must settle nothing
	// new: idOld/idNewest under `seenAt` are already handled above, and
	// other-run.mp4's last_seen_at doesn't match `seenAt` at all.
	if _, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2026-06-01T00:00:00"); err != nil {
		t.Fatalf("PullMarkSettled: %v", err)
	}
	if got, _, err := d.PullGet(ctx, dashcamID, "clips/other-run.mp4"); err != nil || got.SettledAt != nil {
		t.Fatalf("other-run pull SettledAt = %v, want nil (different last_seen_at stamp)", got.SettledAt)
	}
}

// TestPullMarkSettled_NeverSettlesEmptyRemoteStartTime is a final-fix-wave
// item 1 (2nd re-review) covering test: the recorder can emit start_time
// as ” when it has no .timestamp.json and the segment's directory name
// doesn't conform to its own naming convention (see
// resources/metadata.py:1696/679, resources/ingest.py:232). Without an
// explicit guard, ” sorts lexically <= any non-empty cutoff, so such a
// row would settle — and become cleanup-eligible — the very first time
// it's observed, which is exactly the premature-settle bug PullMarkSettled
// exists to prevent.
func TestPullMarkSettled_NeverSettlesEmptyRemoteStartTime(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	shareSourceID, err := d.SourceCreate(ctx, "share", "/mnt/shares/nas", "NAS")
	if err != nil {
		t.Fatalf("SourceCreate (share): %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time) VALUES (?, 'front', 'front/empty.mp4', '2026-01-01T00:00:00Z')`,
		shareSourceID); err != nil {
		t.Fatalf("insert segment: %v", err)
	}

	const seenAt = "2026-01-05T00:00:00.000000000Z"
	id, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/empty.mp4", ShareID: shareID,
		DestRelPath: "front/empty.mp4", Bytes: 100, PulledAt: "2026-01-05T00:00:00Z",
		LastSeenAt: strPtr(seenAt), RemoteStartTime: strPtr(""),
	})
	if err != nil {
		t.Fatalf("PullUpsert: %v", err)
	}
	if err := d.PullMarkVerified(ctx, id, "aaa", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified: %v", err)
	}
	if _, err := d.PullsLinkSegments(ctx, dashcamID, shareSourceID); err != nil {
		t.Fatalf("PullsLinkSegments: %v", err)
	}

	// A cutoff far in the future — '' would lexically compare <= almost
	// anything, so a broken guard would settle this immediately.
	n, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2099-01-01T00:00:00")
	if err != nil {
		t.Fatalf("PullMarkSettled: %v", err)
	}
	if n != 0 {
		t.Fatalf("PullMarkSettled settled %d rows with empty remote_start_time, want 0", n)
	}

	got, found, err := d.PullGet(ctx, dashcamID, "clips/empty.mp4")
	if err != nil || !found {
		t.Fatalf("PullGet: found=%v err=%v", found, err)
	}
	if got.SettledAt != nil {
		t.Fatal("expected SettledAt to stay nil for a row with empty remote_start_time")
	}

	// A NULL remote_start_time (never even set) must behave the same way.
	idNull, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/null.mp4", ShareID: shareID,
		DestRelPath: "front/null.mp4", Bytes: 100, PulledAt: "2026-01-05T00:00:00Z",
		LastSeenAt: strPtr(seenAt),
	})
	if err != nil {
		t.Fatalf("PullUpsert (null start time): %v", err)
	}
	if err := d.PullMarkVerified(ctx, idNull, "bbb", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified: %v", err)
	}
	if n, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2099-01-01T00:00:00"); err != nil {
		t.Fatalf("PullMarkSettled: %v", err)
	} else if n != 0 {
		t.Fatalf("PullMarkSettled settled %d rows with NULL remote_start_time, want 0", n)
	}

	candidates, err := d.PullCleanupCandidates(ctx, dashcamID)
	if err != nil {
		t.Fatalf("PullCleanupCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("PullCleanupCandidates = %+v, want none (neither row ever settled)", candidates)
	}
}

// TestPullUpsert_ChangedFileResetsSettledAt covers PullUpsert's settled_at
// CASE: a re-upsert with different bytes/sha256 (the remote file actually
// changed) must reset settled_at to NULL alongside verified_at/
// deleted_remote_at, same reasoning as those two — a changed file needs
// re-verification and re-settling from scratch, not to inherit a stale
// "safe to delete" state.
func TestPullUpsert_ChangedFileResetsSettledAt(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	const seenAt = "2026-01-01T00:00:00.000000000Z"
	id, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/a.mp4", ShareID: shareID,
		DestRelPath: "front/a.mp4", Bytes: 100, SHA256: strPtr("aaa"), PulledAt: "2026-01-01T00:00:00Z",
		LastSeenAt: strPtr(seenAt), RemoteStartTime: strPtr("2026-01-01T00:00:00"),
	})
	if err != nil {
		t.Fatalf("PullUpsert: %v", err)
	}
	if err := d.PullMarkVerified(ctx, id, "aaa", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified: %v", err)
	}
	if n, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2026-06-01T00:00:00"); err != nil || n != 1 {
		t.Fatalf("PullMarkSettled = (%d, %v), want (1, nil)", n, err)
	}
	got, _, err := d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil || got.SettledAt == nil {
		t.Fatalf("expected SettledAt set before the change, got %+v (err=%v)", got, err)
	}

	// Re-upsert with a different sha256 (remote file changed).
	if _, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/a.mp4", ShareID: shareID,
		DestRelPath: "front/a.mp4", Bytes: 100, SHA256: strPtr("zzz"), PulledAt: "2026-01-02T00:00:00Z",
		LastSeenAt: strPtr(seenAt), RemoteStartTime: strPtr("2026-01-01T00:00:00"),
	}); err != nil {
		t.Fatalf("PullUpsert (changed): %v", err)
	}
	got, _, err = d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet after change: %v", err)
	}
	if got.SettledAt != nil {
		t.Fatal("PullUpsert with a changed sha256 must reset settled_at to NULL")
	}
}

// TestPullTouchSeen_UpdatesSeenFieldsWithoutDisturbingOtherState covers
// PullTouchSeen (final-fix-wave item 1's skip-path stamp): it must update
// last_seen_at/remote_has_detections/remote_has_gps/remote_start_time
// without touching verified_at, bytes, or sha256.
func TestPullTouchSeen_UpdatesSeenFieldsWithoutDisturbingOtherState(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	id, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/a.mp4", ShareID: shareID,
		DestRelPath: "front/a.mp4", Bytes: 1000, SHA256: strPtr("aaa"), PulledAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("PullUpsert: %v", err)
	}
	if err := d.PullMarkVerified(ctx, id, "aaa", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified: %v", err)
	}

	if err := d.PullTouchSeen(ctx, id, "2026-01-02T00:00:00.000000000Z", true, true, "2026-01-01T10:00:00"); err != nil {
		t.Fatalf("PullTouchSeen: %v", err)
	}

	got, found, err := d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil || !found {
		t.Fatalf("PullGet: found=%v err=%v", found, err)
	}
	if got.LastSeenAt == nil || *got.LastSeenAt != "2026-01-02T00:00:00.000000000Z" {
		t.Fatalf("LastSeenAt = %v, want 2026-01-02T00:00:00.000000000Z", got.LastSeenAt)
	}
	if !got.RemoteHasDetections || !got.RemoteHasGPS {
		t.Fatalf("RemoteHasDetections/RemoteHasGPS = %v/%v, want true/true", got.RemoteHasDetections, got.RemoteHasGPS)
	}
	if got.RemoteStartTime == nil || *got.RemoteStartTime != "2026-01-01T10:00:00" {
		t.Fatalf("RemoteStartTime = %v, want 2026-01-01T10:00:00", got.RemoteStartTime)
	}
	if got.VerifiedAt == nil {
		t.Fatal("PullTouchSeen must not disturb verified_at")
	}
	if got.Bytes != 1000 || got.SHA256 == nil || *got.SHA256 != "aaa" {
		t.Fatalf("PullTouchSeen must not disturb bytes/sha256, got bytes=%d sha256=%v", got.Bytes, got.SHA256)
	}

	if err := d.PullTouchSeen(ctx, 999999, "2026-01-02T00:00:00.000000000Z", false, false, "2026-01-01T00:00:00"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PullTouchSeen on unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPullMarkVerifiedEmptySHA256PreservesExisting covers fix round 1 item
// 4: PullMarkVerified used to overwrite sha256 unconditionally, so a
// caller confirming just size/bytes (passing "" because it never computed
// a hash) would clobber a hash PullUpsert had already recorded.
func TestPullMarkVerifiedEmptySHA256PreservesExisting(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamID, err := d.DashcamCreate(ctx, "Front Cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
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
		t.Fatalf("PullUpsert: %v", err)
	}

	// Verify without passing a hash.
	if err := d.PullMarkVerified(ctx, id, "", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified(\"\"): %v", err)
	}
	got, _, err := d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet: %v", err)
	}
	if got.VerifiedAt == nil {
		t.Fatal("expected VerifiedAt to be set")
	}
	if got.SHA256 == nil || *got.SHA256 != "aaa" {
		t.Fatalf("SHA256 after PullMarkVerified(\"\") = %v, want the existing aaa preserved", got.SHA256)
	}

	// Passing a real hash still updates it.
	if err := d.PullMarkVerified(ctx, id, "zzz", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified(\"zzz\"): %v", err)
	}
	got, _, err = d.PullGet(ctx, dashcamID, "clips/a.mp4")
	if err != nil {
		t.Fatalf("PullGet: %v", err)
	}
	if got.SHA256 == nil || *got.SHA256 != "zzz" {
		t.Fatalf("SHA256 after PullMarkVerified(\"zzz\") = %v, want zzz", got.SHA256)
	}
}

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
	// PullCleanupCandidates (final-fix-wave item 1, second pass) requires
	// settled_at IS NOT NULL — give every row a last_seen_at/
	// remote_start_time and settle it via PullMarkSettled below, rather
	// than the earlier (buggy) last_pull_at/mtime-based guard.
	const seenAt = "2026-01-05T00:00:00.000000000Z"
	startTime := strPtr("2026-01-01T00:00:00")

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

	// Pull A: verified + linked to a segment -> cleanup candidate (once
	// settled).
	idA, err := d.PullUpsert(ctx, DashcamPull{
		DashcamSourceID: dashcamID, RemoteRelPath: "clips/a.mp4", ShareID: shareID,
		DestRelPath: "front/a.mp4", Bytes: 100, PulledAt: "2026-01-01T00:00:00Z",
		LastSeenAt: strPtr(seenAt), RemoteStartTime: startTime,
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
		LastSeenAt: strPtr(seenAt), RemoteStartTime: startTime,
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
		LastSeenAt: strPtr(seenAt), RemoteStartTime: startTime,
	}); err != nil {
		t.Fatalf("PullUpsert C: %v", err)
	}

	// Settle every row this "run" (seenAt) touched, well past their
	// remote_start_time.
	if _, err := d.PullMarkSettled(ctx, dashcamID, seenAt, "2026-06-01T00:00:00"); err != nil {
		t.Fatalf("PullMarkSettled: %v", err)
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
	if got.SettledAt == nil {
		t.Fatal("expected pull A to already be settled")
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

// TestPullCounts exercises the verified/pending_cleanup counts the status
// handler surfaces: pull A is verified+linked (still pending cleanup),
// pull B is verified+linked+already-deleted-remote (counts toward
// verified but not pending_cleanup), and pull C is unverified (counts
// toward neither).
func TestPullCounts(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	shareSourceID, err := d.SourceCreate(ctx, "share", "/mnt/shares/nas", "NAS")
	if err != nil {
		t.Fatalf("SourceCreate (share): %v", err)
	}
	dashcamID, err := d.DashcamCreate(ctx, "cam", "http://10.0.0.9", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	if v, p, err := d.PullCounts(ctx, dashcamID); err != nil || v != 0 || p != 0 {
		t.Fatalf("PullCounts on empty = (%d, %d, %v), want (0, 0, nil)", v, p, err)
	}

	idA, err := d.PullUpsert(ctx, DashcamPull{DashcamSourceID: dashcamID, RemoteRelPath: "clips/a.mp4", ShareID: shareID, DestRelPath: "clips/a.mp4", Bytes: 100, PulledAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatalf("PullUpsert A: %v", err)
	}
	idB, err := d.PullUpsert(ctx, DashcamPull{DashcamSourceID: dashcamID, RemoteRelPath: "clips/b.mp4", ShareID: shareID, DestRelPath: "clips/b.mp4", Bytes: 100, PulledAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatalf("PullUpsert B: %v", err)
	}
	if _, err := d.PullUpsert(ctx, DashcamPull{DashcamSourceID: dashcamID, RemoteRelPath: "clips/c.mp4", ShareID: shareID, DestRelPath: "clips/c.mp4", Bytes: 100, PulledAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("PullUpsert C: %v", err)
	}

	if err := d.PullMarkVerified(ctx, idA, "shaA", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified A: %v", err)
	}
	if err := d.PullMarkVerified(ctx, idB, "shaB", time.Now()); err != nil {
		t.Fatalf("PullMarkVerified B: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time) VALUES (?, 'front', 'clips/a.mp4', '2026-01-01T00:00:00Z')`,
		shareSourceID); err != nil {
		t.Fatalf("insert segment a: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, camera, rel_path, start_time) VALUES (?, 'front', 'clips/b.mp4', '2026-01-01T00:00:00Z')`,
		shareSourceID); err != nil {
		t.Fatalf("insert segment b: %v", err)
	}
	if _, err := d.PullsLinkSegments(ctx, dashcamID, shareSourceID); err != nil {
		t.Fatalf("PullsLinkSegments: %v", err)
	}
	if err := d.PullMarkDeletedRemote(ctx, []int64{idB}, time.Now()); err != nil {
		t.Fatalf("PullMarkDeletedRemote: %v", err)
	}

	verified, pendingCleanup, err := d.PullCounts(ctx, dashcamID)
	if err != nil {
		t.Fatalf("PullCounts: %v", err)
	}
	if verified != 2 {
		t.Fatalf("PullCounts verified = %d, want 2", verified)
	}
	if pendingCleanup != 1 {
		t.Fatalf("PullCounts pendingCleanup = %d, want 1", pendingCleanup)
	}
}
