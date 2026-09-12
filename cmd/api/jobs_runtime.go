package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/media"
	"dashcam/internal/scan"
	"dashcam/internal/share"
)

// scanJobParams is the params shape for a "scan" job, shared between
// handleSourcesScan (which enqueues one) and scanJobHandler (which decodes
// it).
type scanJobParams struct {
	SourceID int64 `json:"source_id"`
	Full     bool  `json:"full"`
	// Force bypasses scan.Scan's empty-root guard (see its doc comment) —
	// surfaced as a checkbox on the source scan UI with a warning, for the
	// rare case an operator really did delete everything off a source and
	// wants the index to reflect that. Not needed for a normal fully-
	// ingested-and-cleaned source: the guard already ignores archived
	// rows on its own.
	Force bool `json:"force"`
}

// refreshCatalogAsync schedules a db.RefreshCatalog run in its own
// goroutine with a background context, rather than the caller's. Mirrors
// main.go's startup catalog backfill: RefreshCatalog is a slow (multi-
// second, even after WP8's staging-table rework — see
// queries_catalog.go) rebuild, so running it inline in a job handler (or,
// worse, an HTTP handler) would hold up that job/request for no benefit —
// callers that just made the catalog stale (index_detections, cleanup) only
// need the refresh scheduled, not finished before they return. label
// identifies the caller in the log line on failure.
func refreshCatalogAsync(store *db.DB, label string) {
	go func() {
		if err := store.RefreshCatalog(context.Background()); err != nil {
			log.Printf("[jobs] %s: refresh catalog: %v", label, err)
		}
	}()
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
	// Force bypasses the thumb_status='done' skip in SegmentsForThumbs —
	// set by POST /api/maintenance/thumbs/regenerate, which has already
	// deleted the cached files for its scope and needs them regenerated
	// even though the DB still says 'done'. See SegmentsForThumbs' doc
	// comment.
	Force bool `json:"force,omitempty"`
}

// ffmpegSem bounds the number of ffmpeg processes running concurrently
// across the whole process — both the background "thumbs" job's fan-out
// and on-demand generation triggered by a thumbnail/sprite cache-miss GET
// (handlers_thumbs.go) acquire a slot from this same channel via
// acquireFFmpegSlot/releaseFFmpegSlot, so a burst of browser requests can
// never run more ffmpeg processes than settings.max_ffmpeg_jobs allows
// alongside a running thumbs job. Sized once at boot in registerJobHandlers
// (like hw mode detection, a runtime change to the setting takes effect on
// restart).
var ffmpegSem chan struct{}

// acquireFFmpegSlot blocks until a slot in ffmpegSem is free or ctx is
// done, whichever comes first.
func acquireFFmpegSlot(ctx context.Context) error {
	select {
	case ffmpegSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseFFmpegSlot returns a slot acquired via acquireFFmpegSlot.
func releaseFFmpegSlot() { <-ffmpegSem }

// registerJobHandlers wires every production job type into q. Call once at
// startup before q.Start.
//
// Hardware-acceleration mode (settings.hw_mode override, else an ffmpeg
// -hwaccels probe against cfg.QSVDevice) is resolved in a background
// goroutine rather than blocking here: a real QSV/VAAPI probe execs
// ffmpeg and can take real wall time, and registerJobHandlers runs
// synchronously before the HTTP server starts listening (and is called
// directly by most handler tests) — blocking it would delay every server
// boot and risk slow tests. getHWMode() (used by both the "thumbs" job and
// the on-demand thumbnail/sprite HTTP handlers) defaults to ModeSoftware
// until this goroutine calls setHWMode with the detected result, so a
// request that lands before detection finishes just gets a software-path
// thumbnail instead of blocking or erroring.
func registerJobHandlers(q *jobs.Queue, store *db.DB) {
	q.RegisterHandler("scan", scanJobHandler(store, q))
	q.RegisterHandler("index_detections", indexDetectionsJobHandler(store))
	q.RegisterHandler("tracks_backfill", tracksBackfillJobHandler(store))
	// Only one tracks_backfill job may run at a time — it walks the whole
	// corpus, so a second concurrent instance would just duplicate work
	// (and race on the same segments' det_tracks rows) rather than
	// speeding anything up. See main.go's boot auto-enqueue, which also
	// checks ExistsActive before enqueuing.
	q.SetLimit("tracks_backfill", 1)
	q.RegisterHandler("hash_backfill", hashBackfillJobHandler(store))
	// Same reasoning as tracks_backfill's limit: hash_backfill walks
	// sha256-IS-NULL rows across every share (or one, if scoped), so a
	// second concurrent instance would just contend for the same rows.
	q.SetLimit("hash_backfill", 1)
	q.RegisterHandler("ingest", ingestJobHandler(store, q))
	q.RegisterHandler("cleanup", cleanupJobHandler(store))

	cfg := config.Load()
	registerDashcamJobHandlers(q, store, cfg.SecretKey)
	ffmpegSem = make(chan struct{}, ffmpegConcurrency(context.Background(), store))

	go func() {
		hwCtx, cancelHW := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelHW()
		hwMode := resolveHWMode(hwCtx, store, cfg.QSVDevice)
		setHWMode(hwMode)
		log.Printf("[media] hardware acceleration mode: %s", hwMode)

		// Fix round 1: DetectCapabilities used to share hwCtx with
		// resolveHWMode above. hwCtx's 20s budget is meant for ONE probe
		// (resolveHWMode runs at most one hwaccel init); DetectCapabilities
		// runs several more ffmpeg invocations (-encoders, -hwaccels, up
		// to two more QSV single-frame encodes) after it, so a slow first
		// probe (e.g. a cold/contended GPU) could leave little or no
		// budget left, silently detecting Capabilities{} (all-false) for
		// the rest of the process's life — every h265/av1 export request
		// would then 400 forever, even though the hardware/software
		// encoders are actually fine. Give it its own full-length timeout.
		capsCtx, cancelCaps := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelCaps()
		caps := media.DetectCapabilities(capsCtx, cfg.QSVDevice)
		setCapabilities(caps)
		log.Printf("[media] export codec capabilities: %+v", caps)
	}()

	q.RegisterHandler("thumbs", thumbsJobHandler(store, cfg.Cache, cfg.QSVDevice))
	registerExportJobHandler(q, store, cfg.Cache, cfg.QSVDevice)
	q.RegisterHandler("db_verify", dbVerifyJobHandler(store, cfg.DB, cfg.Cache))
	q.RegisterHandler("db_prune", dbPruneJobHandler(store, cfg.DB, cfg.Cache))
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
		// A share source's root_path only makes sense while the share is
		// actually mounted there — scanning it while unmounted would walk
		// whatever (likely empty) directory happens to sit at that path and,
		// combined with the empty-root guard in scan.Scan, would otherwise
		// either wipe the index or silently index nothing. This is a
		// read-only availability check, so kind=local uses
		// share.LocalDirReadable rather than ingest.ResolveTargetMount's
		// write probe (share.ProbeLocal) — a scan must never fail (or
		// write anything) just because a local storage happens to be
		// mounted read-only.
		if src.Kind == "share" {
			if src.ShareID == nil {
				return nil, fmt.Errorf("scan job: source %d is kind=share but has no share_id", src.ID)
			}
			sh, err := store.ShareGet(ctx, *src.ShareID)
			if err != nil {
				return nil, fmt.Errorf("scan job: share source %d: share %d: %w", src.ID, *src.ShareID, err)
			}
			if sh.MountPoint == "" {
				return nil, fmt.Errorf("scan job: share source %d not available: share %d has no mount point configured", src.ID, *src.ShareID)
			}
			if sh.Kind == db.KindLocal {
				if err := share.LocalDirReadable(sh.MountPoint); err != nil {
					return nil, fmt.Errorf("scan job: share source %d not available: %w", src.ID, err)
				}
			} else {
				mounted, err := share.IsMounted(sh.MountPoint)
				if err != nil {
					return nil, fmt.Errorf("scan job: share source %d: %w", src.ID, err)
				}
				if !mounted {
					return nil, fmt.Errorf("scan job: share source %d not available: not mounted at %s", src.ID, sh.MountPoint)
				}
			}
		}
		// Likewise, an SSD source's root_path only makes sense while the
		// SSD is actually plugged in and mounted there — same failure mode
		// as an unmounted share (index wipe or silent no-op) if we scanned
		// through it anyway.
		if src.Kind == "ssd" {
			if mounted, err := ssdMountedFn(src.RootPath); err != nil {
				return nil, fmt.Errorf("scan job: ssd source %d: is mounted %s: %w", src.ID, src.RootPath, err)
			} else if !mounted {
				return nil, fmt.Errorf("scan job: ssd source %d: %s is not mounted (SSD unplugged?)", src.ID, src.RootPath)
			}
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

		stats, scanErr := scan.Scan(ctx, store, src.ID, src.RootPath, scan.Opts{Full: params.Full, Force: params.Force, Progress: progress})

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
		// A share-backed source's scan is what populates storage_files
		// (internal/scan's storage-files walk) and stamps new/changed
		// segments' sha256 to NULL — auto-enqueue hash_backfill scoped to
		// this share so both catch up without an operator having to
		// remember to trigger it from the Storage page. Lowest priority
		// of the three: it's the least latency-sensitive (duplicate
		// detection, not yet wired — task 7/8 — has no UI waiting on it).
		if src.Kind == "share" && src.ShareID != nil {
			if _, enqErr := q.Enqueue(context.Background(), "hash_backfill", hashBackfillJobParams{ShareID: *src.ShareID}, -3); enqErr != nil {
				log.Printf("[jobs] scan %d: enqueue hash_backfill: %v", job.ID, enqErr)
			}
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
		// WP8 fix: catalog_terms (the precomputed cache GET /api/catalog
		// reads from — see queries_catalog.go) can only go stale by new
		// detections being indexed, so refresh it here rather than on every
		// read. A refresh failure doesn't fail the job: the detections
		// themselves are indexed fine, and the next successful
		// index_detections run (or the next full rebuild) will catch it up.
		if err := store.RefreshCatalog(ctx); err != nil {
			log.Printf("[jobs] index_detections %d: refresh catalog: %v", job.ID, err)
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

// tracksBackfillJobParams is the params shape for a "tracks_backfill" job:
// either an explicit set of segment ids, or a source id to scan for
// segments with detection boxes but no track_id assigned yet.
type tracksBackfillJobParams struct {
	SourceID   int64   `json:"source_id,omitempty"`
	SegmentIDs []int64 `json:"segment_ids,omitempty"`
}

// tracksBackfillJobHandler re-runs scan.IndexDetections for every segment
// SegmentsNeedingTracksBackfill (or an explicit segment_ids list) reports
// as missing tracks — e.g. after upgrading to schema v5's IoU tracker
// (internal/scan/tracks.go), whose det_tracks/track_id rows an older
// IndexDetections run never produced. IndexDetections re-reads
// detections_raw and rebuilds detections/det_objects/det_plates/
// det_vehicles/det_tracks from scratch in one transaction (see its doc
// comment), so this needs no filesystem access at all — safe to run with
// the SSD unmounted or ejected.
func tracksBackfillJobHandler(store *db.DB) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params tracksBackfillJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("tracks_backfill job: invalid params: %w", err)
		}

		segmentIDs := params.SegmentIDs
		if len(segmentIDs) == 0 {
			ids, err := store.SegmentsNeedingTracksBackfill(ctx, params.SourceID)
			if err != nil {
				return nil, err
			}
			segmentIDs = ids
		}

		total := len(segmentIDs)
		backfilled := 0
		for i, id := range segmentIDs {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err := scan.IndexDetections(ctx, store, id); err != nil {
				return nil, fmt.Errorf("tracks_backfill: segment %d: %w", id, err)
			}
			backfilled++
			if total > 0 {
				report(float64(i+1)/float64(total), fmt.Sprintf("backfilled tracks %d/%d segments", i+1, total))
			}
		}
		return map[string]int{"backfilled": backfilled, "total": total}, nil
	}
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
// outstanding per source). It runs in two passes, both under
// acquireFFmpegSlot: thumbnails for every segment first, then sprites for
// every segment whose thumbnail succeeded — so the UI grid (which only
// needs thumbnails) populates immediately instead of waiting on the
// slower sprite pass too. Segments are marked 'done' or 'failed'
// individually so a handful of unreadable files don't fail the whole
// batch, and once a segment's thumbnail lands, any trip it belongs to that
// still has no cover_thumb is given one (first done front-camera segment,
// by seq).
func thumbsJobHandler(store *db.DB, cacheDir, device string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params thumbsJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("thumbs job: invalid params: %w", err)
		}

		segs, err := store.SegmentsForThumbs(ctx, params.SourceID, params.SegmentIDs, params.Force)
		if err != nil {
			return nil, fmt.Errorf("thumbs job: %w", err)
		}
		total := len(segs)
		if total == 0 {
			return map[string]int{"done": 0, "failed": 0, "total": 0}, nil
		}

		hwMode := getHWMode()

		var (
			mu              sync.Mutex
			wg              sync.WaitGroup
			doneCount       int
			failedCount     int
			unreadableCount int
			processed       int
			touchedTrips    = map[int64]struct{}{}
			spriteQueue     = make([]db.ThumbSegment, 0, total)
			hwFallbacks     int64
			firstFallback   int32
		)
		// onHWFallback is shared by every thumbnail/sprite generated in
		// this job: it counts every hw->software fallback (surfaced as
		// "N hardware fallbacks" in the job's final message below) but
		// only logs the first one's ffmpeg stderr at warn level, instead
		// of media.Thumbnail/Sprite's default of one log.Printf per
		// occurrence — with potentially thousands of segments hitting
		// the same underlying hardware issue, that default would flood
		// the log with an identical message per segment.
		onHWFallback := func(err error) {
			atomic.AddInt64(&hwFallbacks, 1)
			if atomic.CompareAndSwapInt32(&firstFallback, 0, 1) {
				log.Printf("[jobs] WARN thumbs %d: hardware fallback (further occurrences this job are counted, not logged): %v", job.ID, err)
			}
		}

		// Pass 1: thumbnails for every segment.
		for _, seg := range segs {
			if ctx.Err() != nil {
				break
			}
			seg := seg
			if err := acquireFFmpegSlot(ctx); err != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer releaseFFmpegSlot()

				result := generateSegmentThumbnail(ctx, store, cacheDir, device, hwMode, seg, onHWFallback)

				mu.Lock()
				processed++
				switch result {
				case thumbResultDone:
					doneCount++
					spriteQueue = append(spriteQueue, seg)
				case thumbResultUnreadable:
					unreadableCount++
				default:
					failedCount++
				}
				if seg.TripID != nil {
					touchedTrips[*seg.TripID] = struct{}{}
				}
				report(0.5*float64(processed)/float64(total), fmt.Sprintf("%d/%d thumbnails", processed, total))
				mu.Unlock()
			}()
		}
		wg.Wait()

		for tripID := range touchedTrips {
			if err := assignTripCover(context.Background(), store, tripID); err != nil {
				log.Printf("[jobs] thumbs %d: trip cover %d: %v", job.ID, tripID, err)
			}
		}

		// Pass 2: hover-scrub sprites, best-effort, only for segments whose
		// thumbnail succeeded — failures here don't affect thumb_status or
		// the done/failed counts above.
		spriteTotal := len(spriteQueue)
		spriteProcessed := 0
		for _, seg := range spriteQueue {
			if ctx.Err() != nil {
				break
			}
			seg := seg
			if err := acquireFFmpegSlot(ctx); err != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer releaseFFmpegSlot()

				generateSegmentSprite(ctx, store, cacheDir, device, hwMode, seg, onHWFallback)

				mu.Lock()
				spriteProcessed++
				report(0.5+0.5*float64(spriteProcessed)/float64(spriteTotal), fmt.Sprintf("%d/%d sprites", spriteProcessed, spriteTotal))
				mu.Unlock()
			}()
		}
		wg.Wait()

		n := atomic.LoadInt64(&hwFallbacks)
		msg := fmt.Sprintf("%d/%d done, %d failed, %d hardware fallbacks", doneCount, total, failedCount, n)
		if unreadableCount > 0 {
			msg += fmt.Sprintf(", %d unreadable", unreadableCount)
		}
		report(1.0, msg)

		accumulateThumbHWStats(context.Background(), store, hwMode, doneCount, int(n), unreadableCount)

		return map[string]int{
			"done": doneCount, "failed": failedCount, "total": total,
			"hw_fallbacks": int(n), "unreadable": unreadableCount,
		}, nil
	}
}

// thumbResult is generateSegmentThumbnail's outcome for one segment.
type thumbResult int

const (
	thumbResultDone thumbResult = iota
	thumbResultFailed
	thumbResultUnreadable
)

// generateSegmentThumbnail generates one segment's thumbnail and persists
// its outcome: 'done' on success, or — on failure — either marked
// unreadable (media.UnreadableReason recognizes the ffmpeg error as a
// file that can never decode; see SegmentMarkUnreadable) or left 'failed'
// for a later thumbs job to retry (a transient or hardware-fallback
// error). onHWFallback (may be nil) is passed through to media.Thumbnail
// to count/log a hw->software fallback job-wide instead of once per
// segment — see thumbsJobHandler. A hardware fallback alone never reaches
// this function's err: media.Thumbnail already retries in software before
// returning, so only a failure that also fails in software (or the
// original error if Mode was already software) shows up here.
// stillCopyingWindow is how recently a file must have been written to for
// inputMaybeStillCopying to treat an ffmpeg "unreadable" signature as "not
// finished writing yet" rather than a genuinely corrupt file — long enough
// to cover a dashcam/rsync-style copy still trickling in, short enough
// that a file this stale is almost certainly done (or truly broken).
const stillCopyingWindow = 2 * time.Minute

// inputMaybeStillCopying reports whether in looks like it might still be
// mid-copy (or the SSD it lives on might have just been unplugged) rather
// than a permanently corrupt file: os.Stat failing outright (ENOENT/EIO —
// the SSD went away mid-read), a 0-byte file, or an mtime within the last
// stillCopyingWindow (still trickling in from a copy) all mean ffmpeg's
// "unreadable" signature might just be a snapshot of a half-written file,
// not proof the file can never decode — so generateSegmentThumbnail must
// not call SegmentMarkUnreadable in any of those cases and should instead
// leave the segment 'failed' for a later thumbs job to retry once the
// copy/remount has settled.
func inputMaybeStillCopying(in string) bool {
	info, err := os.Stat(in)
	if err != nil {
		return true
	}
	if info.Size() == 0 {
		return true
	}
	return time.Since(info.ModTime()) < stillCopyingWindow
}

func generateSegmentThumbnail(ctx context.Context, store *db.DB, cacheDir, device string, hwMode media.Mode, seg db.ThumbSegment, onHWFallback func(error)) thumbResult {
	in := filepath.Join(seg.RootPath, seg.RelPath)
	out := media.ThumbPath(cacheDir, seg.ID)

	err := media.Thumbnail(ctx, in, out, media.ThumbOptions{
		Mode: hwMode, Device: device, AtSec: thumbAtSec(seg.DurationS), Width: 480,
		OnHWFallback: onHWFallback,
	})
	if err == nil {
		if setErr := store.SegmentSetThumbStatus(ctx, seg.ID, "done"); setErr != nil {
			log.Printf("[jobs] set thumb status %d: %v", seg.ID, setErr)
		}
		return thumbResultDone
	}

	log.Printf("[media] thumbnail %s (segment %d): %v", in, seg.ID, err)
	if reason, ok := media.UnreadableReason(err); ok && !inputMaybeStillCopying(in) {
		if setErr := store.SegmentMarkUnreadable(ctx, seg.ID, reason); setErr != nil {
			log.Printf("[jobs] mark unreadable %d: %v", seg.ID, setErr)
		}
		return thumbResultUnreadable
	}
	if setErr := store.SegmentSetThumbStatus(ctx, seg.ID, "failed"); setErr != nil {
		log.Printf("[jobs] set thumb status %d: %v", seg.ID, setErr)
	}
	return thumbResultFailed
}

// generateSegmentSprite generates one segment's hover-scrub sprite sheet.
// Failures are logged only — hover-scrub is a nice-to-have on top of the
// thumbnail, so it never affects the job's done/failed counts. When
// media.Sprite discovers (via ffprobe, after a seek-overshoot empty-output
// failure) that seg.DurationS was stale, it persists the corrected value
// to segments.duration_s so later thumbnail/sprite/seek math for this
// segment uses the right duration instead of repeating the overshoot.
func generateSegmentSprite(ctx context.Context, store *db.DB, cacheDir, device string, hwMode media.Mode, seg db.ThumbSegment, onHWFallback func(error)) {
	in := filepath.Join(seg.RootPath, seg.RelPath)
	spriteJPG, spriteVTT := media.SpritePaths(cacheDir, seg.ID)
	duration := 30.0
	if seg.DurationS != nil && *seg.DurationS > 0 {
		duration = *seg.DurationS
	}
	if err := media.Sprite(ctx, in, spriteJPG, spriteVTT, media.SpriteOptions{
		Mode: hwMode, Device: device, SegmentDurationS: duration,
		OnHWFallback: onHWFallback,
		OnDurationCorrected: func(correctedS float64) {
			if setErr := store.SegmentSetDurationS(ctx, seg.ID, correctedS); setErr != nil {
				log.Printf("[jobs] correct duration_s for segment %d: %v", seg.ID, setErr)
			}
		},
	}); err != nil {
		log.Printf("[media] sprite %s (segment %d): %v", in, seg.ID, err)
	}
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
