package main

import (
	"context"
	"net/http"
	"strconv"

	"dashcam/internal/db"
)

// defaultCatalogLimit / maxCatalogLimit bound GET /api/catalog paging (same
// pattern as GET /api/search in handlers_search.go).
const defaultCatalogLimit = 50
const maxCatalogLimit = 200

var validCatalogKinds = map[string]bool{"make": true, "type": true, "plate": true, "class": true}

// handleCatalogGet serves GET /api/catalog?kind=make|type|plate|class&
// min_count=&q=&cursor=&limit=, returning distinct values for the given
// detection dimension with a count, first/last seen, and a sample segment
// for a thumbnail.
func handleCatalogGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		kind := q.Get("kind")
		if !validCatalogKinds[kind] {
			writeJSONError(w, http.StatusBadRequest, "kind must be one of make, type, plate, class")
			return
		}

		filter := db.CatalogFilter{
			Kind:   kind,
			Q:      q.Get("q"),
			Cursor: q.Get("cursor"),
			Limit:  defaultCatalogLimit,
		}

		if v := q.Get("min_count"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeJSONError(w, http.StatusBadRequest, "min_count must be a non-negative integer")
				return
			}
			filter.MinCount = n
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
				return
			}
			if n > maxCatalogLimit {
				n = maxCatalogLimit
			}
			filter.Limit = n
		}

		items, nextCursor, err := store.CatalogList(r.Context(), filter)
		if err != nil {
			writeInternalError(w, "catalog", err)
			return
		}
		if items == nil {
			items = []db.CatalogItem{}
		}
		if err := attachCatalogItemCopies(r.Context(), store, items); err != nil {
			writeInternalError(w, "catalog: attach copies", err)
			return
		}
		refreshedAt, err := store.CatalogRefreshedAt(r.Context())
		if err != nil {
			writeInternalError(w, "catalog", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":         kind,
			"items":        items,
			"next_cursor":  nextCursor,
			"refreshed_at": refreshedAt,
		})
	}
}

// attachCatalogItemCopies fills in Copies/OnSSD on every item's sample
// segment with one batched SegmentCopies + one batched SegmentsOnSSD call
// across the whole page (task 9's "no N+1" requirement), deduping repeated
// sample segment ids first.
func attachCatalogItemCopies(ctx context.Context, store *db.DB, items []db.CatalogItem) error {
	if len(items) == 0 {
		return nil
	}
	idSet := map[int64]bool{}
	for _, it := range items {
		if it.SampleSegID != 0 {
			idSet[it.SampleSegID] = true
		}
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	copies, err := store.SegmentCopies(ctx, ids)
	if err != nil {
		return err
	}
	onSSD, err := store.SegmentsOnSSD(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Copies = copies[items[i].SampleSegID]
		if items[i].Copies == nil {
			items[i].Copies = []db.CopyRef{}
		}
		items[i].OnSSD = onSSD[items[i].SampleSegID]
	}
	return nil
}
