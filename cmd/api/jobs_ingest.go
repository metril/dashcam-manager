package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"dashcam/internal/db"
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
)

// ingestJobParams is the params shape for an "ingest" job, shared between
// handleIngestPost (which enqueues one) and ingestJobHandler (which
// decodes it).
type ingestJobParams struct {
	SourceID      int64   `json:"source_id"`
	TargetShareID int64   `json:"target_share_id"`
	TripIDs       []int64 `json:"trip_ids,omitempty"`
	Verify        string  `json:"verify,omitempty"`
	DeleteAfter   bool    `json:"delete_after,omitempty"`
}

// cleanupJobParams is the params shape for a "cleanup" job.
type cleanupJobParams struct {
	SourceID   int64   `json:"source_id"`
	TripIDs    []int64 `json:"trip_ids,omitempty"`
	SegmentIDs []int64 `json:"segment_ids,omitempty"`
}

// ingestJobHandler runs ingest.Run for the source/share/trips named in
// job.Params, reporting byte-level progress, then (best-effort) enqueues a
// scan of the target share's own source so the newly-copied segments get
// indexed there independently of the SSD-sourced rows, and — if
// delete_after was requested and every segment verified cleanly —
// immediately runs cleanup for the same trips. Mirrors thumbsJobHandler's
// convention of not failing the whole job over a handful of bad segments:
// per-segment failures are recorded in the returned ingest.Stats
// (SegmentsFailed) rather than turned into a job-level error, so a
// partially-successful ingest still shows up as "done" with the failure
// count visible in its result.
func ingestJobHandler(store *db.DB, q *jobs.Queue) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params ingestJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("ingest job: invalid params: %w", err)
		}

		target, err := ingest.ResolveTargetMount(ctx, store, params.TargetShareID)
		if err != nil {
			return nil, fmt.Errorf("ingest job: %w", err)
		}

		stats, err := ingest.Run(ctx, store, target, ingest.Opts{
			SourceID: params.SourceID, TargetShareID: params.TargetShareID,
			TripIDs: params.TripIDs, Verify: params.Verify,
		}, func(done, total int, bytesDone, bytesTotal int64, msg string) {
			var p float64
			if total > 0 {
				p = float64(done) / float64(total)
			}
			if p > 0.99 {
				p = 0.99 // reserve 1.0 for the queue's own "finished" transition
			}
			report(p, fmt.Sprintf("%d/%d segments (%s/%s) — %s",
				done, total, formatBytesShort(bytesDone), formatBytesShort(bytesTotal), msg))
		})
		if err != nil {
			return nil, fmt.Errorf("ingest job: %w", err)
		}

		if shareSrc, srcErr := store.SourceGetByShareID(context.Background(), params.TargetShareID); srcErr == nil {
			if _, enqErr := q.Enqueue(context.Background(), "scan", scanJobParams{SourceID: shareSrc.ID}, -1); enqErr != nil {
				log.Printf("[jobs] ingest %d: enqueue scan of share source: %v", job.ID, enqErr)
			}
		} else {
			log.Printf("[jobs] ingest %d: find target share's source: %v", job.ID, srcErr)
		}

		if params.DeleteAfter && stats.SegmentsFailed == 0 {
			// Uses ctx (this job's own context, cancelled on shutdown/user
			// cancel) rather than context.Background(), and is scoped to
			// exactly the segment ids this run itself verified
			// (stats.VerifiedSegmentIDs) rather than every verified
			// segment across params.TripIDs — the latter could reach
			// segments a completely unrelated earlier run verified.
			if _, cErr := ingest.Cleanup(ctx, store, ingest.CleanupOpts{
				SourceID:   params.SourceID,
				SegmentIDs: stats.VerifiedSegmentIDs,
			}, nil); cErr != nil {
				log.Printf("[jobs] ingest %d: delete_after cleanup: %v", job.ID, cErr)
			}
		}

		return stats, nil
	}
}

// cleanupJobHandler runs ingest.Cleanup (never in dry-run mode — the HTTP
// handler answers a dry-run request synchronously without enqueueing a
// job at all) for the trips named in job.Params (empty = every verified
// segment).
func cleanupJobHandler(store *db.DB) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params cleanupJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("cleanup job: invalid params: %w", err)
		}

		stats, err := ingest.Cleanup(ctx, store, ingest.CleanupOpts{
			SourceID: params.SourceID, TripIDs: params.TripIDs, SegmentIDs: params.SegmentIDs,
		}, func(done, total int, msg string) {
			var p float64
			if total > 0 {
				p = float64(done) / float64(total)
			}
			if p > 0.99 {
				p = 0.99
			}
			report(p, msg)
		})
		if err != nil {
			return nil, fmt.Errorf("cleanup job: %w", err)
		}
		return stats, nil
	}
}

// formatBytesShort renders n bytes as a short human-readable string
// (e.g. "1.2GB") for progress messages — a tiny local helper rather than
// pulling in the frontend's formatting conventions.
func formatBytesShort(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
