package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/maintenance"
	"dashcam/internal/media"
)

// lastDBVerifySettingsKey is where the most recent db_verify job's report
// is persisted, so GET /api/maintenance/report can serve it without
// re-running the checks or reaching into the jobs table.
const lastDBVerifySettingsKey = "last_db_verify"

// dbVerifyJobHandler runs maintenance.Verify and persists its report both
// as the job's result and under settings key lastDBVerifySettingsKey.
func dbVerifyJobHandler(store *db.DB, dbPath, cacheDir string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		report(0, "checking database integrity")
		rep, err := maintenance.Verify(ctx, store, dbPath, cacheDir)
		if err != nil {
			return nil, fmt.Errorf("db_verify job: %w", err)
		}

		raw, err := json.Marshal(rep)
		if err != nil {
			return nil, fmt.Errorf("db_verify job: marshal report: %w", err)
		}
		if err := store.SettingsSet(context.Background(), lastDBVerifySettingsKey, raw); err != nil {
			// Best-effort, like refreshCatalogAsync's callers elsewhere:
			// the report is still returned as the job's own result even
			// if persisting the "last" copy fails.
			log.Printf("[jobs] db_verify %d: persist last report: %v", job.ID, err)
		}

		msg := fmt.Sprintf("integrity %s, %d missing media, %d orphan rows, %d orphan thumb files",
			map[bool]string{true: "ok", false: "FAILED"}[rep.IntegrityOK],
			rep.MissingMediaSegments,
			rep.OrphanDetections+rep.OrphanDetObjects+rep.OrphanDetPlates+rep.OrphanDetVehicles+rep.OrphanDetTracks,
			rep.OrphanThumbFiles)
		report(1.0, msg)

		return rep, nil
	}
}

// dbPruneJobParams is the params shape for a "db_prune" job — decoded from
// POST /api/maintenance/db/prune's body.
type dbPruneJobParams struct {
	DryRun bool `json:"dry_run"`
	Vacuum bool `json:"vacuum"`
}

// dbPruneJobHandler runs maintenance.Prune with the requested options.
func dbPruneJobHandler(store *db.DB, dbPath, cacheDir string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params dbPruneJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("db_prune job: invalid params: %w", err)
		}

		if params.DryRun {
			report(0, "computing prune counts (dry run)")
		} else {
			report(0, "pruning orphaned rows and files")
		}

		res, err := maintenance.Prune(ctx, store, dbPath, cacheDir, maintenance.PruneOpts{
			DryRun: params.DryRun, Vacuum: params.Vacuum,
		})
		if err != nil {
			return nil, fmt.Errorf("db_prune job: %w", err)
		}

		verb := "would remove"
		if !params.DryRun {
			verb = "removed"
		}
		report(1.0, fmt.Sprintf("%s %d segments, %d orphan rows, %d orphan thumb files",
			verb, res.SegmentsRemoved,
			res.OrphanDetectionsRemoved+res.OrphanDetObjectsRemoved+res.OrphanDetPlatesRemoved+res.OrphanDetVehiclesRemoved+res.OrphanDetTracksRemoved,
			res.OrphanThumbFilesRemoved))

		return res, nil
	}
}

// thumbHWStatsSettingsKey persists cumulative thumbnail hardware-
// acceleration counters across every "thumbs" job run — GET /api/stats
// surfaces it as the "thumbs" field.
const thumbHWStatsSettingsKey = "thumb_hw_stats"

// thumbHWStats is the JSON shape of the thumb_hw_stats setting.
type thumbHWStats struct {
	LastMode    string `json:"last_mode"`
	HWOk        int64  `json:"hw_ok"`
	HWFallbacks int64  `json:"hw_fallbacks"`
	Unreadable  int64  `json:"unreadable"`
	LastRunAt   string `json:"last_run"`
}

// accumulateThumbHWStats folds one "thumbs" job run's outcome into the
// persisted thumb_hw_stats setting: hwOK is an approximation (this run's
// successful thumbnails minus this run's fallback events — thumbsJobHandler
// doesn't currently track "succeeded on the hardware path with zero
// fallbacks" as its own per-thumbnail counter, and fallback events are
// counted job-wide across both the thumbnail and sprite passes) rather than
// an exact hardware-success count; still accurate enough to show whether
// hardware decoding is doing anything at all. When hwMode is already
// software there's no "hardware success" to count, so hwOK contributes 0
// regardless of doneCount. Best-effort: a failure to read/write the
// setting is logged, not returned, since it must never fail the job whose
// thumbnails already generated successfully.
func accumulateThumbHWStats(ctx context.Context, store *db.DB, hwMode media.Mode, doneCount, fallbacks, unreadable int) {
	var hwOK int64
	if hwMode != media.ModeSoftware {
		hwOK = int64(doneCount - fallbacks)
		if hwOK < 0 {
			hwOK = 0
		}
	}

	var stats thumbHWStats
	if raw, err := store.SettingsGet(ctx, thumbHWStatsSettingsKey); err == nil {
		if err := json.Unmarshal(raw, &stats); err != nil {
			log.Printf("[jobs] thumb hw stats: decode existing: %v", err)
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		log.Printf("[jobs] thumb hw stats: read existing: %v", err)
	}

	stats.LastMode = string(hwMode)
	stats.HWOk += hwOK
	stats.HWFallbacks += int64(fallbacks)
	stats.Unreadable += int64(unreadable)
	stats.LastRunAt = time.Now().UTC().Format(time.RFC3339)

	raw, err := json.Marshal(stats)
	if err != nil {
		log.Printf("[jobs] thumb hw stats: marshal: %v", err)
		return
	}
	if err := store.SettingsSet(ctx, thumbHWStatsSettingsKey, raw); err != nil {
		log.Printf("[jobs] thumb hw stats: persist: %v", err)
	}
}
