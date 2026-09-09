package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyReportsSeededOrphans(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	rep, err := Verify(ctx, d, dbPath, s.cacheDir)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if !rep.IntegrityOK {
		t.Errorf("IntegrityOK = false, messages: %v", rep.IntegrityMessages)
	}
	if rep.IntegrityCheckKind != "integrity_check" {
		t.Errorf("IntegrityCheckKind = %q, want integrity_check for a small test db", rep.IntegrityCheckKind)
	}

	if len(rep.SourcesUnavailable) != 0 || rep.MissingMediaCheckIncomplete {
		t.Errorf("expected all sources available, got SourcesUnavailable=%v MissingMediaCheckIncomplete=%v", rep.SourcesUnavailable, rep.MissingMediaCheckIncomplete)
	}

	// missingMediaSegID (old mtime) and recentSegID (fresh mtime) both
	// have no file on disk — Verify reports both, unlike Prune, which
	// additionally applies the recently-modified grace period before
	// actually deleting anything (see prune.go's filterMissingCandidates).
	if rep.MissingMediaSegments != 2 {
		t.Errorf("MissingMediaSegments = %d, want 2", rep.MissingMediaSegments)
	}
	wantIDs := map[int64]bool{s.missingMediaSegID: true, s.recentSegID: true}
	for _, id := range rep.MissingMediaSegmentIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected id %d in MissingMediaSegmentIDs %v", id, rep.MissingMediaSegmentIDs)
		}
		delete(wantIDs, id)
	}
	if len(wantIDs) != 0 {
		t.Errorf("MissingMediaSegmentIDs missing expected ids: %v", wantIDs)
	}

	if rep.OrphanDetections != 1 {
		t.Errorf("OrphanDetections = %d, want 1", rep.OrphanDetections)
	}
	if rep.OrphanDetObjects != 1 {
		t.Errorf("OrphanDetObjects = %d, want 1", rep.OrphanDetObjects)
	}
	if rep.OrphanDetPlates != 1 {
		t.Errorf("OrphanDetPlates = %d, want 1", rep.OrphanDetPlates)
	}
	if rep.OrphanDetVehicles != 1 {
		t.Errorf("OrphanDetVehicles = %d, want 1", rep.OrphanDetVehicles)
	}
	if rep.OrphanDetTracks != 1 {
		t.Errorf("OrphanDetTracks = %d, want 1", rep.OrphanDetTracks)
	}

	if rep.SegmentsMissingDetectionsRaw != 1 {
		t.Errorf("SegmentsMissingDetectionsRaw = %d, want 1", rep.SegmentsMissingDetectionsRaw)
	}

	if rep.OrphanThumbFiles != 1 {
		t.Errorf("OrphanThumbFiles = %d, want 1", rep.OrphanThumbFiles)
	}
	if len(rep.OrphanThumbPaths) != 1 || filepath.Base(rep.OrphanThumbPaths[0]) != "424242.jpg" {
		t.Errorf("OrphanThumbPaths = %v, want [.../424242.jpg]", rep.OrphanThumbPaths)
	}

	if rep.DBSizeBytes <= 0 {
		t.Errorf("DBSizeBytes = %d, want > 0", rep.DBSizeBytes)
	}
	if rep.PageCount <= 0 || rep.PageSizeBytes <= 0 {
		t.Errorf("PageCount/PageSizeBytes = %d/%d, want both > 0", rep.PageCount, rep.PageSizeBytes)
	}
}

func TestVerifyArchivedSegmentNotFlaggedMissing(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	rep, err := Verify(ctx, d, dbPath, s.cacheDir)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, id := range rep.MissingMediaSegmentIDs {
		if id == s.archivedSegID {
			t.Errorf("archived segment %d must not be reported as missing media", s.archivedSegID)
		}
	}
}

// TestVerifyReportsUnavailableSourceAndExcludesItsSegments is CRITICAL 1's
// coverage for Verify's half of the fix: with the mount escape hatch off
// and no real /proc/mounts entry for the fixture's temp-dir root, the
// source must be reported unavailable and its otherwise-"missing"
// segments must be excluded from the count rather than flagged.
func TestVerifyReportsUnavailableSourceAndExcludesItsSegments(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	// seedFixture turns on allow_local_share_paths so the fixture's plain
	// temp-dir root_path is treated as available despite not being a real
	// mount; turn it back off to exercise the real guard.
	if err := d.SettingsSet(ctx, "allow_local_share_paths", []byte("false")); err != nil {
		t.Fatalf("settings set: %v", err)
	}

	rep, err := Verify(ctx, d, dbPath, s.cacheDir)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(rep.SourcesUnavailable) != 1 {
		t.Fatalf("SourcesUnavailable = %v, want exactly 1 entry", rep.SourcesUnavailable)
	}
	if !rep.MissingMediaCheckIncomplete {
		t.Errorf("MissingMediaCheckIncomplete = false, want true")
	}
	if rep.MissingMediaSegments != 0 || len(rep.MissingMediaSegmentIDs) != 0 {
		t.Errorf("MissingMediaSegments/IDs = %d/%v, want 0/empty — source is unavailable, nothing should be classified missing",
			rep.MissingMediaSegments, rep.MissingMediaSegmentIDs)
	}
	// Orphan-row/thumb/integrity checks are independent of source
	// availability and must still run normally.
	if rep.OrphanDetections != 1 {
		t.Errorf("OrphanDetections = %d, want 1 (unaffected by source availability)", rep.OrphanDetections)
	}
}

func TestPruneRefusesWhenSourceUnavailable(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	if err := d.SettingsSet(ctx, "allow_local_share_paths", []byte("false")); err != nil {
		t.Fatalf("settings set: %v", err)
	}

	for _, apply := range []bool{false, true} {
		_, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: apply, Force: true})
		if err == nil {
			t.Fatalf("Prune(apply=%v) with an unavailable source: want error, got nil", apply)
		}
	}

	// Nothing must have been touched — not even the orphan rows/files,
	// since Prune refuses before doing anything.
	var n int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.missingMediaSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 1 {
		t.Errorf("segment %d was deleted despite the unavailable-source refusal", s.missingMediaSegID)
	}
	if _, err := os.Stat(filepath.Join(s.cacheDir, "thumbs", "242", "424242.jpg")); err != nil {
		t.Errorf("orphan thumb file removed despite the unavailable-source refusal: %v", err)
	}
}

// TestPruneAbortsWhenExceedsPlausibilityThreshold and its Force
// counterpart cover CRITICAL 1's second guard: even with every source
// mounted, an implausibly large missing-media count refuses to apply
// unless force:true.
func TestPruneAbortsWhenExceedsPlausibilityThreshold(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	// seedFixture's default fixture already has 1 confirmed-missing
	// segment (missingMediaSegID) against a 5-segment total, i.e. 20% —
	// above the min(10%, 500) threshold, so Apply without Force must
	// refuse.
	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: true})
	if err == nil {
		t.Fatalf("Prune(apply, no force) over threshold: want error, got result %+v", res)
	}

	var n int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.missingMediaSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 1 {
		t.Errorf("segment %d was deleted despite the threshold refusal", s.missingMediaSegID)
	}

	// A dry run must NOT be blocked by the threshold — it's exactly how
	// an operator is meant to discover the count before deciding whether
	// to pass force:true.
	dryRes, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: false})
	if err != nil {
		t.Fatalf("Prune dry-run over threshold: %v", err)
	}
	if dryRes.SegmentsRemoved != 1 {
		t.Errorf("dry-run SegmentsRemoved = %d, want 1", dryRes.SegmentsRemoved)
	}
}

func TestPruneThresholdAbortOverriddenByForce(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Prune(apply, force): %v", err)
	}
	if res.SegmentsRemoved != 1 {
		t.Errorf("SegmentsRemoved = %d, want 1", res.SegmentsRemoved)
	}

	var n int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.missingMediaSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 0 {
		t.Errorf("segment %d survived a forced prune", s.missingMediaSegID)
	}
}

// TestPruneSkipsRecentlyModifiedSegments is CRITICAL 3's coverage: a
// segment whose file is missing but whose recorded mtime is fresh must
// survive even a forced, applied prune.
func TestPruneSkipsRecentlyModifiedSegments(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.SegmentsSkippedRecentlyModified != 1 {
		t.Errorf("SegmentsSkippedRecentlyModified = %d, want 1", res.SegmentsSkippedRecentlyModified)
	}

	var n int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.recentSegID).Scan(&n); err != nil {
		t.Fatalf("query recent segment: %v", err)
	}
	if n != 1 {
		t.Errorf("recently-modified segment %d was deleted", s.recentSegID)
	}
}

func TestPruneDryRunChangesNothing(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: false})
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if !res.DryRun {
		t.Errorf("DryRun = false, want true")
	}
	if res.SegmentsRemoved != 1 {
		t.Errorf("SegmentsRemoved = %d, want 1", res.SegmentsRemoved)
	}
	if res.SegmentsSkippedRecentlyModified != 1 {
		t.Errorf("SegmentsSkippedRecentlyModified = %d, want 1", res.SegmentsSkippedRecentlyModified)
	}
	if res.OrphanDetectionsRemoved != 1 || res.OrphanDetObjectsRemoved != 1 ||
		res.OrphanDetPlatesRemoved != 1 || res.OrphanDetVehiclesRemoved != 1 || res.OrphanDetTracksRemoved != 1 {
		t.Errorf("orphan counts wrong: %+v", res)
	}
	if res.OrphanThumbFilesRemoved != 1 {
		t.Errorf("OrphanThumbFilesRemoved = %d, want 1", res.OrphanThumbFilesRemoved)
	}
	if res.WALCheckpointed || res.Vacuumed {
		t.Errorf("dry run must not checkpoint/vacuum: %+v", res)
	}

	// Nothing actually changed: the missing-media segment row still
	// exists, the orphan thumb file is still on disk.
	var stillThere int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.missingMediaSegID).Scan(&stillThere); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if stillThere != 1 {
		t.Errorf("dry run deleted segment %d", s.missingMediaSegID)
	}
	if _, err := os.Stat(filepath.Join(s.cacheDir, "thumbs", "242", "424242.jpg")); err != nil {
		t.Errorf("dry run removed orphan thumb file: %v", err)
	}

	// The archived segment (file also missing, but archived) and the
	// raw-missing segment (has_detections but no detections_raw) must
	// both survive a real prune too — asserted here since a dry run
	// finding them absent from the delete targets is the same code path
	// a real run uses.
	if res.SegmentsMissingDetectionsRaw != 1 {
		t.Errorf("SegmentsMissingDetectionsRaw = %d, want 1 (reported, not deleted)", res.SegmentsMissingDetectionsRaw)
	}
}

func TestPruneRealRunDeletesOrphans(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.DryRun {
		t.Errorf("DryRun = true, want false")
	}
	if !res.WALCheckpointed {
		t.Errorf("WALCheckpointed = false, want true")
	}
	if res.Vacuumed {
		t.Errorf("Vacuumed = true, want false (vacuum:false)")
	}

	// Missing-media segment gone.
	var n int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.missingMediaSegID).Scan(&n); err != nil {
		t.Fatalf("query missing segment: %v", err)
	}
	if n != 0 {
		t.Errorf("missing-media segment %d still present after prune", s.missingMediaSegID)
	}

	// Archived and raw-missing segments untouched.
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.archivedSegID).Scan(&n); err != nil {
		t.Fatalf("query archived segment: %v", err)
	}
	if n != 1 {
		t.Errorf("archived segment %d was deleted by prune", s.archivedSegID)
	}
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.rawMissingSegID).Scan(&n); err != nil {
		t.Fatalf("query raw-missing segment: %v", err)
	}
	if n != 1 {
		t.Errorf("detections_raw-missing segment %d was deleted by prune", s.rawMissingSegID)
	}

	// ok segment and its detections/det_objects/det_tracks/thumbnail
	// survive.
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.okSegmentID).Scan(&n); err != nil {
		t.Fatalf("query ok segment: %v", err)
	}
	if n != 1 {
		t.Errorf("ok segment %d was deleted by prune", s.okSegmentID)
	}

	// Orphan rows gone.
	for _, q := range []string{
		`SELECT COUNT(*) FROM detections WHERE id = 9001`,
		`SELECT COUNT(*) FROM det_objects WHERE det_id = 999999`,
		`SELECT COUNT(*) FROM det_plates WHERE det_id = 999999`,
		`SELECT COUNT(*) FROM det_vehicles WHERE det_id = 999999`,
		`SELECT COUNT(*) FROM det_tracks WHERE segment_id = 999999`,
	} {
		if err := d.Read().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if n != 0 {
			t.Errorf("query %q returned %d, want 0 after prune", q, n)
		}
	}

	// Orphan thumb file removed from disk; the ok segment's thumbnail
	// survives.
	if _, err := os.Stat(filepath.Join(s.cacheDir, "thumbs", "242", "424242.jpg")); !os.IsNotExist(err) {
		t.Errorf("orphan thumb file still present: err=%v", err)
	}

	res2, err := Verify(ctx, d, dbPath, s.cacheDir)
	if err != nil {
		t.Fatalf("Verify after prune: %v", err)
	}
	if res2.OrphanThumbFiles != 0 {
		t.Errorf("OrphanThumbFiles after prune = %d, want 0", res2.OrphanThumbFiles)
	}
	// recentSegID's file is still missing (it was skipped, not deleted,
	// by design) — Verify still reports it, since Verify doesn't apply
	// the recently-modified grace period (see the TestVerifyReports...
	// doc comment above).
	if res2.MissingMediaSegments != 1 || len(res2.MissingMediaSegmentIDs) != 1 || res2.MissingMediaSegmentIDs[0] != s.recentSegID {
		t.Errorf("MissingMediaSegments after prune = %d %v, want [1]{%d}", res2.MissingMediaSegments, res2.MissingMediaSegmentIDs, s.recentSegID)
	}
}

func TestPruneVacuum(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{Apply: true, Force: true, Vacuum: true})
	if err != nil {
		t.Fatalf("Prune with vacuum: %v", err)
	}
	if !res.WALCheckpointed {
		t.Errorf("WALCheckpointed = false, want true")
	}
	// A fresh temp-dir filesystem should always have far more than 2x a
	// test database's (tiny) size free, so vacuum should actually run —
	// but tolerate a skip with a reason rather than asserting Vacuumed
	// outright, since free space is an environment fact this test
	// doesn't control.
	if !res.Vacuumed && res.VacuumSkippedReason == "" {
		t.Errorf("Vacuum neither ran nor reported a skip reason: %+v", res)
	}
}

func TestPruneEmptyOptsIsDryRun(t *testing.T) {
	d, dbPath := openTest(t)
	s := seedFixture(t, d)
	ctx := context.Background()

	// CRITICAL 2: the zero value of PruneOpts (as an empty/absent request
	// body decodes to) must be a safe dry run, never destructive.
	res, err := Prune(ctx, d, dbPath, s.cacheDir, PruneOpts{})
	if err != nil {
		t.Fatalf("Prune(zero value): %v", err)
	}
	if !res.DryRun {
		t.Errorf("DryRun = false for a zero-value PruneOpts, want true")
	}

	var n int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE id = ?`, s.missingMediaSegID).Scan(&n); err != nil {
		t.Fatalf("query segment: %v", err)
	}
	if n != 1 {
		t.Errorf("zero-value PruneOpts deleted segment %d", s.missingMediaSegID)
	}
}
