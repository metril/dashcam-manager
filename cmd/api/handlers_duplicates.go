// handlers_duplicates.go serves GET /api/duplicates and POST
// /api/duplicates/refresh — the read/refresh endpoints over the
// duplicate_groups/duplicate_group_members report db.DuplicateReportRefresh
// (internal/db/queries_storage.go) rebuilds from the file_copies view.
package main

import (
	"net/http"
	"strconv"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// validDuplicateKinds mirrors db.DuplicateGroupKindSHA256/SizeName — an
// empty kind means "both", matching GET /api/catalog's own convention for
// an unfiltered listing elsewhere in this file's sibling handlers.
var validDuplicateKinds = map[string]bool{
	"":                            true,
	db.DuplicateGroupKindSHA256:   true,
	db.DuplicateGroupKindSizeName: true,
}

const (
	defaultDuplicatesLimit = 50
	maxDuplicatesLimit     = 200
)

// handleDuplicatesGet serves GET /api/duplicates?kind=&storage_id=&limit=&offset=:
// a page of duplicate_groups rows (each with its members), plus the total
// group count for that filter. kind must be "", "sha256", or "size_name";
// storage_id, when given, must be a share id (the cache pseudo-storage has
// no id to filter by — see db.DuplicateReportList's doc comment).
func handleDuplicatesGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		kind := q.Get("kind")
		if !validDuplicateKinds[kind] {
			writeJSONError(w, http.StatusBadRequest, `kind must be "sha256" or "size_name"`)
			return
		}

		var storageID int64
		if v := q.Get("storage_id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id <= 0 {
				writeJSONError(w, http.StatusBadRequest, "storage_id must be a positive integer")
				return
			}
			storageID = id
		}

		limit := defaultDuplicatesLimit
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxDuplicatesLimit {
				n = maxDuplicatesLimit
			}
			limit = n
		}

		offset := 0
		if v := q.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "offset must be a non-negative integer")
				return
			}
			offset = n
		}

		groups, total, err := store.DuplicateReportList(r.Context(), kind, storageID, limit, offset)
		if err != nil {
			writeInternalError(w, "duplicates list", err)
			return
		}
		if groups == nil {
			groups = []db.DuplicateGroup{}
		}
		totals, err := store.DuplicateReportTotalsGet(r.Context(), kind, storageID)
		if err != nil {
			writeInternalError(w, "duplicates totals", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"groups": groups,
			"total":  total,
			"limit":  limit,
			"offset": offset,
			"totals": totals,
		})
	}
}

// handleDuplicatesRefreshPost serves POST /api/duplicates/refresh:
// enqueues a "duplicates_refresh" job (cmd/api/jobs_duplicates.go) rather
// than rebuilding inline on the request goroutine — same pattern as
// POST /api/storages/hash enqueuing hash_backfill.
func handleDuplicatesRefreshPost(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, err := q.Enqueue(r.Context(), "duplicates_refresh", duplicatesRefreshJobParams{}, -1)
		if err != nil {
			writeInternalError(w, "enqueue duplicates_refresh job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}
