package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/scan"
)

// dateLayout is the YYYY-MM-DD format used by ?date=, ?from= and ?to=
// query parameters across the browse endpoints.
const dateLayout = "2006-01-02"

// defaultCalendarSpanDays caps how far back a calendar query with only one
// of from/to (or neither) reaches, so a client can't force an unbounded
// full-table scan by accident.
const defaultCalendarSpanDays = 366

// defaultTripsLimit / maxTripsLimit bound GET /api/trips paging.
const defaultTripsLimit = 50
const maxTripsLimit = 200

// handleCalendarGet serves GET /api/calendar?from=YYYY-MM-DD&to=YYYY-MM-DD,
// returning one entry per day in range that has at least one trip.
func handleCalendarGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, to, err := parseDateRange(q.Get("from"), q.Get("to"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		days, err := store.CalendarRange(r.Context(), from, to)
		if err != nil {
			writeInternalError(w, "calendar range", err)
			return
		}
		if days == nil {
			days = []db.CalendarDay{}
		}
		writeJSON(w, http.StatusOK, days)
	}
}

// handleTripsGet serves GET /api/trips?date=YYYY-MM-DD or
// ?from=...&to=...&cursor=...&limit=..., returning a page of trip cards.
func handleTripsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filter := db.TripsFilter{
			Limit: defaultTripsLimit,
		}

		if cursor := q.Get("cursor"); cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
				return
			}
			filter.Cursor = cursor
		}

		if date := q.Get("date"); date != "" {
			if _, err := time.Parse(dateLayout, date); err != nil {
				writeJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
				return
			}
			filter.Date = date
		} else if q.Get("from") != "" || q.Get("to") != "" {
			from, to, err := parseDateRange(q.Get("from"), q.Get("to"))
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			filter.From, filter.To = from, to
		}

		if limitStr := q.Get("limit"); limitStr != "" {
			n, err := strconv.Atoi(limitStr)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxTripsLimit {
				n = maxTripsLimit
			}
			filter.Limit = n
		}

		cards, nextCursor, err := store.TripsList(r.Context(), filter)
		if err != nil {
			writeInternalError(w, "trips list", err)
			return
		}
		if cards == nil {
			cards = []db.TripCard{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"trips":       cards,
			"next_cursor": nextCursor,
		})
	}
}

// handleTripGet serves GET /api/trips/{id}: the trip plus its segments.
func handleTripGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid trip id")
			return
		}
		trip, err := store.TripGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "trip not found")
			return
		}
		if err != nil {
			writeInternalError(w, "trip get", err)
			return
		}
		if trip.Segments == nil {
			trip.Segments = []db.TripSegment{}
		}
		writeJSON(w, http.StatusOK, trip)
	}
}

// handleSegmentGet serves GET /api/segments/{id}: segment metadata.
func handleSegmentGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}
		seg, err := store.SegmentGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "segment not found")
			return
		}
		if err != nil {
			writeInternalError(w, "segment get", err)
			return
		}
		writeJSON(w, http.StatusOK, seg)
	}
}

// handleSegmentDetectionsGet serves GET /api/segments/{id}/detections: the
// segment's raw detections, gunzipped and re-marshaled with bounding boxes
// normalized to 0..1 (detections_raw stores whatever bbox space the sidecar
// used; scan.ParseDetectionsBytes computes the normalized Norm fields we
// serve here instead of the raw pixel BBox).
func handleSegmentDetectionsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}

		gz, err := store.SegmentDetectionsRaw(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "no detections for segment")
			return
		}
		if err != nil {
			writeInternalError(w, "segment detections raw", err)
			return
		}

		raw, err := gunzip(gz)
		if err != nil {
			writeInternalError(w, "gunzip detections", err)
			return
		}
		if len(raw) > maxDecompressedDetectionsBytes {
			writeJSONError(w, http.StatusInternalServerError, "detections payload too large after decompression")
			return
		}
		parsed, err := scan.ParseDetectionsBytes(raw)
		if err != nil {
			writeInternalError(w, "parse detections", err)
			return
		}

		view := detectionsToView(parsed)
		tracks, err := store.TracksForSegment(r.Context(), id)
		if err != nil {
			writeInternalError(w, "tracks for segment", err)
			return
		}
		view.Tracks = tracks

		objIDs, plateIDs, vehIDs, err := store.DetTrackIDsForSegment(r.Context(), id)
		if err != nil {
			writeInternalError(w, "det track ids for segment", err)
			return
		}
		zipDetTrackIDs(view.Detections, objIDs, plateIDs, vehIDs)

		writeJSON(w, http.StatusOK, view)
	}
}

// handleStatsGet serves GET /api/stats.
// statsView wraps db.Stats with the thumb_hw_stats setting (see
// accumulateThumbHWStats in jobs_maintenance.go) as its "thumbs" field —
// that data lives in settings, not the aggregate queries db.Stats runs, so
// it's merged in here rather than inside the db package.
type statsView struct {
	db.Stats
	Thumbs thumbsStatsView `json:"thumbs"`
}

type thumbsStatsView struct {
	Mode        string `json:"mode"`
	HWOk        int64  `json:"hw_ok"`
	HWFallbacks int64  `json:"hw_fallbacks"`
	Unreadable  int64  `json:"unreadable"`
	LastRun     string `json:"last_run,omitempty"`
}

func handleStatsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := store.Stats(r.Context())
		if err != nil {
			writeInternalError(w, "stats", err)
			return
		}

		view := statsView{Stats: stats}
		if raw, err := store.SettingsGet(r.Context(), thumbHWStatsSettingsKey); err == nil {
			var hw thumbHWStats
			if err := json.Unmarshal(raw, &hw); err == nil {
				view.Thumbs = thumbsStatsView{
					Mode: hw.LastMode, HWOk: hw.HWOk, HWFallbacks: hw.HWFallbacks,
					Unreadable: hw.Unreadable, LastRun: hw.LastRunAt,
				}
			}
		}

		writeJSON(w, http.StatusOK, view)
	}
}

// parseDateRange validates and defaults a from/to pair of YYYY-MM-DD query
// params: a missing "to" defaults to today (UTC), a missing "from" defaults
// to defaultCalendarSpanDays before "to". Both, if present, must parse as
// dateLayout, and from must not be after to.
func parseDateRange(fromStr, toStr string) (string, string, error) {
	to := time.Now().UTC()
	if toStr != "" {
		t, err := time.Parse(dateLayout, toStr)
		if err != nil {
			return "", "", errors.New("to must be YYYY-MM-DD")
		}
		to = t
	}
	from := to.AddDate(0, 0, -defaultCalendarSpanDays)
	if fromStr != "" {
		t, err := time.Parse(dateLayout, fromStr)
		if err != nil {
			return "", "", errors.New("from must be YYYY-MM-DD")
		}
		from = t
	}
	if from.After(to) {
		return "", "", errors.New("from must not be after to")
	}
	return from.Format(dateLayout), to.Format(dateLayout), nil
}

// maxDecompressedDetectionsBytes caps how much decompressed JSON gunzip
// will read out of a detections_raw blob, so a corrupt or maliciously
// crafted gzip entry can't be used to exhaust memory via decompression
// (io.ReadAll on an unbounded gzip.Reader has no such limit on its own).
const maxDecompressedDetectionsBytes = 8 * 1024 * 1024

func gunzip(gz []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, maxDecompressedDetectionsBytes+1))
}

// --- detections view: re-shapes scan.Detections into normalized-bbox JSON ---

type bboxView [4]float64

func normBBox(n scan.NormBox) bboxView {
	return bboxView{n.X1, n.Y1, n.X2, n.Y2}
}

type detObjectView struct {
	Class      string   `json:"class"`
	Confidence float64  `json:"confidence"`
	BBox       bboxView `json:"bbox"`
	// TrackID is the det_tracks row (see TrackRow) this box was assigned
	// to by the IoU tracker, zipped in from det_objects.track_id after
	// detectionsToView runs (see zipDetTrackIDs) — nil (serves as JSON
	// null, not omitted: no `omitempty`) until scan.IndexDetections has
	// run for this segment, or for a box BuildTracks dropped as noise.
	TrackID *int64 `json:"track_id"`
}

type detPlateView struct {
	Text        string   `json:"text"`
	Confidence  float64  `json:"confidence"`
	BBox        bboxView `json:"bbox"`
	VehicleBBox bboxView `json:"vehicle_bbox"`
	// TrackID: see detObjectView.TrackID's comment above.
	TrackID *int64 `json:"track_id"`
}

type detVehicleView struct {
	Type           string   `json:"type"`
	TypeConfidence float64  `json:"type_confidence"`
	Make           string   `json:"make,omitempty"`
	MakeConfidence float64  `json:"make_confidence,omitempty"`
	BBox           bboxView `json:"bbox"`
	// TrackID: see detObjectView.TrackID's comment above.
	TrackID *int64 `json:"track_id"`
}

type detFrameView struct {
	Timestamp  float64          `json:"timestamp"`
	WallTime   string           `json:"wall_time"`
	FrameIndex int              `json:"frame_index"`
	PTSOffset  float64          `json:"pts_offset"`
	Objects    []detObjectView  `json:"objects"`
	Plates     []detPlateView   `json:"plates"`
	Vehicles   []detVehicleView `json:"vehicles"`
	GPS        *scan.FrameGPS   `json:"gps,omitempty"`
}

type detectionsView struct {
	Segment                 string         `json:"segment"`
	Camera                  string         `json:"camera"`
	FrameWidth              int            `json:"frame_width"`
	FrameHeight             int            `json:"frame_height"`
	FramesAnalyzed          int            `json:"frames_analyzed"`
	StartTime               string         `json:"start_time"`
	EndTime                 string         `json:"end_time"`
	TotalObjects            int            `json:"total_objects"`
	TotalPlates             int            `json:"total_plates"`
	TotalVehiclesClassified int            `json:"total_vehicles_classified"`
	Detections              []detFrameView `json:"detections"`
	// Tracks is segID's det_tracks rows, unfiltered — the frontend applies
	// its detection_filter setting live rather than the server
	// re-querying per filter change. Always a real (possibly empty)
	// slice, set by handleSegmentDetectionsGet after detectionsToView
	// runs (detectionsToView itself has no segment id / db handle to
	// look tracks up with).
	Tracks []db.TrackRow `json:"tracks"`
}

func detectionsToView(d *scan.Detections) detectionsView {
	out := detectionsView{
		Segment:                 d.Segment,
		Camera:                  d.Camera,
		FrameWidth:              d.FrameWidth,
		FrameHeight:             d.FrameHeight,
		FramesAnalyzed:          d.FramesAnalyzed,
		StartTime:               d.StartTime,
		EndTime:                 d.EndTime,
		TotalObjects:            d.TotalObjects,
		TotalPlates:             d.TotalPlates,
		TotalVehiclesClassified: d.TotalVehiclesClassified,
		// Always a real (possibly empty) slice, never nil: a detections
		// sidecar with zero analyzed frames should serialize as
		// "detections": [], not "detections": null.
		Detections: []detFrameView{},
		Tracks:     []db.TrackRow{},
	}
	for _, f := range d.DetectionFrames {
		fv := detFrameView{
			Timestamp:  f.Timestamp,
			WallTime:   f.WallTime,
			FrameIndex: f.FrameIndex,
			PTSOffset:  f.PTSOffset,
			GPS:        f.GPS,
			// None of Objects/Plates/Vehicles carry `omitempty` (every frame
			// always has these three lists, even when empty) so none of
			// them must ever come back as JSON null.
			Objects:  []detObjectView{},
			Plates:   []detPlateView{},
			Vehicles: []detVehicleView{},
		}
		for _, o := range f.Objects {
			fv.Objects = append(fv.Objects, detObjectView{Class: o.Class, Confidence: o.Confidence, BBox: normBBox(o.Norm)})
		}
		for _, p := range f.Plates {
			fv.Plates = append(fv.Plates, detPlateView{
				Text: p.Text, Confidence: p.Confidence,
				BBox: normBBox(p.Norm), VehicleBBox: normBBox(p.VehicleNorm),
			})
		}
		for _, v := range f.Vehicles {
			fv.Vehicles = append(fv.Vehicles, detVehicleView{
				Type: v.Type, TypeConfidence: v.TypeConfidence,
				Make: v.Make, MakeConfidence: v.MakeConfidence, BBox: normBBox(v.Norm),
			})
		}
		out.Detections = append(out.Detections, fv)
	}
	return out
}

// zipDetTrackIDs assigns db.DetTrackIDsForSegment's per-kind track_id
// slices onto frames' objects/plates/vehicles in order: both walk the same
// segment's boxes in identical (frame, in-frame index) order (see
// DetTrackIDsForSegment's doc comment), so the i-th object across all
// frames gets objIDs[i], and so on for plates/vehicles. Each kind is only
// zipped when its id slice's length exactly matches the sidecar-derived
// total count for that kind; otherwise the sidecar was re-scanned (or
// never indexed) without index_detections re-running, positional zipping
// would silently misassign ids, and the kind is left entirely nil.
func zipDetTrackIDs(frames []detFrameView, objIDs, plateIDs, vehIDs []*int64) {
	var objTotal, plateTotal, vehTotal int
	for f := range frames {
		objTotal += len(frames[f].Objects)
		plateTotal += len(frames[f].Plates)
		vehTotal += len(frames[f].Vehicles)
	}
	zipObjs := objTotal == len(objIDs)
	zipPlates := plateTotal == len(plateIDs)
	zipVehs := vehTotal == len(vehIDs)

	oi, pi, vi := 0, 0, 0
	for f := range frames {
		for o := range frames[f].Objects {
			if zipObjs {
				frames[f].Objects[o].TrackID = objIDs[oi]
			}
			oi++
		}
		for p := range frames[f].Plates {
			if zipPlates {
				frames[f].Plates[p].TrackID = plateIDs[pi]
			}
			pi++
		}
		for v := range frames[f].Vehicles {
			if zipVehs {
				frames[f].Vehicles[v].TrackID = vehIDs[vi]
			}
			vi++
		}
	}
}
