package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Path                string                 `json:"path"`
	Present             bool                   `json:"present"`
	MountedRW           bool                   `json:"mounted_rw"`
	TotalBytes          *uint64                `json:"total_bytes,omitempty"`
	FreeBytes           *uint64                `json:"free_bytes,omitempty"`
	SegmentsTotal       int                    `json:"segments_total"`
	SegmentsIngested    int                    `json:"segments_ingested"`
	SegmentsNotIngested int                    `json:"segments_not_ingested"`
	SourceID            *int64                 `json:"source_id,omitempty"`
	Trips               []db.IngestTripSummary `json:"trips,omitempty"`
}

// findSSDSource returns the seeded "ssd"-kind source, or (nil, nil) if
// none is configured yet (a fresh install before SourceEnsure has run, or
// a DB with the SSD source deleted) — not itself an error, since callers
// decide differently how to react (a 200 with present:false for the
// status GET; a 400 for an endpoint that actually needs one to act on).
func findSSDSource(ctx context.Context, store *db.DB) (*db.Source, error) {
	sources, err := store.SourcesList(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sources {
		if sources[i].Kind == "ssd" {
			return &sources[i], nil
		}
	}
	return nil, nil
}

// handleSSDStatusGet reports the SSD source's presence, read/write state,
// free space, and how many of its indexed segments have been ingested
// anywhere. An optional ?target_share_id= additionally attaches per-trip
// ingest progress against that specific share, for the Ingest page's trip
// table (rather than a separate endpoint — the brief's API surface lists
// only this one GET plus the two POSTs).
func handleSSDStatusGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ssd, err := findSSDSource(r.Context(), store)
		if err != nil {
			writeInternalError(w, "ssd status: sources list", err)
			return
		}
		if ssd == nil {
			writeJSON(w, http.StatusOK, ssdStatusView{Present: false})
			return
		}

		var targetShareID int64
		if raw := r.URL.Query().Get("target_share_id"); raw != "" {
			id, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || id <= 0 {
				writeJSONError(w, http.StatusBadRequest, "target_share_id must be a positive integer")
				return
			}
			targetShareID = id
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
		view.SegmentsNotIngested = stats.SegmentsNotIngested

		if targetShareID > 0 {
			trips, tErr := store.IngestTripSummariesGet(r.Context(), ssd.ID, targetShareID)
			if tErr != nil {
				writeInternalError(w, "ssd status: trip summaries", tErr)
				return
			}
			view.Trips = trips
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

		// A target_share_id that doesn't name any share at all is a bad
		// request (400); one that names a real share which just isn't
		// currently mounted/reachable is a 502 (the request was valid, the
		// destination isn't ready) — checked separately so the two cases
		// don't collapse into the same status code.
		if _, err := store.ShareGet(r.Context(), body.TargetShareID); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusBadRequest, "target_share_id: share not found")
			return
		} else if err != nil {
			writeInternalError(w, "ingest: target share get", err)
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
	// SourceID is optional; when omitted, defaults to the seeded
	// "ssd"-kind source (the normal case — cleanup is almost always run
	// against the SSD).
	SourceID int64   `json:"source_id,omitempty"`
	TripIDs  []int64 `json:"trip_ids,omitempty"`
	DryRun   bool    `json:"dry_run,omitempty"`
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

		sourceID, notFound, err := resolveCleanupSourceID(r.Context(), store, body.SourceID)
		if notFound {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			writeInternalError(w, "ingest cleanup: resolve source", err)
			return
		}

		if body.DryRun {
			stats, err := ingest.PreviewFor(r.Context(), store, sourceID, body.TripIDs)
			if err != nil {
				writeInternalError(w, "ingest cleanup preview", err)
				return
			}
			writeJSON(w, http.StatusOK, stats)
			return
		}

		job, err := q.Enqueue(r.Context(), "cleanup", cleanupJobParams{SourceID: sourceID, TripIDs: body.TripIDs}, 0)
		if err != nil {
			writeInternalError(w, "enqueue cleanup job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// resolveCleanupSourceID returns requested if non-zero (after confirming
// it actually exists), or the seeded "ssd"-kind source's id if requested
// is zero — the default per the M10 review findings ("API cleanup
// requires source_id (default: the ssd source)"). notFound is true iff
// the returned error is a client-fixable 400 (an explicit source_id that
// doesn't exist, or no ssd source configured to default to) rather than
// an unexpected DB failure.
func resolveCleanupSourceID(ctx context.Context, store *db.DB, requested int64) (id int64, notFound bool, err error) {
	if requested != 0 {
		if _, err := store.SourceGet(ctx, requested); errors.Is(err, db.ErrNotFound) {
			return 0, true, fmt.Errorf("source_id: source not found")
		} else if err != nil {
			return 0, false, err
		}
		return requested, false, nil
	}

	ssd, err := findSSDSource(ctx, store)
	if err != nil {
		return 0, false, err
	}
	if ssd == nil {
		return 0, true, fmt.Errorf("source_id is required (no ssd source is configured)")
	}
	return ssd.ID, false, nil
}
