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
	"time"

	"dashcam/internal/config"
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

// location returns the time.Location used to interpret the recorder's naive
// local timestamps: the timezone setting, else the TZ environment variable
// if set and valid, else time.Local (config.Location). Resolved fresh on
// every call (not sync.Once-cached — tests
// set TZ mid-suite via t.Setenv, and internal/db/queries_vehicle.go's
// vehicleLocalLocation, this function's sibling, hit the exact same
// stale-cache bug — see fix round 3 item 1). Not hot enough to warrant
// caching.
func location() *time.Location {
	return config.Location()
}

// parseLocalTime parses a naive ISO timestamp string (no timezone offset)
// using the configured location.
func parseLocalTime(s string) (time.Time, error) {
	return time.ParseInLocation(timeLayout, s, location())
}

// ParseLocalTime is the exported form of parseLocalTime, for callers outside
// this package that need to parse the recorder's naive local timestamp
// strings the same way sidecars do, so the resulting time.Time is directly
// comparable to segments' start_time/end_time (both produced by this same
// TZ resolution). Note: internal/db cannot import this package (internal/
// scan already imports internal/db, to write scan results), so
// internal/db/queries_vehicle.go deliberately duplicates this TZ
// resolution locally rather than calling this function — kept exported
// anyway for any future non-db caller that wants it.
func ParseLocalTime(s string) (time.Time, error) {
	return parseLocalTime(s)
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

	// Parse WallTimeT for every raw frame up front: mergeSamePTSFrames below
	// needs it (to keep the earliest wall_time when merging two frames that
	// share a pts_offset) before frames are renumbered.
	for fi := range d.DetectionFrames {
		frame := &d.DetectionFrames[fi]
		wallT, err := parseOptionalLocalTime(frame.WallTime)
		if err != nil {
			return nil, fmt.Errorf("detections[%d].wall_time: %w", fi, err)
		}
		frame.WallTimeT = wallT
	}

	d.DetectionFrames = mergeSamePTSFrames(d.DetectionFrames)
	dropEchoedEntries(d.DetectionFrames)

	for fi := range d.DetectionFrames {
		frame := &d.DetectionFrames[fi]
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

// mergeSamePTSFrames merges frames that share an identical PTSOffset —
// observed in the recorder's sidecars as one objects-only row and one
// vehicles-only row per analyzed timestamp (the InferenceThread and
// PlateThread results for that frame, flushed separately when their
// timestamps don't happen to collide before this merge; see
// docs/upstream/recorder-duplicate-vehicles.md). Objects/Plates/Vehicles
// are unioned (simple concatenation — the two rows describe disjoint parts
// of one frame, so there's nothing to deduplicate at this step), the
// earliest non-zero WallTimeT/WallTime of the group is kept, and the
// result is returned ordered by PTSOffset ascending with FrameIndex
// renumbered 0..N-1 to match. Frames are assumed to already have
// WallTimeT populated (see normalizeDetections).
func mergeSamePTSFrames(frames []DetectionFrame) []DetectionFrame {
	if len(frames) == 0 {
		return frames
	}
	order := make([]int, len(frames))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return frames[order[i]].PTSOffset < frames[order[j]].PTSOffset
	})

	merged := make([]DetectionFrame, 0, len(frames))
	i := 0
	for i < len(order) {
		base := frames[order[i]]
		j := i + 1
		for j < len(order) && frames[order[j]].PTSOffset == base.PTSOffset {
			other := frames[order[j]]
			base.Objects = append(base.Objects, other.Objects...)
			base.Plates = append(base.Plates, other.Plates...)
			base.Vehicles = append(base.Vehicles, other.Vehicles...)
			if base.GPS == nil && other.GPS != nil {
				base.GPS = other.GPS
			}
			if !other.WallTimeT.IsZero() && (base.WallTimeT.IsZero() || other.WallTimeT.Before(base.WallTimeT)) {
				base.WallTimeT = other.WallTimeT
				base.WallTime = other.WallTime
			}
			j++
		}
		merged = append(merged, base)
		i = j
	}
	for idx := range merged {
		merged[idx].FrameIndex = idx
	}
	return merged
}

// dropEchoedEntries removes, from each frame (after frame 0), any Vehicle
// or Plate entry that is field-for-field identical to one already present
// in the immediately preceding frame — the recorder's carry-forward bug
// (docs/upstream/recorder-duplicate-vehicles.md) echoes the previous
// frame's classification alongside the new one. frames must already be
// ordered by PTSOffset ascending (see mergeSamePTSFrames). Equality is
// exact (type/type_confidence/make/make_confidence/bbox for vehicles;
// text/confidence/bbox/vehicle_bbox for plates) — deliberately no IoU
// guesswork, since the echo is a byte-identical copy, not a near miss.
//
// Two safeguards against over-dropping a stationary subject across a run
// of frames:
//   - Every frame is compared against the ORIGINAL (pre-filter) previous
//     frame's list, snapshotted up front, never the already-filtered
//     current frame. Comparing against a mutated frames[i-1] would cascade:
//     a genuinely-repeated stationary vehicle present in frames 1..N would
//     have frame 1 dropped (echo of frame 0), then frame 2 would compare
//     against frame 1's now-EMPTY list and wrongly keep everything, or (with
//     an in-place update seen before frame 2 is processed) a real match in
//     frame 2 could itself be dropped if frame 1 had already been emptied
//     back to nothing to compare against — either way the result depends on
//     iteration order rather than each frame's own original contents.
//   - A frame's list is never dropped down to empty by this filter alone:
//     if every entry looks "echoed", that's a real stationary subject
//     persisting across frames (identical is the expected case for a
//     parked car sitting in front of the camera), not the recorder bug
//     (which pairs one echoed frame with a genuinely new one alongside it,
//     never composes the entire frame away). Reverting to the original list
//     keeps that subject visible in every frame instead of erasing it.
func dropEchoedEntries(frames []DetectionFrame) {
	origVehicles := make([][]Vehicle, len(frames))
	origPlates := make([][]Plate, len(frames))
	for i := range frames {
		origVehicles[i] = frames[i].Vehicles
		origPlates[i] = frames[i].Plates
	}

	for i := 1; i < len(frames); i++ {
		prevVehicles := origVehicles[i-1]
		if len(prevVehicles) > 0 && len(origVehicles[i]) > 0 {
			kept := make([]Vehicle, 0, len(origVehicles[i]))
			for _, v := range origVehicles[i] {
				if !vehicleEchoed(v, prevVehicles) {
					kept = append(kept, v)
				}
			}
			if len(kept) == 0 {
				kept = origVehicles[i]
			}
			frames[i].Vehicles = kept
		}

		prevPlates := origPlates[i-1]
		if len(prevPlates) > 0 && len(origPlates[i]) > 0 {
			kept := make([]Plate, 0, len(origPlates[i]))
			for _, p := range origPlates[i] {
				if !plateEchoed(p, prevPlates) {
					kept = append(kept, p)
				}
			}
			if len(kept) == 0 {
				kept = origPlates[i]
			}
			frames[i].Plates = kept
		}
	}
}

func vehicleEchoed(v Vehicle, prev []Vehicle) bool {
	for _, pv := range prev {
		if v.Type == pv.Type && v.TypeConfidence == pv.TypeConfidence &&
			v.Make == pv.Make && v.MakeConfidence == pv.MakeConfidence &&
			v.BBox == pv.BBox {
			return true
		}
	}
	return false
}

func plateEchoed(p Plate, prev []Plate) bool {
	for _, pp := range prev {
		if p.Text == pp.Text && p.Confidence == pp.Confidence &&
			p.BBox == pp.BBox && p.VehicleBBox == pp.VehicleBBox {
			return true
		}
	}
	return false
}

// DetectionClasses are the valid object classes emitted by the detection
// model, in the canonical order used to break ties in TopClasses (see
// Summary) and re-used by cmd/api as the whitelist for DetectionFilter's
// Classes field and exportRequest.OverlayFilter validation.
var DetectionClasses = []string{"car", "truck", "bus", "person", "bicycle", "motorcycle"}

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
	counts := make(map[string]int, len(DetectionClasses))
	plateCount := 0
	for _, frame := range d.DetectionFrames {
		for _, obj := range frame.Objects {
			counts[obj.Class]++
		}
		plateCount += len(frame.Plates)
	}

	classRank := make(map[string]int, len(DetectionClasses))
	for i, c := range DetectionClasses {
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

// ---- .obd.json ----

// OBDPoint is one OBD sample. Fields are pointers because the recorder
// emits JSON null (or omits the field) for a reading it couldn't get on a
// given sample.
type OBDPoint struct {
	TRaw        string   `json:"t"`
	SpeedKph    *float64 `json:"speed_kph"`
	RPM         *float64 `json:"rpm"`
	ThrottlePct *float64 `json:"throttle_pct"`
	AccelMps2   *float64 `json:"accel_mps2"`
	CoolantC    *float64 `json:"coolant_c"`
	BattV       *float64 `json:"batt_v"`
	// T is parsed by ParseOBD from TRaw; not present in the raw JSON.
	T time.Time `json:"-"`
}

// OBD is the parsed .obd.json sidecar.
type OBD struct {
	Segment     string     `json:"segment"`
	Camera      string     `json:"camera"`
	StartTime   string     `json:"start_time"`
	EndTime     string     `json:"end_time"`
	MaxSpeedKph float64    `json:"max_speed_kph"`
	AvgRPM      float64    `json:"avg_rpm"`
	DistanceM   float64    `json:"distance_m"`
	FuelUsedL   float64    `json:"fuel_used_l"`
	Points      []OBDPoint `json:"points"`
	// StartTimeT and EndTimeT are StartTime/EndTime parsed by ParseOBD,
	// zero if the corresponding raw field was empty. Not present in the
	// raw JSON.
	StartTimeT time.Time `json:"-"`
	EndTimeT   time.Time `json:"-"`
}

// ParseOBD reads and parses a .obd.json sidecar at path, populating
// OBDPoint.T on every point and StartTimeT/EndTimeT (tolerating empty raw
// start_time/end_time).
func ParseOBD(path string) (*OBD, error) {
	var o OBD
	if err := readJSON(path, &o); err != nil {
		return nil, fmt.Errorf("parse obd %s: %w", path, err)
	}
	startT, err := parseOptionalLocalTime(o.StartTime)
	if err != nil {
		return nil, fmt.Errorf("parse obd %s: start_time: %w", path, err)
	}
	o.StartTimeT = startT
	endT, err := parseOptionalLocalTime(o.EndTime)
	if err != nil {
		return nil, fmt.Errorf("parse obd %s: end_time: %w", path, err)
	}
	o.EndTimeT = endT

	for i := range o.Points {
		t, err := parseLocalTime(o.Points[i].TRaw)
		if err != nil {
			return nil, fmt.Errorf("parse obd %s: points[%d].t: %w", path, i, err)
		}
		o.Points[i].T = t
	}
	return &o, nil
}

// DecimateOBD downsamples points to roughly one point per interval, always
// keeping the first point and then the next point whose timestamp is at
// least interval past the last kept point. Mirrors Decimate (above), for
// .gps.json points.
func DecimateOBD(points []OBDPoint, interval time.Duration) []OBDPoint {
	if len(points) == 0 {
		return nil
	}
	out := make([]OBDPoint, 0, len(points))
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
