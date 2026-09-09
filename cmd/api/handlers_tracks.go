package main

import (
	"net/http"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// handleTracksBackfillPost serves POST /api/tracks/backfill: enqueues a
// "tracks_backfill" job (see cmd/api/jobs_runtime.go) that re-indexes every
// segment with detection boxes but no track_id assigned yet — the
// Settings/System-page trigger for backfilling det_tracks rows after
// upgrading to schema v5's IoU tracker. Needs no request body: the job
// itself resolves which segments need it via
// db.SegmentsNeedingTracksBackfill.
//
// The pre-check below is used only for the response's count and the
// zero-segments short-circuit — the enqueued job itself gets an empty
// SegmentIDs, so tracksBackfillJobHandler (see cmd/api/jobs_runtime.go)
// re-resolves the set itself when it actually runs, rather than working off
// a list resolved (and potentially stale, or huge in the request/response
// body) at enqueue time.
func handleTracksBackfillPost(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, err := store.SegmentsNeedingTracksBackfill(r.Context(), 0)
		if err != nil {
			writeInternalError(w, "tracks backfill: resolve segments", err)
			return
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"status": "noop", "count": 0})
			return
		}

		job, err := q.Enqueue(r.Context(), "tracks_backfill", tracksBackfillJobParams{}, -1)
		if err != nil {
			writeInternalError(w, "tracks backfill: enqueue job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "count": len(ids), "job": job})
	}
}
