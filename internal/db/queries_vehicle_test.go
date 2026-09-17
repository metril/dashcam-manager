package db

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

// -- fixture helpers --------------------------------------------------

// mustInsertShareSource creates a share row plus its kind='share' source
// and returns both ids.
func mustInsertShareSource(t *testing.T, d *DB, ctx context.Context) (shareID, shareSrcID int64) {
	t.Helper()
	shareID = mustInsertShare(t, d, ctx)
	shareSrcID, err := d.SourceCreateForShare(ctx, shareID, "/mnt/nas", "nas")
	if err != nil {
		t.Fatalf("SourceCreateForShare: %v", err)
	}
	return shareID, shareSrcID
}

func mustInsertVehicleTrip(t *testing.T, d *DB, ctx context.Context, shareSrcID int64, tripKey, startedAt, endedAt string) int64 {
	t.Helper()
	var ea any
	if endedAt != "" {
		ea = endedAt
	}
	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO trips (source_id, trip_key, started_at, ended_at) VALUES (?, ?, ?, ?)`,
		shareSrcID, tripKey, startedAt, ea)
	if err != nil {
		t.Fatalf("insert trip: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func mustInsertVehicleSegment(t *testing.T, d *DB, ctx context.Context, shareSrcID int64, tripID *int64, camera, relPath, startTime, endTime string) int64 {
	t.Helper()
	var et any
	if endTime != "" {
		et = endTime
	}
	res, err := d.Write().ExecContext(ctx,
		`INSERT INTO segments (source_id, trip_id, camera, rel_path, start_time, end_time) VALUES (?, ?, ?, ?, ?, ?)`,
		shareSrcID, tripID, camera, relPath, startTime, et)
	if err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func mustInsertSegmentGPS(t *testing.T, d *DB, ctx context.Context, segID int64, ts string, lat, lon float64) {
	t.Helper()
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segment_gps (segment_id, t, lat, lon) VALUES (?, ?, ?, ?)`, segID, ts, lat, lon); err != nil {
		t.Fatalf("insert segment_gps: %v", err)
	}
}

func mustInsertSegmentOBD(t *testing.T, d *DB, ctx context.Context, segID int64, distanceM, fuelL, maxSpeed float64) {
	t.Helper()
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO segment_obd (segment_id, distance_m, fuel_used_l, max_speed_kph) VALUES (?, ?, ?, ?)`,
		segID, distanceM, fuelL, maxSpeed); err != nil {
		t.Fatalf("insert segment_obd: %v", err)
	}
}

func vehicleTripIDColumn(t *testing.T, d *DB, ctx context.Context, eventID int64) any {
	t.Helper()
	var v any
	if err := d.Read().QueryRowContext(ctx, `SELECT vehicle_trip_id FROM vehicle_events WHERE id = ?`, eventID).Scan(&v); err != nil {
		t.Fatalf("query vehicle_trip_id: %v", err)
	}
	return v
}

// -- VehicleTripsUpsert -------------------------------------------------

func TestVehicleTripsUpsertRoundTripAndUpdate(t *testing.T) {
	t.Setenv("TZ", "UTC")
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	id1, err := d.VehicleTripsUpsert(ctx, dashcamSrcID, VehicleTrip{
		RemoteTripID: "trip-1", StartTime: "2026-01-01T10:00:00", EndTime: "2026-01-01T10:30:00",
		DTCs: []string{"P0171"}, MIL: 1, SampleCount: 5,
	})
	if err != nil {
		t.Fatalf("VehicleTripsUpsert: %v", err)
	}

	var startTime, endTime, dtcs string
	var mil, sampleCount int
	if err := d.Read().QueryRowContext(ctx,
		`SELECT start_time, end_time, dtcs, mil, sample_count FROM vehicle_trips WHERE id = ?`, id1,
	).Scan(&startTime, &endTime, &dtcs, &mil, &sampleCount); err != nil {
		t.Fatalf("query vehicle_trips: %v", err)
	}
	if startTime != "2026-01-01T10:00:00Z" || endTime != "2026-01-01T10:30:00Z" {
		t.Errorf("start/end = %q/%q, want RFC3339 UTC", startTime, endTime)
	}
	if mil != 1 || sampleCount != 5 {
		t.Errorf("mil/sample_count = %d/%d, want 1/5", mil, sampleCount)
	}
	var got []string
	if err := json.Unmarshal([]byte(dtcs), &got); err != nil || len(got) != 1 || got[0] != "P0171" {
		t.Errorf("dtcs = %q, want [\"P0171\"]", dtcs)
	}

	// Re-upsert same remote_trip_id with changed fields: same row (id),
	// fields updated — idempotent on the unique key, not a duplicate.
	id2, err := d.VehicleTripsUpsert(ctx, dashcamSrcID, VehicleTrip{
		RemoteTripID: "trip-1", StartTime: "2026-01-01T10:00:00", EndTime: "2026-01-01T10:45:00",
		MIL: 0, SampleCount: 9,
	})
	if err != nil {
		t.Fatalf("VehicleTripsUpsert (update): %v", err)
	}
	if id2 != id1 {
		t.Fatalf("re-upsert id = %d, want %d (same row)", id2, id1)
	}
	var cnt int
	if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM vehicle_trips`).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("vehicle_trips count = %d, want 1 (upsert, not duplicate)", cnt)
	}
	if err := d.Read().QueryRowContext(ctx,
		`SELECT end_time, mil, sample_count FROM vehicle_trips WHERE id = ?`, id1,
	).Scan(&endTime, &mil, &sampleCount); err != nil {
		t.Fatalf("query after update: %v", err)
	}
	if endTime != "2026-01-01T10:45:00Z" || mil != 0 || sampleCount != 9 {
		t.Errorf("after update end_time/mil/sample_count = %q/%d/%d, want 2026-01-01T10:45:00Z/0/9", endTime, mil, sampleCount)
	}
}

// -- VehicleEventsUpsert: vehicle_trip_id backfill ------------------------

func TestVehicleEventsUpsertBackfillsVehicleTripID(t *testing.T) {
	t.Setenv("TZ", "UTC")
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	evID, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "ev-1", EventType: "hard_brake", WallTime: "2026-01-01T10:05:00",
		RemoteTripID: "trip-1",
	})
	if err != nil {
		t.Fatalf("VehicleEventsUpsert: %v", err)
	}
	if got := vehicleTripIDColumn(t, d, ctx, evID); got != nil {
		t.Fatalf("vehicle_trip_id before trip exists = %v, want nil", got)
	}

	vtID, err := d.VehicleTripsUpsert(ctx, dashcamSrcID, VehicleTrip{RemoteTripID: "trip-1"})
	if err != nil {
		t.Fatalf("VehicleTripsUpsert: %v", err)
	}

	// Re-upsert the same event (tail re-fetch) — must now resolve
	// vehicle_trip_id.
	evID2, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "ev-1", EventType: "hard_brake", WallTime: "2026-01-01T10:05:00",
		RemoteTripID: "trip-1",
	})
	if err != nil {
		t.Fatalf("VehicleEventsUpsert (re-fetch): %v", err)
	}
	if evID2 != evID {
		t.Fatalf("re-upsert id = %d, want %d", evID2, evID)
	}
	got := vehicleTripIDColumn(t, d, ctx, evID)
	gotID, ok := got.(int64)
	if !ok || gotID != vtID {
		t.Fatalf("vehicle_trip_id after trip exists = %v, want %d", got, vtID)
	}
}

// -- VehicleEventsLink ---------------------------------------------------

func TestVehicleEventsLinkSegmentBoundaryAndNearestGPS(t *testing.T) {
	t.Setenv("TZ", "UTC")
	d := openTest(t)
	ctx := context.Background()
	shareID, shareSrcID := mustInsertShareSource(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	tripID := mustInsertVehicleTrip(t, d, ctx, shareSrcID, "trip", "2026-01-01T10:00:00Z", "2026-01-01T10:01:00Z")
	seg1 := mustInsertVehicleSegment(t, d, ctx, shareSrcID, &tripID, "front", "front/seg1.mp4", "2026-01-01T10:00:00Z", "2026-01-01T10:00:30Z")
	seg2 := mustInsertVehicleSegment(t, d, ctx, shareSrcID, &tripID, "front", "front/seg2.mp4", "2026-01-01T10:00:30Z", "2026-01-01T10:01:00Z")
	mustInsertSegmentGPS(t, d, ctx, seg1, "2026-01-01T10:00:00Z", 1.0, 2.0)
	mustInsertSegmentGPS(t, d, ctx, seg1, "2026-01-01T10:00:29Z", 1.5, 2.5)

	// Inside [seg1.start, seg1.end): must match seg1, offset 10s, nearest
	// gps point is the one 10s away (t=10:00:00) not the one 19s away.
	evInSeg1, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "ev-1", EventType: "hard_brake", WallTime: "2026-01-01T10:00:10",
	})
	if err != nil {
		t.Fatalf("VehicleEventsUpsert ev-1: %v", err)
	}
	// Exactly at seg1's end_time (exclusive) / seg2's start_time
	// (inclusive): must match seg2, not seg1 — proves [start,end).
	evAtBoundary, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "ev-2", EventType: "hard_accel", WallTime: "2026-01-01T10:00:30",
	})
	if err != nil {
		t.Fatalf("VehicleEventsUpsert ev-2: %v", err)
	}

	n, err := d.VehicleEventsLink(ctx, dashcamSrcID, shareSrcID)
	if err != nil {
		t.Fatalf("VehicleEventsLink: %v", err)
	}
	if n != 2 {
		t.Fatalf("VehicleEventsLink linked = %d, want 2", n)
	}

	var segID, evTripID int64
	var offset, lat, lon float64
	if err := d.Read().QueryRowContext(ctx,
		`SELECT segment_id, trip_id, offset_s, lat, lon FROM vehicle_events WHERE id = ?`, evInSeg1,
	).Scan(&segID, &evTripID, &offset, &lat, &lon); err != nil {
		t.Fatalf("query ev-1: %v", err)
	}
	if segID != seg1 || evTripID != tripID || offset != 10 {
		t.Errorf("ev-1 segment/trip/offset = %d/%d/%v, want %d/%d/10", segID, evTripID, offset, seg1, tripID)
	}
	if lat != 1.0 || lon != 2.0 {
		t.Errorf("ev-1 lat/lon = %v/%v, want nearest point 1.0/2.0 (10s away, not 1.5/2.5 at 19s)", lat, lon)
	}

	if err := d.Read().QueryRowContext(ctx,
		`SELECT segment_id, offset_s FROM vehicle_events WHERE id = ?`, evAtBoundary,
	).Scan(&segID, &offset); err != nil {
		t.Fatalf("query ev-2: %v", err)
	}
	if segID != seg2 || offset != 0 {
		t.Errorf("ev-2 (at boundary) segment/offset = %d/%v, want %d/0", segID, offset, seg2)
	}

	// Idempotent: a second run must not re-touch already-linked rows (0
	// newly linked) or change their values.
	n2, err := d.VehicleEventsLink(ctx, dashcamSrcID, shareSrcID)
	if err != nil {
		t.Fatalf("VehicleEventsLink (2nd): %v", err)
	}
	if n2 != 0 {
		t.Fatalf("VehicleEventsLink (2nd) linked = %d, want 0", n2)
	}
}

func TestVehicleEventsLinkFallbackWindowClamped(t *testing.T) {
	t.Setenv("TZ", "UTC")
	d := openTest(t)
	ctx := context.Background()
	shareID, shareSrcID := mustInsertShareSource(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	tripID := mustInsertVehicleTrip(t, d, ctx, shareSrcID, "trip", "2026-01-01T10:00:00Z", "2026-01-01T10:00:30Z")
	// No segments at all: every event here must fall back to the trip
	// window (or stay unmatched).

	evBefore, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "before", EventType: "stall", WallTime: "2026-01-01T09:59:00", // 60s before start
	})
	if err != nil {
		t.Fatalf("upsert before: %v", err)
	}
	evAfter, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "after", EventType: "stall", WallTime: "2026-01-01T10:02:00", // 90s after end
	})
	if err != nil {
		t.Fatalf("upsert after: %v", err)
	}
	evTooFar, err := d.VehicleEventsUpsert(ctx, dashcamSrcID, VehicleEvent{
		RemoteEventID: "too-far", EventType: "stall", WallTime: "2026-01-01T10:05:00", // 300s after end
	})
	if err != nil {
		t.Fatalf("upsert too-far: %v", err)
	}

	n, err := d.VehicleEventsLink(ctx, dashcamSrcID, shareSrcID)
	if err != nil {
		t.Fatalf("VehicleEventsLink: %v", err)
	}
	if n != 2 {
		t.Fatalf("linked = %d, want 2 (before, after; too-far stays unmatched)", n)
	}

	assertTripOffset := func(evID, wantTripID int64, wantOffset float64) {
		t.Helper()
		var gotTripID any
		var gotOffset any
		if err := d.Read().QueryRowContext(ctx,
			`SELECT trip_id, offset_s FROM vehicle_events WHERE id = ?`, evID,
		).Scan(&gotTripID, &gotOffset); err != nil {
			t.Fatalf("query event %d: %v", evID, err)
		}
		if gotTripID != wantTripID {
			t.Errorf("event %d trip_id = %v, want %d", evID, gotTripID, wantTripID)
		}
		if gotOffset != wantOffset {
			t.Errorf("event %d offset_s = %v, want %v", evID, gotOffset, wantOffset)
		}
	}
	assertTripOffset(evBefore, tripID, 0)   // clamped to span start
	assertTripOffset(evAfter, tripID, 30)   // clamped to span end (30s span)

	var tooFarTripID any
	if err := d.Read().QueryRowContext(ctx, `SELECT trip_id FROM vehicle_events WHERE id = ?`, evTooFar).Scan(&tooFarTripID); err != nil {
		t.Fatalf("query too-far: %v", err)
	}
	if tooFarTripID != nil {
		t.Errorf("too-far event trip_id = %v, want nil (outside +-120s window)", tooFarTripID)
	}
}

// -- VehicleTripSummary ---------------------------------------------------

func TestVehicleTripSummarySumsEventsAndOverlap(t *testing.T) {
	t.Setenv("TZ", "UTC")
	d := openTest(t)
	ctx := context.Background()
	shareID, shareSrcID := mustInsertShareSource(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	tripID := mustInsertVehicleTrip(t, d, ctx, shareSrcID, "trip", "2026-01-01T10:00:00Z", "2026-01-01T10:10:00Z")
	seg1 := mustInsertVehicleSegment(t, d, ctx, shareSrcID, &tripID, "front", "front/seg1.mp4", "2026-01-01T10:00:00Z", "2026-01-01T10:05:00Z")
	seg2 := mustInsertVehicleSegment(t, d, ctx, shareSrcID, &tripID, "front", "front/seg2.mp4", "2026-01-01T10:05:00Z", "2026-01-01T10:10:00Z")
	mustInsertSegmentOBD(t, d, ctx, seg1, 1000, 0.1, 80)
	mustInsertSegmentOBD(t, d, ctx, seg2, 1500, 0.2, 95)
	// A rear segment sharing the trip must NOT be summed (front-only).
	segRear := mustInsertVehicleSegment(t, d, ctx, shareSrcID, &tripID, "rear", "rear/seg1.mp4", "2026-01-01T10:00:00Z", "2026-01-01T10:05:00Z")
	mustInsertSegmentOBD(t, d, ctx, segRear, 99999, 99, 999)

	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO vehicle_events (dashcam_source_id, remote_event_id, event_type, trip_id) VALUES (?, ?, ?, ?)`,
		dashcamSrcID, "ev-1", "hard_brake", tripID); err != nil {
		t.Fatalf("insert event 1: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO vehicle_events (dashcam_source_id, remote_event_id, event_type, trip_id) VALUES (?, ?, ?, ?)`,
		dashcamSrcID, "ev-2", "hard_brake", tripID); err != nil {
		t.Fatalf("insert event 2: %v", err)
	}

	battMin, battMax := 11.5, 14.2
	overlapping, err := d.VehicleTripsUpsert(ctx, dashcamSrcID, VehicleTrip{
		RemoteTripID: "vt-overlap", StartTime: "2026-01-01T10:02:00", EndTime: "2026-01-01T10:08:00",
		DTCs: []string{"P0300"}, MIL: 1, BattMinV: &battMin, BattMaxV: &battMax,
	})
	if err != nil {
		t.Fatalf("upsert overlapping vehicle trip: %v", err)
	}
	if _, err := d.VehicleTripsUpsert(ctx, dashcamSrcID, VehicleTrip{
		RemoteTripID: "vt-no-overlap", StartTime: "2026-01-01T11:00:00", EndTime: "2026-01-01T11:10:00",
		DTCs: []string{"SHOULD_NOT_APPEAR"},
	}); err != nil {
		t.Fatalf("upsert non-overlapping vehicle trip: %v", err)
	}

	sum, err := d.VehicleTripSummary(ctx, tripID)
	if err != nil {
		t.Fatalf("VehicleTripSummary: %v", err)
	}
	if sum.DistanceM != 2500 || math.Abs(sum.FuelUsedL-0.3) > 1e-9 || sum.MaxSpeedKph != 95 {
		t.Errorf("distance/fuel/maxspeed = %v/%v/%v, want 2500/0.3/95 (front-only)", sum.DistanceM, sum.FuelUsedL, sum.MaxSpeedKph)
	}
	if sum.EventCounts["hard_brake"] != 2 {
		t.Errorf("event counts = %v, want hard_brake:2", sum.EventCounts)
	}
	if !sum.MIL {
		t.Error("MIL = false, want true (from overlapping vehicle_trips row)")
	}
	if len(sum.DTCs) != 1 || sum.DTCs[0] != "P0300" {
		t.Errorf("DTCs = %v, want [P0300] (non-overlapping trip's DTC must not appear)", sum.DTCs)
	}
	if sum.BattMinV == nil || *sum.BattMinV != battMin {
		t.Errorf("BattMinV = %v, want %v", sum.BattMinV, battMin)
	}
	if sum.BattMaxV == nil || *sum.BattMaxV != battMax {
		t.Errorf("BattMaxV = %v, want %v", sum.BattMaxV, battMax)
	}
	if len(sum.OverlappingVehicleTripIDs) != 1 || sum.OverlappingVehicleTripIDs[0] != overlapping {
		t.Errorf("OverlappingVehicleTripIDs = %v, want [%d]", sum.OverlappingVehicleTripIDs, overlapping)
	}
}

// -- DashcamGetVehicleCursors/DashcamSetVehicleCursors --------------------

func TestDashcamVehicleCursorsRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}

	trips, events, err := d.DashcamGetVehicleCursors(ctx, dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGetVehicleCursors: %v", err)
	}
	if trips != nil || events != nil {
		t.Fatalf("initial cursors = %v/%v, want nil/nil", trips, events)
	}

	tv, ev := int64(42), int64(7)
	if err := d.DashcamSetVehicleCursors(ctx, dashcamSrcID, &tv, &ev); err != nil {
		t.Fatalf("DashcamSetVehicleCursors: %v", err)
	}
	trips, events, err = d.DashcamGetVehicleCursors(ctx, dashcamSrcID)
	if err != nil {
		t.Fatalf("DashcamGetVehicleCursors (after set): %v", err)
	}
	if trips == nil || *trips != 42 || events == nil || *events != 7 {
		t.Fatalf("cursors after set = %v/%v, want 42/7", trips, events)
	}
}

func TestDashcamSourceIDsForTargetShare(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	shareID := mustInsertShare(t, d, ctx)
	otherShareID := mustInsertShare(t, d, ctx)
	dashcamSrcID, err := d.DashcamCreate(ctx, "cam", "http://x", shareID)
	if err != nil {
		t.Fatalf("DashcamCreate: %v", err)
	}
	if _, err := d.DashcamCreate(ctx, "other cam", "http://y", otherShareID); err != nil {
		t.Fatalf("DashcamCreate (other): %v", err)
	}

	ids, err := d.DashcamSourceIDsForTargetShare(ctx, shareID)
	if err != nil {
		t.Fatalf("DashcamSourceIDsForTargetShare: %v", err)
	}
	if len(ids) != 1 || ids[0] != dashcamSrcID {
		t.Fatalf("ids = %v, want [%d]", ids, dashcamSrcID)
	}
}
