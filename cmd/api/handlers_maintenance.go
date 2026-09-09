package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/media"
)

// maxMaintenanceBodyBytes caps every maintenance POST body — all of them
// are small, fixed-shape objects (scope/id, or a couple of booleans),
// never a bulk upload.
const maxMaintenanceBodyBytes = 64 * 1024

// --- POST /api/maintenance/thumbs/regenerate --------------------------------

// thumbsRegenerateRequestBody is POST /api/maintenance/thumbs/regenerate's
// body. Scope "all" ignores id; "trip" and "segment" require it.
type thumbsRegenerateRequestBody struct {
	Scope string `json:"scope"`
	ID    string `json:"id,omitempty"`
}

// handleMaintenanceThumbsRegeneratePost serves POST
// /api/maintenance/thumbs/regenerate: deletes the cached thumbnail+sprite
// files for the requested scope, then enqueues a "thumbs" job (with
// force=true, so the DB's thumb_status='done' bookkeeping — now stale,
// since the files are gone — doesn't cause SegmentsForThumbs to skip them)
// limited to that same scope.
func handleMaintenanceThumbsRegeneratePost(store *db.DB, q *jobs.Queue, cacheDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxMaintenanceBodyBytes)

		var body thumbsRegenerateRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		var (
			params thumbsJobParams
			ids    []int64
		)
		switch body.Scope {
		case "all":
			if err := os.RemoveAll(filepath.Join(cacheDir, "thumbs")); err != nil {
				writeInternalError(w, "maintenance thumbs regenerate: remove thumbs cache", err)
				return
			}
			if err := os.RemoveAll(filepath.Join(cacheDir, "sprites")); err != nil {
				writeInternalError(w, "maintenance thumbs regenerate: remove sprites cache", err)
				return
			}
			// No SourceID/SegmentIDs: SegmentsForThumbs treats that as
			// "every segment" (see its doc comment).

		case "trip":
			tripID, err := strconv.ParseInt(body.ID, 10, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "id must be a trip id integer for scope=trip")
				return
			}
			ids, err = store.SegmentIDsForTrip(r.Context(), tripID)
			if err != nil {
				writeInternalError(w, "maintenance thumbs regenerate: trip segment ids", err)
				return
			}
			if len(ids) == 0 {
				writeJSONError(w, http.StatusNotFound, "trip not found or has no segments")
				return
			}
			removeSegmentThumbFiles(cacheDir, ids)
			params.SegmentIDs = ids

		case "segment":
			segID, err := strconv.ParseInt(body.ID, 10, 64)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "id must be a segment id integer for scope=segment")
				return
			}
			ids = []int64{segID}
			removeSegmentThumbFiles(cacheDir, ids)
			params.SegmentIDs = ids

		default:
			writeJSONError(w, http.StatusBadRequest, `scope must be "all", "trip", or "segment"`)
			return
		}

		params.Force = true
		job, err := q.Enqueue(r.Context(), "thumbs", params, 0)
		if err != nil {
			writeInternalError(w, "maintenance thumbs regenerate: enqueue thumbs job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// removeSegmentThumbFiles best-effort deletes the cached thumbnail and
// sprite (jpg+vtt) files for each id — a missing file is not an error
// (the point is just that nothing stale is left behind), but any other
// filesystem error is logged since it likely means the regenerate job's
// force-write will hit the same problem.
func removeSegmentThumbFiles(cacheDir string, ids []int64) {
	for _, id := range ids {
		paths := []string{media.ThumbPath(cacheDir, id)}
		jpg, vtt := media.SpritePaths(cacheDir, id)
		paths = append(paths, jpg, vtt)
		for _, p := range paths {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				log.Printf("[api] maintenance thumbs regenerate: remove %s: %v", p, err)
			}
		}
	}
}

// --- POST /api/maintenance/db/verify ----------------------------------------

// handleMaintenanceDBVerifyPost serves POST /api/maintenance/db/verify: it
// just enqueues a "db_verify" job (see dbVerifyJobHandler) and returns it —
// the actual checks can take real wall time (an integrity_check walks
// every page) so they run off the request path like every other
// maintenance/scan operation.
func handleMaintenanceDBVerifyPost(q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, err := q.Enqueue(r.Context(), "db_verify", struct{}{}, 0)
		if err != nil {
			writeInternalError(w, "maintenance db verify: enqueue", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// --- GET /api/maintenance/report --------------------------------------------

// handleMaintenanceReportGet serves GET /api/maintenance/report: the most
// recent db_verify job's report, persisted under settings key
// lastDBVerifySettingsKey. 404s if db_verify has never run.
func handleMaintenanceReportGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := store.SettingsGet(r.Context(), lastDBVerifySettingsKey)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "no db_verify report yet — run POST /api/maintenance/db/verify first")
			return
		}
		if err != nil {
			writeInternalError(w, "maintenance report", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}
}

// --- POST /api/maintenance/db/prune -----------------------------------------

// handleMaintenanceDBPrunePost serves POST /api/maintenance/db/prune: it
// enqueues a "db_prune" job with the request body's apply/vacuum/force
// flags decoded straight through as dbPruneJobParams. An absent or empty
// body decodes to the zero value (Apply: false), which maintenance.Prune
// always treats as a safe, non-destructive dry run — deletion requires
// the caller to explicitly pass {"apply": true}.
func handleMaintenanceDBPrunePost(q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxMaintenanceBodyBytes)

		var params dbPruneJobParams
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}

		job, err := q.Enqueue(r.Context(), "db_prune", params, 0)
		if err != nil {
			writeInternalError(w, "maintenance db prune: enqueue", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}
