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

		job, err := q.Enqueue(r.Context(), "tracks_backfill", tracksBackfillJobParams{SegmentIDs: ids}, -1)
		if err != nil {
			writeInternalError(w, "tracks backfill: enqueue job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "count": len(ids), "job": job})
	}
}
