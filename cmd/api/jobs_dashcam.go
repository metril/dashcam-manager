package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
)

// dashcamReprocessPollInterval is how often dashcam_reprocess polls
// ReprocessStatus while a run is active. A var, not a const, so a test can
// shrink it to bound how long a reprocess test takes.
var dashcamReprocessPollInterval = 5 * time.Second

// dashcamReprocessLeaseTTL is the maintenance lease dashcam_reprocess
// acquires for the duration of a reprocess run — generous enough to cover
// a slow full-day reprocess without lapsing, renewed well before expiry by
// the Keeper regardless (see dashcam.Client.KeepLease).
const dashcamReprocessLeaseTTL = 15 * time.Minute

// dashcamPullManifestPageSize bounds how many segments a single
// GET /api/ingest/manifest page asks for, matching the brief's "500".
const dashcamPullManifestPageSize = 500

// dashcamDeleteBatchSize bounds how many paths one POST /api/ingest/delete
// call names, matching resources/ingest_server.py's own per-request limit.
const dashcamDeleteBatchSize = 200

// dashcamPullJobParams is the params shape for a "dashcam_pull" job.
type dashcamPullJobParams struct {
	SourceID int64 `json:"source_id"`
	// Full, when true, ignores the source's saved pull_cursor and walks
	// the dashcam's entire manifest from the beginning — for a first pull
	// or a deliberate re-sync, mirroring scanJobParams.Full.
	Full bool `json:"full"`
}

// dashcamCleanupJobParams is the params shape for a "dashcam_cleanup" job.
// DryRun is a pointer (not a plain bool) so an absent/omitted field can
// default to true — the safe direction — rather than to false the way a
// plain bool's zero value would; a caller must explicitly pass
// dry_run:false to actually delete anything off the dashcam.
type dashcamCleanupJobParams struct {
	SourceID int64 `json:"source_id"`
	DryRun   *bool `json:"dry_run,omitempty"`
}

// dashcamReprocessJobParams is the params shape for a "dashcam_reprocess"
// job. Exactly one scope must be given: camera+date, path, or segment.
type dashcamReprocessJobParams struct {
	SourceID    int64  `json:"source_id"`
	Camera      string `json:"camera,omitempty"`
	Date        string `json:"date,omitempty"`
	Path        string `json:"path,omitempty"`
	Segment     string `json:"segment,omitempty"`
	OnlyMissing bool   `json:"only_missing,omitempty"`
	SkipFrames  bool   `json:"skip_frames,omitempty"`
}

func (p dashcamReprocessJobParams) validate() error {
	if p.Camera != "" && p.Date != "" {
		return nil
	}
	if p.Path != "" || p.Segment != "" {
		return nil
	}
	return fmt.Errorf("dashcam_reprocess job: need camera+date, path, or segment to scope the reprocess")
}

// registerDashcamJobHandlers wires the three dashcam_* job types into q.
// Called from registerJobHandlers, which already has cfg/store in scope.
func registerDashcamJobHandlers(q *jobs.Queue, store *db.DB, secretKey string) {
	q.RegisterHandler("dashcam_pull", dashcamPullJobHandler(store, q, secretKey))
	q.SetLimit("dashcam_pull", 1)
	q.RegisterHandler("dashcam_cleanup", dashcamCleanupJobHandler(store, secretKey))
	q.SetLimit("dashcam_cleanup", 1)
	q.RegisterHandler("dashcam_reprocess", dashcamReprocessJobHandler(store, q, secretKey))
	q.SetLimit("dashcam_reprocess", 1)
}

// dashcamClientFor loads sourceID's kind='dashcam' Source row and builds a
// signed client for it. Fails with a clear, user-facing message if no
// signing key has been generated/saved yet (see dashcam.LoadKey) — every
// dashcam_* job needs one before it can make a single request.
func dashcamClientFor(ctx context.Context, store *db.DB, secretKey string, sourceID int64) (db.Source, *dashcam.Client, error) {
	src, err := store.DashcamGet(ctx, sourceID)
	if err != nil {
		return db.Source{}, nil, fmt.Errorf("dashcam source %d: %w", sourceID, err)
	}
	keyID, priv, ok, err := dashcam.LoadKey(ctx, store, secretKey)
	if err != nil {
		return db.Source{}, nil, fmt.Errorf("load dashcam key: %w", err)
	}
	if !ok {
		return db.Source{}, nil, fmt.Errorf("dashcam key not configured")
	}
	baseURL := ""
	if src.BaseURL != nil {
		baseURL = *src.BaseURL
	}
	return src, dashcam.New(baseURL, keyID, priv), nil
}

// dashcamTargetShare resolves src's configured pull target: the share's
// mounted root (via ingest.ResolveTargetMount, so an unmounted/misconfigured
// share fails the job the same way it would fail a scan or ingest) and the
// kind='share' source row for that share, which the pull/reprocess jobs
// need in order to enqueue a "scan" of the destination and to link
// dashcam_pulls rows to the segments that scan indexes.
func dashcamTargetShare(ctx context.Context, store *db.DB, src db.Source) (root string, shareSrc db.Source, err error) {
	if src.TargetShareID == nil {
		return "", db.Source{}, fmt.Errorf("dashcam source %d has no target share configured", src.ID)
	}
	root, err = ingest.ResolveTargetMount(ctx, store, *src.TargetShareID)
	if err != nil {
		return "", db.Source{}, err
	}
	shareSrc, err = store.SourceGetByShareID(ctx, *src.TargetShareID)
	if err != nil {
		return "", db.Source{}, fmt.Errorf("target share %d source row: %w", *src.TargetShareID, err)
	}
	return root, shareSrc, nil
}

// dashcamDestPath maps a dashcam-reported rel path (a manifest segment's
// Path, or a sidecar's SidecarRelPath) onto a local destination under
// root, refusing anything that isn't a clean, relative, contained path —
// the same defense-in-depth cleanup.go and scan's walker apply to paths
// that ultimately come from outside this process.
func dashcamDestPath(root, relPath string) (string, error) {
	if !filepath.IsLocal(relPath) {
		return "", fmt.Errorf("unsafe remote path %q", relPath)
	}
	return filepath.Join(root, filepath.FromSlash(relPath)), nil
}

// downloadSegmentSidecars downloads every sidecar named in seg.Sidecars to
// its mapped destination under root (same rel-path scheme as the segment
// itself — see dashcamDestPath).
func downloadSegmentSidecars(ctx context.Context, client *dashcam.Client, root string, seg dashcam.Segment) error {
	for _, sidecarURL := range seg.Sidecars {
		rel, err := dashcam.SidecarRelPath(sidecarURL)
		if err != nil {
			return err
		}
		dest, err := dashcamDestPath(root, rel)
		if err != nil {
			return err
		}
		if _, err := client.Download(ctx, rel, dest); err != nil {
			return fmt.Errorf("download sidecar %s: %w", rel, err)
		}
	}
	return nil
}

// dashcamPullJobHandler pulls every new-or-changed segment (since the
// source's saved pull_cursor, or from the very beginning when params.Full)
// from a dashcam onto its configured target share, then enqueues a "scan"
// of that share so the pulled files get indexed the normal way.
func dashcamPullJobHandler(store *db.DB, q *jobs.Queue, secretKey string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params dashcamPullJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("dashcam_pull job: invalid params: %w", err)
		}

		src, client, err := dashcamClientFor(ctx, store, secretKey, params.SourceID)
		if err != nil {
			return nil, fmt.Errorf("dashcam_pull job: %w", err)
		}
		root, shareSrc, err := dashcamTargetShare(ctx, store, src)
		if err != nil {
			return nil, fmt.Errorf("dashcam_pull job: %w", err)
		}

		// Cheap and idempotent — the scan that would newly link any of
		// this run's pulls hasn't run yet (it's enqueued below, and runs
		// asynchronously), but an *earlier* pull's segments may only just
		// have become linkable since the last time this ran.
		if _, err := store.PullsLinkSegments(ctx, src.ID, shareSrc.ID); err != nil {
			log.Printf("[jobs] dashcam_pull %d: link segments: %v", job.ID, err)
		}

		since := ""
		if !params.Full && src.PullCursor != nil {
			since = *src.PullCursor
		}

		var pulled, skipped int
		var bytesTotal int64

		persistCursor := func(cursor string) error {
			return store.DashcamSetCursor(context.Background(), src.ID, cursor, time.Now())
		}

		finalCursor, err := client.EachSegmentPaged(ctx, since, "", dashcamPullManifestPageSize, persistCursor, func(seg dashcam.Segment) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			dest, err := dashcamDestPath(root, seg.Path)
			if err != nil {
				return fmt.Errorf("segment %s: %w", seg.Path, err)
			}

			if existing, found, err := store.PullGet(ctx, src.ID, seg.Path); err != nil {
				return err
			} else if found && existing.VerifiedAt != nil && existing.Bytes == seg.Size {
				if info, statErr := os.Stat(dest); statErr == nil && info.Size() == seg.Size {
					skipped++
					report(0, fmt.Sprintf("skipped %d, pulled %d", skipped, pulled))
					return nil
				}
			}

			dlRes, err := client.Download(ctx, seg.Path, dest)
			if err != nil {
				return fmt.Errorf("download %s: %w", seg.Path, err)
			}
			if err := downloadSegmentSidecars(ctx, client, root, seg); err != nil {
				return fmt.Errorf("segment %s: %w", seg.Path, err)
			}

			sha := dlRes.SHA256
			mtime := seg.MTime
			id, err := store.PullUpsert(ctx, db.DashcamPull{
				DashcamSourceID: src.ID, RemoteRelPath: seg.Path, ShareID: *src.TargetShareID,
				DestRelPath: seg.Path, Bytes: dlRes.Bytes, SHA256: &sha, Mtime: &mtime,
				PulledAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
			if err != nil {
				return err
			}
			if err := store.PullMarkVerified(ctx, id, dlRes.SHA256, time.Now()); err != nil {
				return err
			}

			pulled++
			bytesTotal += dlRes.Bytes
			report(0, fmt.Sprintf("pulled %d, skipped %d (%s)", pulled, skipped, formatBytesShort(bytesTotal)))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("dashcam_pull job: %w", err)
		}

		if err := store.DashcamSetCursor(context.Background(), src.ID, finalCursor, time.Now()); err != nil {
			log.Printf("[jobs] dashcam_pull %d: set cursor: %v", job.ID, err)
		}

		var scanJobID int64
		if scanJob, enqErr := q.Enqueue(context.Background(), "scan", scanJobParams{SourceID: shareSrc.ID}, -1); enqErr != nil {
			log.Printf("[jobs] dashcam_pull %d: enqueue scan of share source: %v", job.ID, enqErr)
		} else {
			scanJobID = scanJob.ID
		}

		return map[string]any{
			"pulled": pulled, "skipped": skipped, "bytes": bytesTotal,
			"cursor": finalCursor, "scan_job_id": scanJobID,
		}, nil
	}
}

// dashcamCleanupJobHandler deletes remote copies off the dashcam for
// pulls that are verified, linked to an indexed segment, and whose local
// copy under the target share still exists with the matching size — the
// same three-part safety guard ingest.Cleanup applies to an SSD original,
// applied here to a dashcam's own storage instead. dry_run (the default)
// only reports what would be deleted.
func dashcamCleanupJobHandler(store *db.DB, secretKey string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params dashcamCleanupJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("dashcam_cleanup job: invalid params: %w", err)
		}
		dryRun := true
		if params.DryRun != nil {
			dryRun = *params.DryRun
		}

		src, client, err := dashcamClientFor(ctx, store, secretKey, params.SourceID)
		if err != nil {
			return nil, fmt.Errorf("dashcam_cleanup job: %w", err)
		}
		root, shareSrc, err := dashcamTargetShare(ctx, store, src)
		if err != nil {
			return nil, fmt.Errorf("dashcam_cleanup job: %w", err)
		}

		if _, err := store.PullsLinkSegments(ctx, src.ID, shareSrc.ID); err != nil {
			log.Printf("[jobs] dashcam_cleanup %d: link segments: %v", job.ID, err)
		}

		candidates, err := store.PullCleanupCandidates(ctx, src.ID)
		if err != nil {
			return nil, fmt.Errorf("dashcam_cleanup job: %w", err)
		}

		var eligible []db.DashcamPull
		var candidateBytes int64
		for _, p := range candidates {
			dest, err := dashcamDestPath(root, p.DestRelPath)
			if err != nil {
				continue
			}
			info, statErr := os.Stat(dest)
			if statErr != nil || info.Size() != p.Bytes {
				continue
			}
			eligible = append(eligible, p)
			candidateBytes += p.Bytes
		}

		if dryRun {
			return map[string]any{"candidates": len(eligible), "bytes": candidateBytes}, nil
		}

		byPath := make(map[string]db.DashcamPull, len(eligible))
		for _, p := range eligible {
			byPath[p.RemoteRelPath] = p
		}

		var deletedIDs []int64
		var deletedBytes int64
		var skipped []dashcam.SkippedPath
		for i := 0; i < len(eligible); i += dashcamDeleteBatchSize {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			end := i + dashcamDeleteBatchSize
			if end > len(eligible) {
				end = len(eligible)
			}
			batch := eligible[i:end]

			paths := make([]string, len(batch))
			sha := make(map[string]string, len(batch))
			for j, p := range batch {
				paths[j] = p.RemoteRelPath
				if p.SHA256 != nil {
					sha[p.RemoteRelPath] = *p.SHA256
				}
			}

			res, err := client.Delete(ctx, paths, sha)
			if err != nil {
				return nil, fmt.Errorf("dashcam_cleanup job: delete: %w", err)
			}
			for _, path := range res.Deleted {
				if p, ok := byPath[path]; ok {
					deletedIDs = append(deletedIDs, p.ID)
					deletedBytes += p.Bytes
				}
			}
			skipped = append(skipped, res.Skipped...)
			report(float64(end)/float64(len(eligible)), fmt.Sprintf("deleted %d/%d", end, len(eligible)))
		}

		if err := store.PullMarkDeletedRemote(ctx, deletedIDs, time.Now()); err != nil {
			return nil, fmt.Errorf("dashcam_cleanup job: %w", err)
		}

		return map[string]any{
			"deleted": len(deletedIDs), "skipped": skipped, "bytes": deletedBytes,
		}, nil
	}
}

// dashcamReprocess starts and drives a recorder-side reprocess run behind
// a maintenance lease, then re-pulls whatever segments/sidecars it
// touched.
func dashcamReprocessJobHandler(store *db.DB, q *jobs.Queue, secretKey string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params dashcamReprocessJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: invalid params: %w", err)
		}
		if err := params.validate(); err != nil {
			return nil, err
		}

		src, client, err := dashcamClientFor(ctx, store, secretKey, params.SourceID)
		if err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: %w", err)
		}
		root, shareSrc, err := dashcamTargetShare(ctx, store, src)
		if err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: %w", err)
		}

		lease, err := client.AcquireLease(ctx, dashcamReprocessLeaseTTL, "dashcam-manager reprocess")
		if err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: acquire lease: %w", err)
		}
		keeper := client.KeepLease(ctx, lease, dashcamReprocessLeaseTTL)
		defer func() { _ = keeper.Close(false) }()

		tracking := true
		if _, err := client.StartReprocess(ctx, dashcam.ReprocessRequest{
			Camera: params.Camera, Date: params.Date, Path: params.Path, Segment: params.Segment,
			OnlyMissing: params.OnlyMissing, SkipFrames: params.SkipFrames, Tracking: &tracking,
		}); err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: start reprocess: %w", err)
		}

		var lastNonIdle dashcam.ReprocessStatus
		ticker := time.NewTicker(dashcamReprocessPollInterval)
		defer ticker.Stop()
	pollLoop:
		for {
			select {
			case <-ctx.Done():
				_ = keeper.Close(true)
				return nil, ctx.Err()
			case <-ticker.C:
				st, err := client.ReprocessStatus(ctx)
				if err != nil {
					return nil, fmt.Errorf("dashcam_reprocess job: status: %w", err)
				}
				if st.State == "idle" {
					break pollLoop
				}
				lastNonIdle = st
				if st.TotalSegments > 0 {
					report(float64(st.Processed)/float64(st.TotalSegments),
						fmt.Sprintf("%d/%d segments", st.Processed, st.TotalSegments))
				}
			}
		}

		if lastNonIdle.Error != "" {
			return nil, fmt.Errorf("dashcam_reprocess job: reprocess failed: %s", lastNonIdle.Error)
		}

		segmentsReprocessed := 0
		sidecarsRefreshed := 0
		_, err = client.EachSegment(ctx, "", params.Camera, dashcamPullManifestPageSize, func(seg dashcam.Segment) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !dashcamReprocessScopeMatches(params, seg) {
				return nil
			}

			dest, err := dashcamDestPath(root, seg.Path)
			if err != nil {
				return fmt.Errorf("segment %s: %w", seg.Path, err)
			}

			existing, found, err := store.PullGet(ctx, src.ID, seg.Path)
			if err != nil {
				return err
			}

			bytesVal := seg.Size
			var shaVal *string
			if found {
				shaVal = existing.SHA256
			}
			if !found || existing.Bytes != seg.Size {
				dlRes, err := client.Download(ctx, seg.Path, dest)
				if err != nil {
					return fmt.Errorf("download %s: %w", seg.Path, err)
				}
				bytesVal = dlRes.Bytes
				sha := dlRes.SHA256
				shaVal = &sha
			}

			if err := downloadSegmentSidecars(ctx, client, root, seg); err != nil {
				return fmt.Errorf("segment %s: %w", seg.Path, err)
			}
			sidecarsRefreshed += len(seg.Sidecars)

			mtime := seg.MTime
			id, err := store.PullUpsert(ctx, db.DashcamPull{
				DashcamSourceID: src.ID, RemoteRelPath: seg.Path, ShareID: *src.TargetShareID,
				DestRelPath: seg.Path, Bytes: bytesVal, SHA256: shaVal, Mtime: &mtime,
				PulledAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
			if err != nil {
				return err
			}
			verifySHA := ""
			if shaVal != nil {
				verifySHA = *shaVal
			}
			if err := store.PullMarkVerified(ctx, id, verifySHA, time.Now()); err != nil {
				return err
			}

			segmentsReprocessed++
			report(0, fmt.Sprintf("re-pulled %d segments", segmentsReprocessed))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: re-pull: %w", err)
		}

		var scanJobID int64
		if scanJob, enqErr := q.Enqueue(context.Background(), "scan", scanJobParams{SourceID: shareSrc.ID}, -1); enqErr != nil {
			log.Printf("[jobs] dashcam_reprocess %d: enqueue scan of share source: %v", job.ID, enqErr)
		} else {
			scanJobID = scanJob.ID
		}

		return map[string]any{
			"segments_reprocessed": segmentsReprocessed,
			"sidecars_refreshed":   sidecarsRefreshed,
			"scan_job_id":          scanJobID,
			"total_objects":        lastNonIdle.TotalObjects,
			"total_plates":         lastNonIdle.TotalPlates,
			"total_vehicles":       lastNonIdle.TotalVehicles,
			"total_track_events":   lastNonIdle.TotalTrackEvents,
		}, nil
	}
}

// dashcamReprocessScopeMatches reports whether seg falls within the scope
// params.validate() has already confirmed is non-empty: an exact path
// match, a substring match against the segment identifier ("segment"), or
// a same-day match against the segment's start_time (date, only combined
// with camera — which EachSegment already filtered server-side).
func dashcamReprocessScopeMatches(params dashcamReprocessJobParams, seg dashcam.Segment) bool {
	switch {
	case params.Path != "":
		return seg.Path == params.Path
	case params.Segment != "":
		return filepath.Base(seg.Path) == params.Segment || seg.Path == params.Segment
	case params.Date != "":
		return len(seg.StartTime) >= 10 && seg.StartTime[:10] == params.Date
	default:
		return false
	}
}
