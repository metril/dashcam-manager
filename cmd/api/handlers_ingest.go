package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"

	"dashcam/internal/db"
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
	"dashcam/internal/share"
)

// maxIngestBodyBytes caps POST /api/ingest and /api/ingest/cleanup request
// bodies — both are small fixed-shape objects (at most a modest list of
// trip ids), never a bulk upload.
const maxIngestBodyBytes = 64 * 1024

// ssdStatusView is the response shape for GET /api/ssd/status.
type ssdStatusView struct {
	Path             string                 `json:"path"`
	Present          bool                   `json:"present"`
	MountedRW        bool                   `json:"mounted_rw"`
	TotalBytes       *uint64                `json:"total_bytes,omitempty"`
	FreeBytes        *uint64                `json:"free_bytes,omitempty"`
	SegmentsTotal    int                    `json:"segments_total"`
	SegmentsIngested int                    `json:"segments_ingested"`
	SegmentsUnindex  int                    `json:"segments_unindexed"`
	SourceID         *int64                 `json:"source_id,omitempty"`
	Trips            []db.IngestTripSummary `json:"trips,omitempty"`
}

// handleSSDStatusGet reports the SSD source's presence, read/write state,
// free space, and how many of its indexed segments have been ingested
// anywhere. An optional ?target_share_id= additionally attaches per-trip
// ingest progress against that specific share, for the Ingest page's trip
// table (rather than a separate endpoint — the brief's API surface lists
// only this one GET plus the two POSTs).
func handleSSDStatusGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := store.SourcesList(r.Context())
		if err != nil {
			writeInternalError(w, "ssd status: sources list", err)
			return
		}
		var ssd *db.Source
		for i := range sources {
			if sources[i].Kind == "ssd" {
				ssd = &sources[i]
				break
			}
		}
		if ssd == nil {
			writeJSON(w, http.StatusOK, ssdStatusView{Present: false})
			return
		}

		view := ssdStatusView{Path: ssd.RootPath, SourceID: &ssd.ID}

		if info, statErr := os.Stat(ssd.RootPath); statErr == nil && info.IsDir() {
			view.Present = true
			view.MountedRW = ingest.WriteProbe(ssd.RootPath) == nil
			if free, total, fsErr := share.FreeSpace(ssd.RootPath); fsErr == nil {
				view.FreeBytes = &free
				view.TotalBytes = &total
			}
		}

		stats, err := store.SSDIngestStatsGet(r.Context(), ssd.ID)
		if err != nil {
			writeInternalError(w, "ssd status: stats", err)
			return
		}
		view.SegmentsTotal = stats.SegmentsTotal
		view.SegmentsIngested = stats.SegmentsIngested
		view.SegmentsUnindex = stats.SegmentsUnindexed

		if raw := r.URL.Query().Get("target_share_id"); raw != "" {
			if targetShareID, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && targetShareID > 0 {
				trips, tErr := store.IngestTripSummariesGet(r.Context(), ssd.ID, targetShareID)
				if tErr != nil {
					writeInternalError(w, "ssd status: trip summaries", tErr)
					return
				}
				view.Trips = trips
			}
		}

		writeJSON(w, http.StatusOK, view)
	}
}

// ingestRequestBody is the JSON body for POST /api/ingest.
type ingestRequestBody struct {
	SourceID      int64   `json:"source_id"`
	TargetShareID int64   `json:"target_share_id"`
	TripIDs       []int64 `json:"trip_ids,omitempty"`
	Verify        string  `json:"verify,omitempty"`
	DeleteAfter   bool    `json:"delete_after,omitempty"`
}

// handleIngestPost validates the request (source exists, target share is
// actually mounted/reachable) and enqueues an "ingest" job — the copy
// itself runs asynchronously on the job queue (see jobs_ingest.go).
func handleIngestPost(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxIngestBodyBytes)

		var body ingestRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.SourceID == 0 || body.TargetShareID == 0 {
			writeJSONError(w, http.StatusBadRequest, "source_id and target_share_id are required")
			return
		}
		if body.Verify == "" {
			body.Verify = ingest.VerifySHA256
		}
		if body.Verify != ingest.VerifySHA256 && body.Verify != ingest.VerifySize {
			writeJSONError(w, http.StatusBadRequest, `verify must be "sha256" or "size"`)
			return
		}

		if _, err := store.SourceGet(r.Context(), body.SourceID); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "source not found")
			return
		} else if err != nil {
			writeInternalError(w, "ingest: source get", err)
			return
		}

		if _, err := ingest.ResolveTargetMount(r.Context(), store, body.TargetShareID); err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}

		job, err := q.Enqueue(r.Context(), "ingest", ingestJobParams{
			SourceID: body.SourceID, TargetShareID: body.TargetShareID,
			TripIDs: body.TripIDs, Verify: body.Verify, DeleteAfter: body.DeleteAfter,
		}, 0)
		if err != nil {
			writeInternalError(w, "enqueue ingest job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// ingestCleanupRequestBody is the JSON body for POST /api/ingest/cleanup.
type ingestCleanupRequestBody struct {
	TripIDs []int64 `json:"trip_ids,omitempty"`
	DryRun  bool    `json:"dry_run,omitempty"`
}

// handleIngestCleanupPost either runs a synchronous dry-run preview (fast
// and read-only — no need to round-trip through the job queue for
// something that never touches disk) or enqueues a "cleanup" job that
// actually deletes verified SSD originals.
func handleIngestCleanupPost(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxIngestBodyBytes)

		var body ingestCleanupRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		if body.DryRun {
			stats, err := ingest.PreviewFor(r.Context(), store, body.TripIDs)
			if err != nil {
				writeInternalError(w, "ingest cleanup preview", err)
				return
			}
			writeJSON(w, http.StatusOK, stats)
			return
		}

		job, err := q.Enqueue(r.Context(), "cleanup", cleanupJobParams{TripIDs: body.TripIDs}, 0)
		if err != nil {
			writeInternalError(w, "enqueue cleanup job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}
