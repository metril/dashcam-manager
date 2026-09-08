package main

import (
	"bytes"
	"compress/gzip"
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
			Cursor: q.Get("cursor"),
			Limit:  defaultTripsLimit,
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
		parsed, err := scan.ParseDetectionsBytes(raw)
		if err != nil {
			writeInternalError(w, "parse detections", err)
			return
		}

		writeJSON(w, http.StatusOK, detectionsToView(parsed))
	}
}

// handleStatsGet serves GET /api/stats.
func handleStatsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := store.Stats(r.Context())
		if err != nil {
			writeInternalError(w, "stats", err)
			return
		}
		writeJSON(w, http.StatusOK, stats)
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

func gunzip(gz []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
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
}

type detPlateView struct {
	Text        string   `json:"text"`
	Confidence  float64  `json:"confidence"`
	BBox        bboxView `json:"bbox"`
	VehicleBBox bboxView `json:"vehicle_bbox"`
}

type detVehicleView struct {
	Type           string   `json:"type"`
	TypeConfidence float64  `json:"type_confidence"`
	Make           string   `json:"make,omitempty"`
	MakeConfidence float64  `json:"make_confidence,omitempty"`
	BBox           bboxView `json:"bbox"`
}

type detFrameView struct {
	Timestamp  float64          `json:"timestamp"`
	WallTime   string           `json:"wall_time"`
	FrameIndex int              `json:"frame_index"`
	PTSOffset  float64          `json:"pts_offset"`
	Objects    []detObjectView  `json:"objects"`
	Plates     []detPlateView   `json:"plates,omitempty"`
	Vehicles   []detVehicleView `json:"vehicles,omitempty"`
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
	}
	for _, f := range d.DetectionFrames {
		fv := detFrameView{
			Timestamp:  f.Timestamp,
			WallTime:   f.WallTime,
			FrameIndex: f.FrameIndex,
			PTSOffset:  f.PTSOffset,
			GPS:        f.GPS,
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
