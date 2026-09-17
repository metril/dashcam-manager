package scan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// emptyFixture is a real 0-byte sidecar fixture: 221 sidecars on the actual
// SSD are empty (writer interrupted mid-flush), and all three parsers must
// treat that as ErrEmptySidecar rather than a generic decode failure.
const emptyFixture = "empty.json"

const testdataDir = "testdata"

func sample(name string) string {
	return filepath.Join(testdataDir, name)
}

func TestParseTimestamp(t *testing.T) {
	ts, err := ParseTimestamp(sample("segment_00259.timestamp.json"))
	if err != nil {
		t.Fatalf("ParseTimestamp: %v", err)
	}
	if ts.Segment != "segment_00259.mp4" {
		t.Errorf("Segment = %q, want segment_00259.mp4", ts.Segment)
	}
	if ts.Camera != "front" {
		t.Errorf("Camera = %q, want front", ts.Camera)
	}
	wantStart := time.Date(2026, 9, 3, 21, 35, 0, 562000000, location())
	if !ts.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", ts.StartTime, wantStart)
	}
	if !ts.EndTime.After(ts.StartTime) {
		t.Errorf("EndTime %v should be after StartTime %v", ts.EndTime, ts.StartTime)
	}
}

func TestParseTimestampMissingFile(t *testing.T) {
	_, err := ParseTimestamp(sample("does_not_exist.timestamp.json"))
	if err == nil {
		t.Fatal("expected error for missing sidecar, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
	}
}

func TestParseTimestampEmptySidecar(t *testing.T) {
	_, err := ParseTimestamp(sample(emptyFixture))
	if !errors.Is(err, ErrEmptySidecar) {
		t.Errorf("expected error wrapping ErrEmptySidecar, got: %v", err)
	}
}

func TestParseDetections(t *testing.T) {
	d, err := ParseDetections(sample("segment_00259.detections.json"))
	if err != nil {
		t.Fatalf("ParseDetections: %v", err)
	}
	if d.FrameWidth != 640 || d.FrameHeight != 640 {
		t.Fatalf("frame dims = %dx%d, want 640x640", d.FrameWidth, d.FrameHeight)
	}
	if len(d.DetectionFrames) == 0 {
		t.Fatal("expected at least one detection frame")
	}

	inRange := func(v float64) bool { return v >= 0 && v <= 1 }
	checkNormBox := func(t *testing.T, label string, nb NormBox) {
		t.Helper()
		if !inRange(nb.X1) || !inRange(nb.Y1) || !inRange(nb.X2) || !inRange(nb.Y2) {
			t.Errorf("%s Norm %+v has a coordinate out of 0..1 range", label, nb)
		}
	}

	var sawCar, sawPerson, sawPlate, sawVehicle, sawGPS, sawWallTime bool
	for _, frame := range d.DetectionFrames {
		if !frame.WallTimeT.IsZero() {
			sawWallTime = true
		}
		for _, obj := range frame.Objects {
			switch obj.Class {
			case "car":
				sawCar = true
			case "person":
				sawPerson = true
			}
			checkNormBox(t, "Object", obj.Norm)
		}
		for _, p := range frame.Plates {
			sawPlate = true
			checkNormBox(t, "Plate", p.Norm)
			checkNormBox(t, "Plate.VehicleNorm", p.VehicleNorm)
		}
		for _, v := range frame.Vehicles {
			sawVehicle = true
			checkNormBox(t, "Vehicle", v.Norm)
		}
		if frame.GPS != nil {
			sawGPS = true
		}
	}
	if !sawCar || !sawPerson {
		t.Errorf("expected both car and person classes in sample, sawCar=%v sawPerson=%v", sawCar, sawPerson)
	}
	if !sawPlate {
		t.Error("expected at least one plate detection in sample")
	}
	if !sawVehicle {
		t.Error("expected at least one vehicle classification in sample")
	}
	if !sawGPS {
		t.Error("expected at least one frame with embedded GPS in sample")
	}
	if !sawWallTime {
		t.Error("expected at least one frame with a populated WallTimeT")
	}
	if d.StartTimeT.IsZero() {
		t.Error("expected non-zero StartTimeT")
	}
	if d.EndTimeT.IsZero() {
		t.Error("expected non-zero EndTimeT")
	}
	if !d.EndTimeT.After(d.StartTimeT) {
		t.Errorf("EndTimeT %v should be after StartTimeT %v", d.EndTimeT, d.StartTimeT)
	}
}

func TestParseDetectionsMissingFile(t *testing.T) {
	_, err := ParseDetections(sample("does_not_exist.detections.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
	}
}

func TestParseDetectionsEmptySidecar(t *testing.T) {
	_, err := ParseDetections(sample(emptyFixture))
	if !errors.Is(err, ErrEmptySidecar) {
		t.Errorf("expected error wrapping ErrEmptySidecar, got: %v", err)
	}
}

// TestParseDetectionsDefaultFrameDims verifies that a sidecar missing (or
// zeroing) frame_width/frame_height falls back to defaultFrameDim (640)
// instead of silently zeroing every normalized bbox.
func TestParseDetectionsDefaultFrameDims(t *testing.T) {
	const raw = `{
		"segment": "segment_00000.mp4",
		"camera": "front",
		"detections": [
			{
				"wall_time": "2026-01-01T00:00:00.000",
				"objects": [{"class": "car", "confidence": 0.9, "bbox": [0, 66, 277, 321]}]
			}
		]
	}`
	path := filepath.Join(t.TempDir(), "no_dims.detections.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := ParseDetections(path)
	if err != nil {
		t.Fatalf("ParseDetections: %v", err)
	}
	if d.FrameWidth != 0 || d.FrameHeight != 0 {
		t.Fatalf("raw FrameWidth/FrameHeight should stay as decoded (0,0), got (%d,%d)", d.FrameWidth, d.FrameHeight)
	}
	got := d.DetectionFrames[0].Objects[0].Norm
	want := NormBox{X1: 0, Y1: 0.103125, X2: 0.4328125, Y2: 0.5015625}
	if got != want {
		t.Errorf("Norm with missing frame dims = %+v, want %+v (defaulted to 640x640)", got, want)
	}
}

// TestBox4UnmarshalTooShort verifies a bbox array with fewer than 4
// elements is a decode error, not silently zero-padded.
func TestBox4UnmarshalTooShort(t *testing.T) {
	const raw = `{
		"segment": "segment_00000.mp4",
		"camera": "front",
		"frame_width": 640,
		"frame_height": 640,
		"detections": [
			{"wall_time": "", "objects": [{"class": "car", "confidence": 0.9, "bbox": [0, 66, 277]}]}
		]
	}`
	path := filepath.Join(t.TempDir(), "short_bbox.detections.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDetections(path); err == nil {
		t.Fatal("expected error for bbox with fewer than 4 elements, got nil")
	}
}

// TestNormalizeBBoxGolden pins the exact mapping specified in the M3 brief:
// bbox [0,66,277,321] at 640x640 -> (0, 0.103125, 0.4328125, 0.5015625).
func TestNormalizeBBoxGolden(t *testing.T) {
	got := NormalizeBBox([4]int{0, 66, 277, 321}, 640, 640)
	want := NormBox{X1: 0, Y1: 0.103125, X2: 0.4328125, Y2: 0.5015625}
	if got != want {
		t.Errorf("NormalizeBBox = %+v, want %+v", got, want)
	}
}

func TestNormalizeBBoxClamps(t *testing.T) {
	got := NormalizeBBox([4]int{-10, -10, 700, 700}, 640, 640)
	want := NormBox{X1: 0, Y1: 0, X2: 1, Y2: 1}
	if got != want {
		t.Errorf("NormalizeBBox clamping = %+v, want %+v", got, want)
	}
}

func TestDetectionsSummary(t *testing.T) {
	d := &Detections{
		DetectionFrames: []DetectionFrame{
			{Objects: []Object{{Class: "car"}, {Class: "car"}, {Class: "person"}}, Plates: []Plate{{Text: "ABC123"}}},
			{Objects: []Object{{Class: "truck"}}, Plates: []Plate{{Text: "1"}, {Text: "2"}}},
		},
	}
	sum := d.Summary()
	if sum.ClassCounts["car"] != 2 {
		t.Errorf("car count = %d, want 2", sum.ClassCounts["car"])
	}
	if sum.ClassCounts["person"] != 1 || sum.ClassCounts["truck"] != 1 {
		t.Errorf("unexpected counts: %+v", sum.ClassCounts)
	}
	if sum.PlateCount != 3 {
		t.Errorf("PlateCount = %d, want 3", sum.PlateCount)
	}
	if len(sum.TopClasses) == 0 || sum.TopClasses[0] != "car" {
		t.Errorf("TopClasses = %v, want car first (highest count)", sum.TopClasses)
	}
}

func TestParseGPS(t *testing.T) {
	g, err := ParseGPS(sample("segment_00259.gps.json"))
	if err != nil {
		t.Fatalf("ParseGPS: %v", err)
	}
	if len(g.Points) == 0 {
		t.Fatal("expected GPS points")
	}
	for i, p := range g.Points {
		if p.T.IsZero() {
			t.Errorf("point %d has zero T", i)
		}
	}
	if g.StartLat == 0 && g.StartLon == 0 {
		t.Error("expected non-zero start_lat/start_lon")
	}
	if g.StartTimeT.IsZero() {
		t.Error("expected non-zero StartTimeT")
	}
	if g.EndTimeT.IsZero() {
		t.Error("expected non-zero EndTimeT")
	}
	if !g.EndTimeT.After(g.StartTimeT) {
		t.Errorf("EndTimeT %v should be after StartTimeT %v", g.EndTimeT, g.StartTimeT)
	}
}

func TestParseGPSMissingFile(t *testing.T) {
	_, err := ParseGPS(sample("does_not_exist.gps.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
	}
}

func TestParseGPSEmptySidecar(t *testing.T) {
	_, err := ParseGPS(sample(emptyFixture))
	if !errors.Is(err, ErrEmptySidecar) {
		t.Errorf("expected error wrapping ErrEmptySidecar, got: %v", err)
	}
}

func TestDecimate(t *testing.T) {
	g, err := ParseGPS(sample("segment_00259.gps.json"))
	if err != nil {
		t.Fatalf("ParseGPS: %v", err)
	}
	dec := Decimate(g.Points, time.Second)
	if len(dec) == 0 {
		t.Fatal("expected decimated points")
	}
	if len(dec) > len(g.Points) {
		t.Fatalf("decimated %d > original %d", len(dec), len(g.Points))
	}
	for i := 1; i < len(dec); i++ {
		gap := dec[i].T.Sub(dec[i-1].T)
		if gap < time.Second {
			t.Errorf("decimated points %d,%d only %v apart, want >= 1s", i-1, i, gap)
		}
	}
}

func TestDecimateEmpty(t *testing.T) {
	if got := Decimate(nil, time.Second); got != nil {
		t.Errorf("Decimate(nil) = %v, want nil", got)
	}
}

func TestParseOBD(t *testing.T) {
	o, err := ParseOBD(sample("segment_00259.obd.json"))
	if err != nil {
		t.Fatalf("ParseOBD: %v", err)
	}
	if o.Segment != "segment_00259.mp4" {
		t.Errorf("Segment = %q, want segment_00259.mp4", o.Segment)
	}
	if o.Camera != "front" {
		t.Errorf("Camera = %q, want front", o.Camera)
	}
	if o.MaxSpeedKph != 34 {
		t.Errorf("MaxSpeedKph = %v, want 34", o.MaxSpeedKph)
	}
	if o.AvgRPM != 1344.7 {
		t.Errorf("AvgRPM = %v, want 1344.7", o.AvgRPM)
	}
	if o.DistanceM != 194.8 {
		t.Errorf("DistanceM = %v, want 194.8", o.DistanceM)
	}
	if o.FuelUsedL != 0.031 {
		t.Errorf("FuelUsedL = %v, want 0.031", o.FuelUsedL)
	}
	if len(o.Points) != 171 {
		t.Fatalf("len(Points) = %d, want 171", len(o.Points))
	}
	if o.StartTimeT.IsZero() {
		t.Error("expected non-zero StartTimeT")
	}
	if o.EndTimeT.IsZero() {
		t.Error("expected non-zero EndTimeT")
	}
	if !o.EndTimeT.After(o.StartTimeT) {
		t.Errorf("EndTimeT %v should be after StartTimeT %v", o.EndTimeT, o.StartTimeT)
	}

	p0 := o.Points[0]
	wantT := time.Date(2026, 9, 16, 18, 37, 0, 53000000, location())
	if !p0.T.Equal(wantT) {
		t.Errorf("Points[0].T = %v, want %v", p0.T, wantT)
	}
	if p0.SpeedKph == nil || *p0.SpeedKph != 32 {
		t.Errorf("Points[0].SpeedKph = %v, want 32", p0.SpeedKph)
	}
	if p0.RPM == nil || *p0.RPM != 1125 {
		t.Errorf("Points[0].RPM = %v, want 1125", p0.RPM)
	}
}

func TestParseOBDMissingFile(t *testing.T) {
	_, err := ParseOBD(sample("does_not_exist.obd.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
	}
}

func TestParseOBDEmptySidecar(t *testing.T) {
	_, err := ParseOBD(sample(emptyFixture))
	if !errors.Is(err, ErrEmptySidecar) {
		t.Errorf("expected error wrapping ErrEmptySidecar, got: %v", err)
	}
}

// TestParseOBDMissingNullFields covers a point with an omitted field and a
// point with an explicit JSON null: both must parse as a nil pointer (NULL
// when indexed), never silently as 0.
func TestParseOBDMissingNullFields(t *testing.T) {
	raw := `{"segment":"segment_00000.mp4","camera":"front","start_time":"2026-02-27T00:16:54.000","end_time":"2026-02-27T00:16:59.000","max_speed_kph":10,"avg_rpm":900,"distance_m":50,"fuel_used_l":0.01,"points":[` +
		`{"t":"2026-02-27T00:16:54.000","speed_kph":10,"rpm":900,"throttle_pct":5,"accel_mps2":0,"coolant_c":90,"batt_v":12.6},` +
		`{"t":"2026-02-27T00:16:54.500","speed_kph":null,"rpm":null,"accel_mps2":0,"coolant_c":90,"batt_v":12.6}` +
		`]}`
	path := filepath.Join(t.TempDir(), "missing_null.obd.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := ParseOBD(path)
	if err != nil {
		t.Fatalf("ParseOBD: %v", err)
	}
	if len(o.Points) != 2 {
		t.Fatalf("len(Points) = %d, want 2", len(o.Points))
	}
	p := o.Points[1]
	if p.SpeedKph != nil {
		t.Errorf("Points[1].SpeedKph = %v, want nil (JSON null)", *p.SpeedKph)
	}
	if p.RPM != nil {
		t.Errorf("Points[1].RPM = %v, want nil (JSON null)", *p.RPM)
	}
	if p.ThrottlePct != nil {
		t.Errorf("Points[1].ThrottlePct = %v, want nil (omitted field)", *p.ThrottlePct)
	}
	if p.CoolantC == nil || *p.CoolantC != 90 {
		t.Errorf("Points[1].CoolantC = %v, want 90 (present field unaffected)", p.CoolantC)
	}
}

func TestDecimateOBD(t *testing.T) {
	o, err := ParseOBD(sample("segment_00259.obd.json"))
	if err != nil {
		t.Fatalf("ParseOBD: %v", err)
	}
	dec := DecimateOBD(o.Points, 500*time.Millisecond)
	if len(dec) == 0 {
		t.Fatal("expected decimated points")
	}
	if len(dec) > len(o.Points) {
		t.Fatalf("decimated %d > original %d", len(dec), len(o.Points))
	}
	for i := 1; i < len(dec); i++ {
		gap := dec[i].T.Sub(dec[i-1].T)
		if gap < 500*time.Millisecond {
			t.Errorf("decimated points %d,%d only %v apart, want >= 500ms", i-1, i, gap)
		}
	}
}

func TestDecimateOBDEmpty(t *testing.T) {
	if got := DecimateOBD(nil, 500*time.Millisecond); got != nil {
		t.Errorf("DecimateOBD(nil) = %v, want nil", got)
	}
}

func TestIsJunk(t *testing.T) {
	cases := map[string]bool{
		"._segment_00000.mp4":          true,
		".DS_Store":                    true,
		".Spotlight-V100":              true,
		"segment_00000.mp4":            false,
		"segment_00000.timestamp.json": false,
		".hidden":                      false,
	}
	for name, want := range cases {
		if got := IsJunk(name); got != want {
			t.Errorf("IsJunk(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseSegmentPath(t *testing.T) {
	sp, err := ParseSegmentPath("front/2026/09/03/recording_20260903_192528/segment_00259.mp4")
	if err != nil {
		t.Fatalf("ParseSegmentPath: %v", err)
	}
	if sp.Camera != "front" {
		t.Errorf("Camera = %q, want front", sp.Camera)
	}
	if sp.RecordingKey != "recording_20260903_192528" {
		t.Errorf("RecordingKey = %q, want recording_20260903_192528", sp.RecordingKey)
	}
	if sp.Seq != 259 {
		t.Errorf("Seq = %d, want 259", sp.Seq)
	}
	wantDate := time.Date(2026, 9, 3, 0, 0, 0, 0, location())
	if !sp.Date.Equal(wantDate) {
		t.Errorf("Date = %v, want %v", sp.Date, wantDate)
	}
}

func TestParseSegmentPathRear(t *testing.T) {
	sp, err := ParseSegmentPath("rear/2026/02/27/recording_20260227_001654/segment_00007.mp4")
	if err != nil {
		t.Fatalf("ParseSegmentPath: %v", err)
	}
	if sp.Camera != "rear" {
		t.Errorf("Camera = %q, want rear", sp.Camera)
	}
	if sp.Seq != 7 {
		t.Errorf("Seq = %d, want 7", sp.Seq)
	}
}

// TestParseDetectionsBytesMergesSamePTSFrames verifies the frame-hygiene
// pass (A1): two frames sharing a pts_offset — one objects-only, one
// vehicles-only, as observed on the real corpus (segment 5606) — merge
// into a single frame with unioned objects/vehicles, the earliest
// wall_time, and frames renumbered/reordered by pts_offset.
func TestParseDetectionsBytesMergesSamePTSFrames(t *testing.T) {
	const raw = `{
		"segment": "segment_00000.mp4",
		"camera": "front",
		"frame_width": 640,
		"frame_height": 640,
		"detections": [
			{
				"pts_offset": 8.900,
				"wall_time": "2026-01-01T00:00:01.000",
				"objects": [{"class": "bus", "confidence": 0.9, "bbox": [0, 0, 10, 10]}]
			},
			{
				"pts_offset": 8.900,
				"wall_time": "2026-01-01T00:00:00.500",
				"vehicles": [{"type": "largevehicle", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.796, "bbox": [333, 341, 402, 437]}]
			}
		]
	}`
	d, err := ParseDetectionsBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseDetectionsBytes: %v", err)
	}
	if len(d.DetectionFrames) != 1 {
		t.Fatalf("len(DetectionFrames) = %d, want 1 (merged)", len(d.DetectionFrames))
	}
	f := d.DetectionFrames[0]
	if len(f.Objects) != 1 || f.Objects[0].Class != "bus" {
		t.Errorf("merged Objects = %+v, want one bus", f.Objects)
	}
	if len(f.Vehicles) != 1 || f.Vehicles[0].Make != "toyota" {
		t.Errorf("merged Vehicles = %+v, want one toyota", f.Vehicles)
	}
	if f.FrameIndex != 0 {
		t.Errorf("FrameIndex = %d, want 0", f.FrameIndex)
	}
	wantWall := time.Date(2026, 1, 1, 0, 0, 0, 500000000, location())
	if !f.WallTimeT.Equal(wantWall) {
		t.Errorf("WallTimeT = %v, want earliest %v", f.WallTimeT, wantWall)
	}
}

// TestParseDetectionsBytesDropsEchoedVehicles pins the segment-6840-derived
// fixture from the plan: each frame after the first echoes the previous
// frame's vehicle entry byte-for-byte alongside the new classification.
// The echo must be dropped, leaving exactly one (the new) vehicle entry per
// frame after frame 0.
func TestParseDetectionsBytesDropsEchoedVehicles(t *testing.T) {
	const raw = `{
		"segment": "segment_00094.mp4",
		"camera": "front",
		"frame_width": 640,
		"frame_height": 640,
		"detections": [
			{"pts_offset": 8.900, "wall_time": "2026-03-22T05:30:59.000",
			 "vehicles": [{"type": "largevehicle", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.796, "bbox": [333,341,402,437]}]},
			{"pts_offset": 9.100, "wall_time": "2026-03-22T05:30:59.200",
			 "vehicles": [
				{"type": "largevehicle", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.796, "bbox": [333,341,402,437]},
				{"type": "largevehicle", "type_confidence": 1.0, "make": "honda", "make_confidence": 0.602, "bbox": [333,338,401,435]}
			 ]},
			{"pts_offset": 9.264, "wall_time": "2026-03-22T05:30:59.364",
			 "vehicles": [
				{"type": "largevehicle", "type_confidence": 1.0, "make": "honda", "make_confidence": 0.602, "bbox": [333,338,401,435]},
				{"type": "largevehicle", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.567, "bbox": [335,334,401,433]}
			 ]},
			{"pts_offset": 9.433, "wall_time": "2026-03-22T05:30:59.533",
			 "vehicles": [
				{"type": "largevehicle", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.567, "bbox": [335,334,401,433]},
				{"type": "largevehicle", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.691, "bbox": [335,330,401,431]}
			 ]}
		]
	}`
	d, err := ParseDetectionsBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseDetectionsBytes: %v", err)
	}
	if len(d.DetectionFrames) != 4 {
		t.Fatalf("len(DetectionFrames) = %d, want 4 (no same-pts merge in this fixture)", len(d.DetectionFrames))
	}
	wantMakes := []string{"toyota", "honda", "toyota", "toyota"}
	wantConfs := []float64{0.796, 0.602, 0.567, 0.691}
	for i, f := range d.DetectionFrames {
		if len(f.Vehicles) != 1 {
			t.Fatalf("frame %d Vehicles = %+v, want exactly 1 (echo dropped)", i, f.Vehicles)
		}
		if f.Vehicles[0].Make != wantMakes[i] {
			t.Errorf("frame %d Make = %q, want %q", i, f.Vehicles[0].Make, wantMakes[i])
		}
		if f.Vehicles[0].MakeConfidence != wantConfs[i] {
			t.Errorf("frame %d MakeConfidence = %v, want %v", i, f.Vehicles[0].MakeConfidence, wantConfs[i])
		}
	}
}

// TestParseDetectionsBytesKeepsNonIdenticalOverlappingVehicles verifies
// that a vehicle entry overlapping the previous frame's but differing in
// any field (here, bbox) is kept rather than dropped — dropEchoedEntries
// is an exact-match filter, not IoU-based.
func TestParseDetectionsBytesKeepsNonIdenticalOverlappingVehicles(t *testing.T) {
	const raw = `{
		"segment": "segment_00000.mp4",
		"camera": "front",
		"frame_width": 640,
		"frame_height": 640,
		"detections": [
			{"pts_offset": 1.0, "wall_time": "2026-01-01T00:00:00.000",
			 "vehicles": [{"type": "sedan", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.8, "bbox": [0,0,100,100]}]},
			{"pts_offset": 1.1, "wall_time": "2026-01-01T00:00:00.100",
			 "vehicles": [{"type": "sedan", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.8, "bbox": [1,0,100,100]}]}
		]
	}`
	d, err := ParseDetectionsBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseDetectionsBytes: %v", err)
	}
	if len(d.DetectionFrames[1].Vehicles) != 1 {
		t.Fatalf("frame 1 Vehicles = %+v, want 1 kept (differs by bbox, not an echo)", d.DetectionFrames[1].Vehicles)
	}
}

// TestParseDetectionsBytesKeepsStationaryVehicleAcrossFrames covers the
// dropEchoedEntries safeguard: a truly stationary vehicle detected
// identically in 3 consecutive frames must be kept in all 3 (an
// all-entries-echoed frame reverts to its original list rather than being
// filtered to empty), while a genuine echo-pair case elsewhere still drops
// the echo.
func TestParseDetectionsBytesKeepsStationaryVehicleAcrossFrames(t *testing.T) {
	const raw = `{
		"segment": "segment_00000.mp4",
		"camera": "front",
		"frame_width": 640,
		"frame_height": 640,
		"detections": [
			{"pts_offset": 1.0, "wall_time": "2026-01-01T00:00:00.000",
			 "vehicles": [{"type": "sedan", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.8, "bbox": [0,0,100,100]}]},
			{"pts_offset": 1.1, "wall_time": "2026-01-01T00:00:00.100",
			 "vehicles": [{"type": "sedan", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.8, "bbox": [0,0,100,100]}]},
			{"pts_offset": 1.2, "wall_time": "2026-01-01T00:00:00.200",
			 "vehicles": [{"type": "sedan", "type_confidence": 1.0, "make": "toyota", "make_confidence": 0.8, "bbox": [0,0,100,100]}]}
		]
	}`
	d, err := ParseDetectionsBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseDetectionsBytes: %v", err)
	}
	if len(d.DetectionFrames) != 3 {
		t.Fatalf("len(DetectionFrames) = %d, want 3", len(d.DetectionFrames))
	}
	for i, f := range d.DetectionFrames {
		if len(f.Vehicles) != 1 {
			t.Fatalf("frame %d Vehicles = %+v, want exactly 1 (all-echoed frame must revert to original, not empty)", i, f.Vehicles)
		}
	}
}

func TestParseSegmentPathInvalid(t *testing.T) {
	cases := []string{
		"",
		"front/segment_00000.mp4",
		"left/2026/09/03/recording_20260903_192528/segment_00259.mp4",
		"front/2026/09/03/recording_20260903_192528/segment_00259.mov",
		"front/2026/09/03/not_a_recording_dir/segment_00259.mp4",
	}
	for _, rel := range cases {
		if _, err := ParseSegmentPath(rel); err == nil {
			t.Errorf("ParseSegmentPath(%q): expected error, got nil", rel)
		}
	}
}
