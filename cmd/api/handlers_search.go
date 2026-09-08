package main

import (
	"net/http"
	"strconv"
	"time"

	"dashcam/internal/db"
)

// defaultSearchLimit / maxSearchLimit bound GET /api/search paging (same
// pattern as GET /api/trips in handlers_browse.go).
const defaultSearchLimit = 30
const maxSearchLimit = 100

// handleSearchGet serves GET /api/search?q=&class=&make=&type=&plate=&
// from=&to=&min_conf=&min_speed=&limit=&cursor=, returning results grouped
// by segment.
func handleSearchGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		filter := db.SearchFilter{
			Q:       q.Get("q"),
			Classes: q["class"], // repeated ?class=car&class=truck=... is a true OR multi-select
			Make:    q.Get("make"),
			Type:    q.Get("type"),
			Plate:   q.Get("plate"),
			Cursor:  q.Get("cursor"),
			Limit:   defaultSearchLimit,
		}

		if from := q.Get("from"); from != "" {
			if _, err := time.Parse(dateLayout, from); err != nil {
				writeJSONError(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
				return
			}
			filter.From = from
		}
		if to := q.Get("to"); to != "" {
			if _, err := time.Parse(dateLayout, to); err != nil {
				writeJSONError(w, http.StatusBadRequest, "to must be YYYY-MM-DD")
				return
			}
			filter.To = to
		}
		if v := q.Get("min_conf"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "min_conf must be a number")
				return
			}
			filter.MinConf = f
		}
		if v := q.Get("min_speed"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "min_speed must be a number")
				return
			}
			filter.MinSpeedMS = f
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxSearchLimit {
				n = maxSearchLimit
			}
			filter.Limit = n
		}

		if filter.Q == "" && len(filter.Classes) == 0 && filter.Make == "" && filter.Type == "" &&
			filter.Plate == "" && filter.MinConf == 0 && filter.MinSpeedMS == 0 {
			writeJSONError(w, http.StatusBadRequest, "at least one of q, class, make, type, plate, min_conf, min_speed is required")
			return
		}

		results, nextCursor, err := store.Search(r.Context(), filter)
		if err != nil {
			writeInternalError(w, "search", err)
			return
		}
		if results == nil {
			results = []db.SearchResult{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results":     results,
			"next_cursor": nextCursor,
		})
	}
}

// gpsTrackPoint is one point of the GET /api/gps/track GeoJSON-adjacent
// response.
type gpsTrackPoint struct {
	Lat      float64  `json:"lat"`
	Lon      float64  `json:"lon"`
	SpeedMps *float64 `json:"speed_mps"`
	T        string   `json:"t"`
}

// handleGPSTrackGet serves GET /api/gps/track?trip_id=, returning a GeoJSON
// LineString (coordinates as [lon, lat]) with a parallel "points" array
// (per-point speed/time) inside `properties` — GeoJSON has no first-class
// way to attach per-vertex properties to a LineString, so the client zips
// properties.points[i] against geometry.coordinates[i]; `points` must live
// under `properties` (not as a sibling of `type`/`geometry`) to keep the
// Feature object itself spec-valid GeoJSON.
func handleGPSTrackGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tripIDStr := r.URL.Query().Get("trip_id")
		tripID, err := strconv.ParseInt(tripIDStr, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "trip_id is required and must be an integer")
			return
		}

		pts, err := store.TripGPSTrack(r.Context(), tripID)
		if err != nil {
			writeInternalError(w, "trip gps track", err)
			return
		}

		coords := make([][2]float64, 0, len(pts))
		points := make([]gpsTrackPoint, 0, len(pts))
		for _, p := range pts {
			coords = append(coords, [2]float64{p.Lon, p.Lat})
			points = append(points, gpsTrackPoint{Lat: p.Lat, Lon: p.Lon, SpeedMps: p.SpeedMps, T: p.T})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"type": "Feature",
			"geometry": map[string]any{
				"type":        "LineString",
				"coordinates": coords,
			},
			"properties": map[string]any{
				"points": points,
			},
		})
	}
}
