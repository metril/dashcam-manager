package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// unreadableSegmentsView is the response shape for GET
// /api/segments/unreadable.
type unreadableSegmentsView struct {
	Segments   []db.UnreadableSegment `json:"segments"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// defaultUnreadableLimit matches defaultTripsLimit's page size for a list
// of similar shape/use (a paginated stats-page section).
const defaultUnreadableLimit = 50

// handleSegmentsUnreadableGet serves GET /api/segments/unreadable: a
// paginated list of segments whose media file was found undecodable (see
// db.SegmentMarkUnreadable), for the stats page's "Unreadable segments"
// section.
func handleSegmentsUnreadableGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := defaultUnreadableLimit
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			limit = n
		}
		cursor := q.Get("cursor")
		if cursor != "" {
			if n, err := strconv.Atoi(cursor); err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
				return
			}
		}

		segs, next, err := store.SegmentsUnreadable(r.Context(), limit, cursor)
		if err != nil {
			writeInternalError(w, "segments unreadable", err)
			return
		}
		writeJSON(w, http.StatusOK, unreadableSegmentsView{Segments: segs, NextCursor: next})
	}
}

// maxUnreadableRecheckBodyBytes caps POST /api/segments/unreadable/recheck
// request bodies — an optional, modest list of segment ids at most, never
// a bulk upload.
const maxUnreadableRecheckBodyBytes = 64 * 1024

// unreadableRecheckRequestBody is POST /api/segments/unreadable/recheck's
// body: an empty/omitted ids list means "recheck every currently
// unreadable segment", a non-empty list scopes the recheck to just those
// ids.
type unreadableRecheckRequestBody struct {
	IDs []int64 `json:"ids,omitempty"`
}

// handleSegmentsUnreadableRecheckPost serves POST
// /api/segments/unreadable/recheck: clears the unreadable flag (and
// thumb_status) on the requested segments — or every unreadable segment,
// if the body's ids list is empty — then enqueues a "thumbs" job for
// exactly those ids so they're given another real attempt (e.g. after an
// ffmpeg/driver upgrade, or because the classifier was wrong) instead of
// staying permanently skipped by SegmentsForThumbs.
func handleSegmentsUnreadableRecheckPost(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUnreadableRecheckBodyBytes)

		var body unreadableRecheckRequestBody
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}

		ids, err := store.SegmentIDsToRecheck(r.Context(), body.IDs)
		if err != nil {
			writeInternalError(w, "segments unreadable recheck: resolve ids", err)
			return
		}
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"status": "noop", "count": 0})
			return
		}

		if len(body.IDs) == 0 {
			if _, err := store.SegmentClearUnreadableAll(r.Context()); err != nil {
				writeInternalError(w, "segments unreadable recheck: clear all", err)
				return
			}
		} else if err := store.SegmentsClearUnreadableByIDs(r.Context(), ids); err != nil {
			writeInternalError(w, "segments unreadable recheck: clear", err)
			return
		}

		job, err := q.Enqueue(r.Context(), "thumbs", thumbsJobParams{SegmentIDs: ids}, 0)
		if err != nil {
			writeInternalError(w, "segments unreadable recheck: enqueue thumbs job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "count": len(ids), "job": job})
	}
}
