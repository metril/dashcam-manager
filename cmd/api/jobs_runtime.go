package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/scan"
)

// scanJobParams is the params shape for a "scan" job, shared between
// handleSourcesScan (which enqueues one) and scanJobHandler (which decodes
// it).
type scanJobParams struct {
	SourceID int64 `json:"source_id"`
	Full     bool  `json:"full"`
}

// indexDetectionsJobParams is the params shape for an "index_detections"
// job: either an explicit set of segment ids, or a source id to scan for
// segments that have a detections sidecar but no exploded rows yet.
type indexDetectionsJobParams struct {
	SourceID   int64   `json:"source_id,omitempty"`
	SegmentIDs []int64 `json:"segment_ids,omitempty"`
}

// registerJobHandlers wires every production job type into q. Call once at
// startup before q.Start.
func registerJobHandlers(q *jobs.Queue, store *db.DB) {
	q.RegisterHandler("scan", scanJobHandler(store, q))
	q.RegisterHandler("index_detections", indexDetectionsJobHandler(store))
}

// scanJobHandler runs scan.Scan (then, best-effort, legacy DB enrichment)
// for the source named in job.Params, mirroring the pre-M7 runScan
// goroutine but driven by the job queue: progress flows through report,
// cancellation flows through ctx (cancelled by the queue once
// cancel_requested is observed), and a low-priority "index_detections" job
// is auto-enqueued for the source on success so the expensive per-frame
// explosion happens off the scan's critical path.
func scanJobHandler(store *db.DB, q *jobs.Queue) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params scanJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("scan job: invalid params: %w", err)
		}

		src, err := store.SourceGet(ctx, params.SourceID)
		if err != nil {
			return nil, fmt.Errorf("scan job: source %d: %w", params.SourceID, err)
		}
		if err := store.SourceSetStatus(context.Background(), src.ID, "scanning"); err != nil {
			log.Printf("[jobs] scan %d: set status: %v", job.ID, err)
		}

		progress := func(done, total int, msg string) {
			var p float64
			if total > 0 {
				p = float64(done) / float64(total)
			}
			if p > 0.99 {
				p = 0.99 // reserve 1.0 for the queue's own "finished" transition
			}
			report(p, msg)
		}

		stats, scanErr := scan.Scan(ctx, store, src.ID, src.RootPath, scan.Opts{Full: params.Full, Progress: progress})

		status := "idle"
		if scanErr != nil {
			status = "error"
		} else if _, legacyErr := scan.EnrichFromLegacyDB(ctx, store, src.ID, src.RootPath); legacyErr != nil {
			// Best-effort by design, same as the pre-M7 goroutine.
			log.Printf("[jobs] scan %d: legacy db enrichment: %v", job.ID, legacyErr)
		}
		finishedAt := time.Now().UTC().Format(time.RFC3339)
		if setErr := store.SourceSetLastScanAt(context.Background(), src.ID, finishedAt, status); setErr != nil {
			log.Printf("[jobs] scan %d: set last_scan_at: %v", job.ID, setErr)
		}

		if scanErr != nil {
			return nil, scanErr
		}

		if _, enqErr := q.Enqueue(context.Background(), "index_detections", indexDetectionsJobParams{SourceID: src.ID}, -1); enqErr != nil {
			log.Printf("[jobs] scan %d: enqueue index_detections: %v", job.ID, enqErr)
		}

		return stats, nil
	}
}

// indexDetectionsJobHandler runs scan.IndexDetections over either an
// explicit segment_ids list or every segment (optionally scoped to
// source_id) that has a detections sidecar (has_detections=1) but no
// exploded detections/det_objects/... rows yet.
func indexDetectionsJobHandler(store *db.DB) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params indexDetectionsJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("index_detections job: invalid params: %w", err)
		}

		segmentIDs := params.SegmentIDs
		if len(segmentIDs) == 0 {
			ids, err := segmentsNeedingIndex(ctx, store, params.SourceID)
			if err != nil {
				return nil, err
			}
			segmentIDs = ids
		}

		total := len(segmentIDs)
		indexed := 0
		for i, id := range segmentIDs {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err := scan.IndexDetections(ctx, store, id); err != nil {
				return nil, fmt.Errorf("index_detections: segment %d: %w", id, err)
			}
			indexed++
			if total > 0 {
				report(float64(i+1)/float64(total), fmt.Sprintf("indexed %d/%d segments", i+1, total))
			}
		}
		return map[string]int{"indexed": indexed, "total": total}, nil
	}
}

// segmentsNeedingIndex returns ids of segments with has_detections=1 but no
// row in `detections` yet, optionally scoped to sourceID (0 = all sources).
func segmentsNeedingIndex(ctx context.Context, store *db.DB, sourceID int64) ([]int64, error) {
	query := `SELECT s.id FROM segments s WHERE s.has_detections = 1 AND NOT EXISTS (SELECT 1 FROM detections d WHERE d.segment_id = s.id)`
	var args []any
	if sourceID != 0 {
		query += ` AND s.source_id = ?`
		args = append(args, sourceID)
	}

	rows, err := store.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("segments needing index: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
