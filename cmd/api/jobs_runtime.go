package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/media"
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

// thumbsJobParams is the params shape for a "thumbs" job: either an
// explicit set of segment ids, or a source id to generate thumbnails for
// every segment with thumb_status != 'done'.
type thumbsJobParams struct {
	SourceID   int64   `json:"source_id,omitempty"`
	SegmentIDs []int64 `json:"segment_ids,omitempty"`
}

// registerJobHandlers wires every production job type into q. Call once at
// startup before q.Start.
//
// It also resolves the ffmpeg hardware-acceleration mode (settings.hw_mode
// override, else an ffmpeg -hwaccels probe against cfg.QSVDevice) once
// here — synchronously, before the HTTP server starts — logging the
// chosen mode and caching it (setHWMode) for the on-demand thumbnail/
// sprite HTTP handlers to reuse without re-probing per request.
func registerJobHandlers(q *jobs.Queue, store *db.DB) {
	q.RegisterHandler("scan", scanJobHandler(store, q))
	q.RegisterHandler("index_detections", indexDetectionsJobHandler(store))

	cfg := config.Load()
	hwCtx, cancelHW := context.WithTimeout(context.Background(), 20*time.Second)
	hwMode := resolveHWMode(hwCtx, store, cfg.QSVDevice)
	cancelHW()
	setHWMode(hwMode)
	log.Printf("[media] hardware acceleration mode: %s", hwMode)

	q.RegisterHandler("thumbs", thumbsJobHandler(store, cfg.Cache, cfg.QSVDevice, hwMode))
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
		// Lower priority still than index_detections: thumbnails are purely
		// cosmetic (browse cards, hover scrub) and shouldn't compete with
		// the detection explosion for worker/ffmpeg attention right after a
		// scan.
		if _, enqErr := q.Enqueue(context.Background(), "thumbs", thumbsJobParams{SourceID: src.ID}, -2); enqErr != nil {
			log.Printf("[jobs] scan %d: enqueue thumbs: %v", job.ID, enqErr)
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

// defaultFFmpegConcurrency is used when settings.max_ffmpeg_jobs is unset
// or invalid — the design spec caps concurrent ffmpeg processes at 2 for
// the target HD 630 iGPU.
const defaultFFmpegConcurrency = 2

// thumbsJobHandler generates a thumbnail (and, best-effort, a hover-scrub
// sprite) for every segment named by job.Params (segment_ids, or every
// segment of source_id with thumb_status != 'done'), running up to
// settings.max_ffmpeg_jobs of them concurrently — the job itself fans out
// that many ffmpeg processes in parallel rather than the queue limiting
// how many "thumbs" jobs run at once (there's normally only one
// outstanding per source). Segments are marked 'done' or 'failed'
// individually so a handful of unreadable files don't fail the whole
// batch, and once a segment's thumbnail lands, any trip it belongs to that
// still has no cover_thumb is given one (first done front-camera segment,
// by seq).
func thumbsJobHandler(store *db.DB, cacheDir, device string, hwMode media.Mode) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params thumbsJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("thumbs job: invalid params: %w", err)
		}

		segs, err := store.SegmentsForThumbs(ctx, params.SourceID, params.SegmentIDs)
		if err != nil {
			return nil, fmt.Errorf("thumbs job: %w", err)
		}
		total := len(segs)
		if total == 0 {
			return map[string]int{"done": 0, "failed": 0, "total": 0}, nil
		}

		concurrency := ffmpegConcurrency(ctx, store)
		sem := make(chan struct{}, concurrency)

		var (
			mu           sync.Mutex
			wg           sync.WaitGroup
			doneCount    int
			failedCount  int
			processed    int
			touchedTrips = map[int64]struct{}{}
		)

		for _, seg := range segs {
			if ctx.Err() != nil {
				break
			}
			seg := seg
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				ok := generateSegmentThumb(ctx, store, cacheDir, device, hwMode, seg)

				mu.Lock()
				processed++
				if ok {
					doneCount++
				} else {
					failedCount++
				}
				if seg.TripID != nil {
					touchedTrips[*seg.TripID] = struct{}{}
				}
				report(float64(processed)/float64(total), fmt.Sprintf("%d/%d thumbnails", processed, total))
				mu.Unlock()
			}()
		}
		wg.Wait()

		for tripID := range touchedTrips {
			if err := assignTripCover(context.Background(), store, tripID); err != nil {
				log.Printf("[jobs] thumbs %d: trip cover %d: %v", job.ID, tripID, err)
			}
		}

		return map[string]int{"done": doneCount, "failed": failedCount, "total": total}, nil
	}
}

// generateSegmentThumb generates (and best-effort, alongside, a sprite
// for) one segment, persisting its thumb_status, and returns whether the
// thumbnail itself succeeded (sprite failures are logged but don't fail
// the segment — hover-scrub is a nice-to-have on top of the thumbnail).
func generateSegmentThumb(ctx context.Context, store *db.DB, cacheDir, device string, hwMode media.Mode, seg db.ThumbSegment) bool {
	in := filepath.Join(seg.RootPath, seg.RelPath)
	out := media.ThumbPath(cacheDir, seg.ID)

	err := media.Thumbnail(ctx, in, out, media.ThumbOptions{
		Mode: hwMode, Device: device, AtSec: thumbAtSec(seg.DurationS), Width: 480,
	})

	status := "done"
	if err != nil {
		log.Printf("[media] thumbnail %s (segment %d): %v", in, seg.ID, err)
		status = "failed"
	}
	if setErr := store.SegmentSetThumbStatus(ctx, seg.ID, status); setErr != nil {
		log.Printf("[jobs] set thumb status %d: %v", seg.ID, setErr)
	}

	if err == nil {
		spriteJPG, spriteVTT := media.SpritePaths(cacheDir, seg.ID)
		duration := 30.0
		if seg.DurationS != nil && *seg.DurationS > 0 {
			duration = *seg.DurationS
		}
		if spriteErr := media.Sprite(ctx, in, spriteJPG, spriteVTT, media.SpriteOptions{
			Mode: hwMode, Device: device, SegmentDurationS: duration,
		}); spriteErr != nil {
			log.Printf("[media] sprite %s (segment %d): %v", in, seg.ID, spriteErr)
		}
	}

	return err == nil
}

// thumbAtSec picks a seek offset for the thumbnail frame: 5s in for a
// normal ~30s segment, half its duration for anything shorter (so a
// truncated tail segment still gets a real frame instead of ffmpeg
// seeking past EOF).
func thumbAtSec(durationS *float64) float64 {
	const defaultAt = 5.0
	if durationS != nil && *durationS > 0 && *durationS < 2*defaultAt {
		return *durationS / 2
	}
	return defaultAt
}

// assignTripCover gives tripID a cover_thumb (idempotent — TripSetCoverThumb
// only writes if it's still unset) once its first-by-seq front-camera
// segment has a thumbnail.
func assignTripCover(ctx context.Context, store *db.DB, tripID int64) error {
	segID, ok, err := store.TripCoverCandidate(ctx, tripID)
	if err != nil || !ok {
		return err
	}
	return store.TripSetCoverThumb(ctx, tripID, fmt.Sprintf("/api/segments/%d/thumb.jpg", segID))
}

// ffmpegConcurrency reads settings.max_ffmpeg_jobs, defaulting to
// defaultFFmpegConcurrency when unset or invalid.
func ffmpegConcurrency(ctx context.Context, store *db.DB) int {
	n := defaultFFmpegConcurrency
	if raw, err := store.SettingsGet(ctx, "max_ffmpeg_jobs"); err == nil {
		var v int
		if json.Unmarshal(raw, &v) == nil && v > 0 {
			n = v
		}
	}
	return n
}
