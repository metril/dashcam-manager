package scan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

	var sawCar, sawPerson, sawPlate, sawVehicle, sawGPS bool
	for _, frame := range d.DetectionFrames {
		for _, obj := range frame.Objects {
			switch obj.Class {
			case "car":
				sawCar = true
			case "person":
				sawPerson = true
			}
			// Every normalized coordinate must be clamped to 0..1.
			for _, v := range []float64{obj.Norm.X1, obj.Norm.Y1, obj.Norm.X2, obj.Norm.Y2} {
				if v < 0 || v > 1 {
					t.Errorf("Norm coordinate %v out of 0..1 range for class %s", v, obj.Class)
				}
			}
		}
		if len(frame.Plates) > 0 {
			sawPlate = true
		}
		if len(frame.Vehicles) > 0 {
			sawVehicle = true
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
}

func TestParseDetectionsMissingFile(t *testing.T) {
	_, err := ParseDetections(sample("does_not_exist.detections.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
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
}

func TestParseGPSMissingFile(t *testing.T) {
	_, err := ParseGPS(sample("does_not_exist.gps.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got: %v", err)
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
