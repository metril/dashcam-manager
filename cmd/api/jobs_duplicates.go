// jobs_duplicates.go registers the "duplicates_refresh" job: a thin
// wrapper around db.DuplicateReportRefresh (internal/db/queries_storage.go)
// so a scan or hash_backfill completing, or an operator hitting POST
// /api/duplicates/refresh, all go through the job queue rather than
// running the rebuild inline on whatever goroutine/request happened to
// trigger it — same reasoning as tracks_backfill/hash_backfill: only one
// rebuild should run at a time (SetLimit 1), and a request handler
// shouldn't block on a full-table rebuild.
package main

import (
	"context"
	"fmt"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// duplicatesRefreshJobParams is empty: the rebuild always covers every
// storage (file_copies has no per-share scoping worth exposing yet), so
// there's nothing to parameterize.
type duplicatesRefreshJobParams struct{}

// duplicatesRefreshJobHandler runs db.DuplicateReportRefresh under the
// job's own ctx.
func duplicatesRefreshJobHandler(store *db.DB) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		if err := store.DuplicateReportRefresh(ctx); err != nil {
			return nil, fmt.Errorf("duplicates_refresh job: %w", err)
		}
		return map[string]any{"status": "ok"}, nil
	}
}
