// Package scan parses the recorder's per-segment JSON sidecars
// (.timestamp.json, .detections.json, .gps.json) and the directory layout
// they live in, normalizing detection bounding boxes to 0..1 coordinates.
package scan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// timeLayout matches the recorder's local naive ISO timestamps, e.g.
// "2026-02-27T00:16:54.947".
const timeLayout = "2006-01-02T15:04:05.999999999"

// defaultFrameDim is the inference frame width/height assumed when a
// .detections.json sidecar omits frame_width/frame_height or gives a
// non-positive value, so a malformed sidecar doesn't silently zero every
// bbox instead of just falling back to the recorder's known default.
const defaultFrameDim = 640

// ErrEmptySidecar is returned (wrapped) by ParseTimestamp, ParseDetections,
// and ParseGPS when the sidecar file exists but is zero bytes — a real
// condition on the SSD (writer interrupted mid-flush) that scanners should
// treat as "no sidecar data" rather than a fatal parse error.
var ErrEmptySidecar = errors.New("empty sidecar file")

var (
	tzOnce sync.Once
	tzLoc  *time.Location
)

// location returns the time.Location used to interpret the recorder's naive
// local timestamps: the TZ environment variable if set and valid, else
// time.Local. Resolved once per process.
func location() *time.Location {
	tzOnce.Do(func() {
		tzLoc = time.Local
		if tz := os.Getenv("TZ"); tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				tzLoc = loc
			}
		}
	})
	return tzLoc
}

// parseLocalTime parses a naive ISO timestamp string (no timezone offset)
// using the configured location.
func parseLocalTime(s string) (time.Time, error) {
	return time.ParseInLocation(timeLayout, s, location())
}

// parseOptionalLocalTime is like parseLocalTime but tolerates an empty
// string, returning the zero time.Time with no error — for sidecar fields
// that are sometimes omitted rather than always present.
func parseOptionalLocalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseLocalTime(s)
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

// Box4 is an integer bounding box [x1,y1,x2,y2] as emitted by the recorder.
// Unmarshaling rejects JSON arrays with fewer than 4 elements instead of
// silently zero-padding a truncated bbox.
type Box4 [4]int

// UnmarshalJSON implements json.Unmarshaler, rejecting bbox arrays shorter
// than 4 elements.
func (b *Box4) UnmarshalJSON(data []byte) error {
	var raw []int
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) < 4 {
		return fmt.Errorf("bbox: want at least 4 elements, got %d: %v", len(raw), raw)
	}
	copy(b[:], raw[:4])
	return nil
}

// Object is one classified detection within a frame.
type Object struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
	BBox       Box4    `json:"bbox"`
	// Norm is computed by ParseDetections from BBox and the sidecar's
	// frame_width/frame_height; it is not present in the raw JSON.
	Norm NormBox `json:"-"`
}

// Plate is a license-plate detection, optionally linked to the vehicle it
// was read from.
type Plate struct {
	Text        string  `json:"text"`
	Confidence  float64 `json:"confidence"`
	BBox        Box4    `json:"bbox"`
	VehicleBBox Box4    `json:"vehicle_bbox"`
	// Norm and VehicleNorm are computed by ParseDetections from BBox and
	// VehicleBBox respectively, using the sidecar's
	// frame_width/frame_height; neither is present in the raw JSON.
	Norm        NormBox `json:"-"`
	VehicleNorm NormBox `json:"-"`
}

// Vehicle is a make/type classification for a detected vehicle.
type Vehicle struct {
	Type           string  `json:"type"`
	TypeConfidence float64 `json:"type_confidence"`
	Make           string  `json:"make,omitempty"`
	MakeConfidence float64 `json:"make_confidence,omitempty"`
	BBox           Box4    `json:"bbox"`
	// Norm is computed by ParseDetections from BBox and the sidecar's
	// frame_width/frame_height; it is not present in the raw JSON.
	Norm NormBox `json:"-"`
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
	// WallTimeT is WallTime parsed by ParseDetections; zero if WallTime was
	// empty. Not present in the raw JSON.
	WallTimeT time.Time `json:"-"`
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
	// StartTimeT and EndTimeT are StartTime/EndTime parsed by
	// ParseDetections; zero if the corresponding raw field was empty. Not
	// present in the raw JSON.
	StartTimeT time.Time `json:"-"`
	EndTimeT   time.Time `json:"-"`
}

// ParseDetections reads and parses a .detections.json sidecar at path.
// It populates Object.Norm, Plate.Norm/VehicleNorm, and Vehicle.Norm on
// every detection from the sidecar's frame_width/frame_height, defaulting
// to defaultFrameDim (640) when either is missing or non-positive so a
// malformed sidecar doesn't silently zero every bbox. It also populates
// StartTimeT, EndTimeT, and each frame's WallTimeT, tolerating empty raw
// time strings.
func ParseDetections(path string) (*Detections, error) {
	var d Detections
	if err := readJSON(path, &d); err != nil {
		return nil, fmt.Errorf("parse detections %s: %w", path, err)
	}
	out, err := normalizeDetections(&d)
	if err != nil {
		return nil, fmt.Errorf("parse detections %s: %w", path, err)
	}
	return out, nil
}

// ParseDetectionsBytes parses raw .detections.json bytes (e.g. read back
// from a gzipped detections_raw blob rather than a file on disk) and
// normalizes it exactly as ParseDetections does.
func ParseDetectionsBytes(data []byte) (*Detections, error) {
	if len(data) == 0 {
		return nil, ErrEmptySidecar
	}
	var d Detections
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse detections bytes: %w", err)
	}
	return normalizeDetections(&d)
}

// normalizeDetections populates Object.Norm, Plate.Norm/VehicleNorm,
// Vehicle.Norm, StartTimeT/EndTimeT and each frame's WallTimeT on an
// already-decoded Detections value.
func normalizeDetections(d *Detections) (*Detections, error) {
	frameW, frameH := d.FrameWidth, d.FrameHeight
	if frameW <= 0 {
		frameW = defaultFrameDim
	}
	if frameH <= 0 {
		frameH = defaultFrameDim
	}

	startT, err := parseOptionalLocalTime(d.StartTime)
	if err != nil {
		return nil, fmt.Errorf("start_time: %w", err)
	}
	d.StartTimeT = startT
	endT, err := parseOptionalLocalTime(d.EndTime)
	if err != nil {
		return nil, fmt.Errorf("end_time: %w", err)
	}
	d.EndTimeT = endT

	for fi := range d.DetectionFrames {
		frame := &d.DetectionFrames[fi]
		wallT, err := parseOptionalLocalTime(frame.WallTime)
		if err != nil {
			return nil, fmt.Errorf("detections[%d].wall_time: %w", fi, err)
		}
		frame.WallTimeT = wallT

		for oi := range frame.Objects {
			frame.Objects[oi].Norm = NormalizeBBox(frame.Objects[oi].BBox, frameW, frameH)
		}
		for pi := range frame.Plates {
			frame.Plates[pi].Norm = NormalizeBBox(frame.Plates[pi].BBox, frameW, frameH)
			frame.Plates[pi].VehicleNorm = NormalizeBBox(frame.Plates[pi].VehicleBBox, frameW, frameH)
		}
		for vi := range frame.Vehicles {
			frame.Vehicles[vi].Norm = NormalizeBBox(frame.Vehicles[vi].BBox, frameW, frameH)
		}
	}
	return d, nil
}

// detectionClasses are the object classes counted by Summary, in the
// canonical order used to break ties in TopClasses.
var detectionClasses = []string{"car", "truck", "bus", "person", "bicycle", "motorcycle"}

// DetectionSummary is a per-class rollup of a Detections sidecar.
type DetectionSummary struct {
	// ClassCounts holds every class string seen in the sidecar's objects,
	// not just the canonical six (car, truck, bus, person, bicycle,
	// motorcycle) — the model can in principle emit other labels, and
	// those are not silently dropped.
	ClassCounts map[string]int
	PlateCount  int
	// TopClasses lists every class in ClassCounts (a superset of the
	// canonical six when the sidecar contains other labels), ordered by
	// count descending.
	TopClasses []string
}

// Summary computes per-class detection counts, the total plate count, and
// classes ordered by count descending (ties broken by the canonical class
// order car/truck/bus/person/bicycle/motorcycle, then alphabetically for
// any class outside it). See DetectionSummary for why ClassCounts/
// TopClasses are a superset of those six classes.
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
	// StartTimeT and EndTimeT are StartTime/EndTime parsed by ParseGPS;
	// zero if the corresponding raw field was empty. Not present in the
	// raw JSON.
	StartTimeT time.Time `json:"-"`
	EndTimeT   time.Time `json:"-"`
}

// ParseGPS reads and parses a .gps.json sidecar at path, populating
// GPSPoint.T on every point and StartTimeT/EndTimeT (tolerating empty raw
// start_time/end_time).
func ParseGPS(path string) (*GPS, error) {
	var g GPS
	if err := readJSON(path, &g); err != nil {
		return nil, fmt.Errorf("parse gps %s: %w", path, err)
	}
	startT, err := parseOptionalLocalTime(g.StartTime)
	if err != nil {
		return nil, fmt.Errorf("parse gps %s: start_time: %w", path, err)
	}
	g.StartTimeT = startT
	endT, err := parseOptionalLocalTime(g.EndTime)
	if err != nil {
		return nil, fmt.Errorf("parse gps %s: end_time: %w", path, err)
	}
	g.EndTimeT = endT

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

// readJSON opens path and JSON-decodes it into v. A zero-byte file (a real
// condition on the SSD when the recorder is interrupted mid-write) decodes
// as io.EOF from the very first Decode call; that case is mapped to
// ErrEmptySidecar so callers/scanners can distinguish "no data" from a
// genuinely malformed sidecar.
func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return ErrEmptySidecar
		}
		return err
	}
	return nil
}
