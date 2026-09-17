// handlers_vehicle.go serves the OBD vehicle-telemetry read API (Task 6):
// GET /api/segments/{id}/obd, GET /api/trips/{id}/vehicle, and the
// GET /api/vehicle/{events,trips,status,dtcs,trends} collection endpoints.
// It's a thin HTTP layer over internal/db/queries_vehicle.go's Task 6 read
// models — no rollup logic lives here. handleVehicleStatusGet follows
// handleDashcamStatus's degrade-to-200 pattern (an unreachable/misconfigured
// recorder is never a 500).
package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
)

// defaultVehicleListLimit/maxVehicleListLimit bound GET /api/vehicle/events
// and GET /api/vehicle/trips paging.
const defaultVehicleListLimit = 100
const maxVehicleListLimit = 500

// defaultVehicleTrendsDays/maxVehicleTrendsDays bound GET
// /api/vehicle/trends's ?days= window.
const defaultVehicleTrendsDays = 30
const maxVehicleTrendsDays = 365

// parseVehicleBound parses a ?from=/?to= value as either a bare ISO date
// (YYYY-MM-DD) or a full ISO datetime, returning a string that compares
// correctly (lexicographically) against the RFC3339 wall_time/start_time
// text columns it's filtered against — the same trick TripsList's own
// day-range bounds rely on. A bare date used as an exclusive upper bound
// (endExclusive) is advanced one day, matching dayRange's half-open
// [start, end) convention; used as a lower bound it's returned as-is.
func parseVehicleBound(s string, endExclusive bool) (string, error) {
	if t, err := time.Parse(dateLayout, s); err == nil {
		if endExclusive {
			t = t.AddDate(0, 0, 1)
		}
		return t.Format(dateLayout), nil
	}
	if _, err := time.Parse(time.RFC3339, s); err == nil {
		return s, nil
	}
	return "", errors.New("must be an ISO date (YYYY-MM-DD) or datetime")
}

// --- GET /api/segments/{id}/obd ----------------------------------------------

func handleSegmentOBDGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}
		obd, err := store.SegmentOBDGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "segment has no OBD data")
			return
		}
		if err != nil {
			writeInternalError(w, "segment obd get", err)
			return
		}
		writeJSON(w, http.StatusOK, obd)
	}
}

// --- GET /api/trips/{id}/vehicle ---------------------------------------------

func handleTripVehicleGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid trip id")
			return
		}
		resp, err := store.TripVehicleGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "trip not found")
			return
		}
		if err != nil {
			writeInternalError(w, "trip vehicle get", err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- GET /api/vehicle/events --------------------------------------------------

func handleVehicleEventsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filter := db.VehicleEventsFilter{Limit: defaultVehicleListLimit}

		if ty := q.Get("type"); ty != "" {
			filter.Types = strings.Split(ty, ",")
		}
		if from := q.Get("from"); from != "" {
			b, err := parseVehicleBound(from, false)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "from "+err.Error())
				return
			}
			filter.From = b
		}
		if to := q.Get("to"); to != "" {
			b, err := parseVehicleBound(to, true)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "to "+err.Error())
				return
			}
			filter.To = b
		}
		if tripStr := q.Get("trip"); tripStr != "" {
			tid, err := strconv.ParseInt(tripStr, 10, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "trip must be an integer")
				return
			}
			filter.TripID = tid
		}
		if cursor := q.Get("cursor"); cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
				return
			}
			filter.Cursor = cursor
		}
		if limitStr := q.Get("limit"); limitStr != "" {
			n, err := strconv.Atoi(limitStr)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxVehicleListLimit {
				n = maxVehicleListLimit
			}
			filter.Limit = n
		}

		events, nextCursor, err := store.VehicleEventsList(r.Context(), filter)
		if err != nil {
			writeInternalError(w, "vehicle events list", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"events":      events,
			"next_cursor": nextCursor,
		})
	}
}

// --- GET /api/vehicle/trips ---------------------------------------------------

func handleVehicleTripsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filter := db.VehicleTripsFilter{Limit: defaultVehicleListLimit}

		if from := q.Get("from"); from != "" {
			b, err := parseVehicleBound(from, false)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "from "+err.Error())
				return
			}
			filter.From = b
		}
		if to := q.Get("to"); to != "" {
			b, err := parseVehicleBound(to, true)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "to "+err.Error())
				return
			}
			filter.To = b
		}
		if cursor := q.Get("cursor"); cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
				return
			}
			filter.Cursor = cursor
		}
		if limitStr := q.Get("limit"); limitStr != "" {
			n, err := strconv.Atoi(limitStr)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxVehicleListLimit {
				n = maxVehicleListLimit
			}
			filter.Limit = n
		}

		trips, nextCursor, err := store.VehicleTripsList(r.Context(), filter)
		if err != nil {
			writeInternalError(w, "vehicle trips list", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"trips":       trips,
			"next_cursor": nextCursor,
		})
	}
}

// --- GET /api/vehicle/status --------------------------------------------------

// handleVehicleStatusGet proxies the recorder's live OBD status for the
// first configured dashcam source, degrading to a 200 {error_code,message}
// body (never a 500) for every expected failure: no dashcam configured, no
// signing key configured, the recorder unreachable, or a 404 from an older
// recorder that predates the vehicle-telemetry routes (error_code
// "unsupported_firmware").
func handleVehicleStatusGet(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dashcams, err := store.DashcamList(r.Context())
		if err != nil {
			writeInternalError(w, "dashcam list", err)
			return
		}
		if len(dashcams) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"error_code": "no_dashcam", "message": "no dashcam source configured",
			})
			return
		}

		_, client, err := dashcamClientFor(r.Context(), store, secretKey, dashcams[0].ID)
		if err != nil {
			code, ok := dashcamClientErrorCode(err)
			if !ok {
				writeInternalError(w, "dashcam client", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"error_code": code, "message": "dashcam signing key not configured",
			})
			return
		}

		raw, err := client.VehicleStatus(r.Context())
		if err != nil {
			code, message := "unreachable", err.Error()
			if dashcam.IsNotFound(err) {
				code, message = "unsupported_firmware", "recorder does not support vehicle telemetry"
			} else if ae, ok := asAPIError(err); ok {
				code = ae.Code
			}
			writeJSON(w, http.StatusOK, map[string]any{"error_code": code, "message": message})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}
}

// --- GET /api/vehicle/dtcs ----------------------------------------------------

func handleVehicleDTCsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dtcs, err := store.VehicleDTCsList(r.Context())
		if err != nil {
			writeInternalError(w, "vehicle dtcs list", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dtcs": dtcs})
	}
}

// --- GET /api/vehicle/trends ---------------------------------------------------

func handleVehicleTrendsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days := defaultVehicleTrendsDays
		if daysStr := r.URL.Query().Get("days"); daysStr != "" {
			n, err := strconv.Atoi(daysStr)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "days must be a positive integer")
				return
			}
			if n > maxVehicleTrendsDays {
				n = maxVehicleTrendsDays
			}
			days = n
		}
		trends, err := store.VehicleTrendsList(r.Context(), days)
		if err != nil {
			writeInternalError(w, "vehicle trends list", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"trips": trends})
	}
}
