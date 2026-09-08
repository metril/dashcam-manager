// Package scan parses the recorder's per-segment JSON sidecars
// (.timestamp.json, .detections.json, .gps.json) and the directory layout
// they live in, normalizing detection bounding boxes to 0..1 coordinates.
package scan

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// timeLayout matches the recorder's local naive ISO timestamps, e.g.
// "2026-02-27T00:16:54.947".
const timeLayout = "2006-01-02T15:04:05.999999999"

// location returns the time.Location used to interpret the recorder's naive
// local timestamps: the TZ environment variable if set and valid, else
// time.Local.
func location() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// parseLocalTime parses a naive ISO timestamp string (no timezone offset)
// using the configured location.
func parseLocalTime(s string) (time.Time, error) {
	return time.ParseInLocation(timeLayout, s, location())
}

// ---- .timestamp.json ----

// Timestamp is the always-present per-segment sidecar recording the
// segment's wall-clock start/end time.
type Timestamp struct {
	Segment      string    `json:"segment"`
	Camera       string    `json:"camera"`
	StartTimeRaw string    `json:"start_time"`
	EndTimeRaw   string    `json:"end_time"`
	StartTime    time.Time `json:"-"`
	EndTime      time.Time `json:"-"`
}

// ParseTimestamp reads and parses a .timestamp.json sidecar at path.
func ParseTimestamp(path string) (*Timestamp, error) {
	var ts Timestamp
	if err := readJSON(path, &ts); err != nil {
		return nil, fmt.Errorf("parse timestamp %s: %w", path, err)
	}
	start, err := parseLocalTime(ts.StartTimeRaw)
	if err != nil {
		return nil, fmt.Errorf("parse timestamp %s: start_time: %w", path, err)
	}
	end, err := parseLocalTime(ts.EndTimeRaw)
	if err != nil {
		return nil, fmt.Errorf("parse timestamp %s: end_time: %w", path, err)
	}
	ts.StartTime = start
	ts.EndTime = end
	return &ts, nil
}

// ---- .detections.json ----

// NormBox is a bounding box normalized to 0..1 coordinates within the
// inference frame (frame_width x frame_height), clamped to that range.
type NormBox struct {
	X1, Y1, X2, Y2 float64
}

// NormalizeBBox scales an integer pixel bbox [x1,y1,x2,y2] from a
// frameW x frameH inference frame into 0..1 coordinates, clamping any
// out-of-range values.
func NormalizeBBox(bbox [4]int, frameW, frameH int) NormBox {
	clamp := func(v float64) float64 {
		if v < 0 {
			return 0
		}
		if v > 1 {
			return 1
		}
		return v
	}
	w, h := float64(frameW), float64(frameH)
	if w == 0 || h == 0 {
		return NormBox{}
	}
	return NormBox{
		X1: clamp(float64(bbox[0]) / w),
		Y1: clamp(float64(bbox[1]) / h),
		X2: clamp(float64(bbox[2]) / w),
		Y2: clamp(float64(bbox[3]) / h),
	}
}

// Object is one classified detection within a frame.
type Object struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
	BBox       [4]int  `json:"bbox"`
	// Norm is computed by ParseDetections from BBox and the sidecar's
	// frame_width/frame_height; it is not present in the raw JSON.
	Norm NormBox `json:"-"`
}

// Plate is a license-plate detection, optionally linked to the vehicle it
// was read from.
type Plate struct {
	Text        string  `json:"text"`
	Confidence  float64 `json:"confidence"`
	BBox        [4]int  `json:"bbox"`
	VehicleBBox [4]int  `json:"vehicle_bbox"`
}

// Vehicle is a make/type classification for a detected vehicle.
type Vehicle struct {
	Type           string  `json:"type"`
	TypeConfidence float64 `json:"type_confidence"`
	Make           string  `json:"make,omitempty"`
	MakeConfidence float64 `json:"make_confidence,omitempty"`
	BBox           [4]int  `json:"bbox"`
}

// FrameGPS is the GPS fix attached to a detection frame, when available.
type FrameGPS struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	SpeedMps float64 `json:"speed_mps"`
	Heading  float64 `json:"heading"`
	Sats     int     `json:"sats"`
	Fix      int     `json:"fix"`
}

// DetectionFrame is one analyzed frame's detections.
type DetectionFrame struct {
	Timestamp  float64   `json:"timestamp"`
	WallTime   string    `json:"wall_time"`
	Objects    []Object  `json:"objects"`
	Plates     []Plate   `json:"plates,omitempty"`
	Vehicles   []Vehicle `json:"vehicles,omitempty"`
	FrameIndex int       `json:"frame_index"`
	PTSOffset  float64   `json:"pts_offset"`
	GPS        *FrameGPS `json:"gps,omitempty"`
}

// Detections is the parsed .detections.json sidecar.
type Detections struct {
	Segment                 string           `json:"segment"`
	Camera                  string           `json:"camera"`
	FrameWidth              int              `json:"frame_width"`
	FrameHeight             int              `json:"frame_height"`
	FramesAnalyzed          int              `json:"frames_analyzed"`
	StartTime               string           `json:"start_time"`
	EndTime                 string           `json:"end_time"`
	PTSStart                float64          `json:"pts_start"`
	PTSEnd                  float64          `json:"pts_end"`
	TotalObjects            int              `json:"total_objects"`
	TotalPlates             int              `json:"total_plates"`
	TotalVehiclesClassified int              `json:"total_vehicles_classified"`
	DetectionFrames         []DetectionFrame `json:"detections"`
}

// ParseDetections reads and parses a .detections.json sidecar at path,
// populating Object.Norm on every detected object from the sidecar's
// frame_width/frame_height.
func ParseDetections(path string) (*Detections, error) {
	var d Detections
	if err := readJSON(path, &d); err != nil {
		return nil, fmt.Errorf("parse detections %s: %w", path, err)
	}
	for fi := range d.DetectionFrames {
		objs := d.DetectionFrames[fi].Objects
		for oi := range objs {
			objs[oi].Norm = NormalizeBBox(objs[oi].BBox, d.FrameWidth, d.FrameHeight)
		}
	}
	return &d, nil
}

// detectionClasses are the object classes counted by Summary, in the
// canonical order used to break ties in TopClasses.
var detectionClasses = []string{"car", "truck", "bus", "person", "bicycle", "motorcycle"}

// DetectionSummary is a per-class rollup of a Detections sidecar.
type DetectionSummary struct {
	ClassCounts map[string]int
	PlateCount  int
	TopClasses  []string
}

// Summary computes per-class detection counts (car, truck, bus, person,
// bicycle, motorcycle), the total plate count, and classes ordered by count
// descending (ties broken by the canonical class order, then alphabetically
// for any class outside it).
func (d *Detections) Summary() DetectionSummary {
	counts := make(map[string]int, len(detectionClasses))
	plateCount := 0
	for _, frame := range d.DetectionFrames {
		for _, obj := range frame.Objects {
			counts[obj.Class]++
		}
		plateCount += len(frame.Plates)
	}

	classRank := make(map[string]int, len(detectionClasses))
	for i, c := range detectionClasses {
		classRank[c] = i
	}

	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		ci, cj := classes[i], classes[j]
		if counts[ci] != counts[cj] {
			return counts[ci] > counts[cj]
		}
		ri, iok := classRank[ci]
		rj, jok := classRank[cj]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		default:
			return ci < cj
		}
	})

	return DetectionSummary{
		ClassCounts: counts,
		PlateCount:  plateCount,
		TopClasses:  classes,
	}
}

// ---- .gps.json ----

// GPSPoint is one GPS fix. Heading, AltM, Sats are pointers because the
// recorder emits JSON null for them when unavailable.
type GPSPoint struct {
	TRaw     string   `json:"t"`
	Lat      float64  `json:"lat"`
	Lon      float64  `json:"lon"`
	SpeedMps float64  `json:"speed_mps"`
	Heading  *float64 `json:"heading"`
	AltM     *float64 `json:"alt_m"`
	Sats     *int     `json:"sats"`
	Fix      int      `json:"fix"`
	// T is parsed by ParseGPS from TRaw; not present in the raw JSON.
	T time.Time `json:"-"`
}

// GPS is the parsed .gps.json sidecar.
type GPS struct {
	Segment     string     `json:"segment"`
	Camera      string     `json:"camera"`
	StartTime   string     `json:"start_time"`
	EndTime     string     `json:"end_time"`
	StartLat    float64    `json:"start_lat"`
	StartLon    float64    `json:"start_lon"`
	EndLat      float64    `json:"end_lat"`
	EndLon      float64    `json:"end_lon"`
	DistanceM   float64    `json:"distance_m"`
	MaxSpeedMps float64    `json:"max_speed_mps"`
	Points      []GPSPoint `json:"points"`
}

// ParseGPS reads and parses a .gps.json sidecar at path, populating
// GPSPoint.T on every point.
func ParseGPS(path string) (*GPS, error) {
	var g GPS
	if err := readJSON(path, &g); err != nil {
		return nil, fmt.Errorf("parse gps %s: %w", path, err)
	}
	for i := range g.Points {
		t, err := parseLocalTime(g.Points[i].TRaw)
		if err != nil {
			return nil, fmt.Errorf("parse gps %s: points[%d].t: %w", path, i, err)
		}
		g.Points[i].T = t
	}
	return &g, nil
}

// Decimate downsamples points to roughly one point per interval, always
// keeping the first point and then the next point whose timestamp is at
// least interval past the last kept point.
func Decimate(points []GPSPoint, interval time.Duration) []GPSPoint {
	if len(points) == 0 {
		return nil
	}
	out := make([]GPSPoint, 0, len(points))
	out = append(out, points[0])
	last := points[0].T
	for _, p := range points[1:] {
		if p.T.Sub(last) >= interval {
			out = append(out, p)
			last = p.T
		}
	}
	return out
}

// ---- junk / path helpers ----

// junkNames are exact filenames always skipped during a scan.
var junkNames = map[string]bool{
	".DS_Store":       true,
	".Spotlight-V100": true,
}

// IsJunk reports whether name (a base filename) should be skipped during a
// scan: macOS AppleDouble files (._*), .DS_Store, and .Spotlight-V100.
func IsJunk(name string) bool {
	if len(name) >= 2 && name[0] == '.' && name[1] == '_' {
		return true
	}
	return junkNames[name]
}

// segmentPathRe matches the recorder's relative segment path:
// {front|rear}/YYYY/MM/DD/recording_YYYYMMDD_HHMMSS/segment_NNNNN.mp4
var segmentPathRe = regexp.MustCompile(
	`^(front|rear)/(\d{4})/(\d{2})/(\d{2})/(recording_(\d{8})_(\d{6}))/segment_(\d+)\.mp4$`,
)

// SegmentPath is the decomposed form of a segment's relative path.
type SegmentPath struct {
	Camera       string
	Date         time.Time
	RecordingKey string
	Seq          int
}

// ParseSegmentPath parses rel, a path relative to the SSD/share root, of
// the form {front|rear}/YYYY/MM/DD/recording_YYYYMMDD_HHMMSS/segment_NNNNN.mp4.
func ParseSegmentPath(rel string) (SegmentPath, error) {
	rel = path.Clean(rel)
	m := segmentPathRe.FindStringSubmatch(rel)
	if m == nil {
		return SegmentPath{}, fmt.Errorf("parse segment path %q: does not match expected layout", rel)
	}
	camera := m[1]
	year, month, day := m[2], m[3], m[4]
	recordingKey := m[5]
	seq, err := strconv.Atoi(m[8])
	if err != nil {
		return SegmentPath{}, fmt.Errorf("parse segment path %q: seq: %w", rel, err)
	}
	date, err := time.ParseInLocation("2006-01-02", year+"-"+month+"-"+day, location())
	if err != nil {
		return SegmentPath{}, fmt.Errorf("parse segment path %q: date: %w", rel, err)
	}
	return SegmentPath{
		Camera:       camera,
		Date:         date,
		RecordingKey: recordingKey,
		Seq:          seq,
	}, nil
}

// ---- shared JSON reading ----

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}
