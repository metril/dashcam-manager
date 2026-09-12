package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
	"dashcam/internal/share"
)

// hashBackfillBatchSize is how many storage_files/segments rows the
// hash_backfill job pulls per query round-trip — same reasoning as
// StorageFileUpsert's own batch size: big enough to amortize query
// overhead, small enough that a long-running backfill still reports
// progress and observes ctx cancellation frequently.
// var, not const, so tests can shrink it to exercise the multi-batch loop
// (query -> process -> requery) without needing hundreds of fixture files.
var hashBackfillBatchSize = 200

// hashFileFn is ingest.HashFile, indirected through a var (same pattern as
// ssdMountedFn in jobs_runtime.go) so tests can substitute a fake that
// counts calls or cancels ctx mid-run without touching the real
// filesystem.
var hashFileFn = ingest.HashFile

// hashBackfillJobParams is the params shape for a "hash_backfill" job:
// an optional share_id restricts the backfill to one storage (used by the
// per-share auto-enqueue after a scan, and by POST /api/storages/hash when
// called with a share_id); zero means every share. limit optionally caps
// the total number of files hashed in this one run (0 = unbounded — walk
// every unhashed row this run finds forward progress on).
type hashBackfillJobParams struct {
	ShareID int64 `json:"share_id,omitempty"`
	Limit   int   `json:"limit,omitempty"`
}

// hashBackfillResult is the job's returned summary.
type hashBackfillResult struct {
	Hashed    int `json:"hashed"`
	Skipped   int `json:"skipped"`
	Remaining int `json:"remaining"`
}

// hashThrottle is a simple token-bucket rate limiter over bytes/sec,
// applied at whole-file granularity by hashBackfillJobHandler (once per
// file hashed, not mid-stream): a burst up to one second's worth of
// tokens is allowed, and wait sleeps just long enough to bring the
// average rate back down to the configured cap. Zero bytesPerSec means
// unthrottled — wait always returns immediately.
type hashThrottle struct {
	mu          sync.Mutex
	bytesPerSec float64
	tokens      float64
	last        time.Time
}

// newHashThrottle builds a hashThrottle for mbPerSec (settings.
// hash_throttle_mb_s); mbPerSec <= 0 means unthrottled.
func newHashThrottle(mbPerSec int) *hashThrottle {
	if mbPerSec <= 0 {
		return &hashThrottle{}
	}
	bps := float64(mbPerSec) * 1024 * 1024
	return &hashThrottle{bytesPerSec: bps, tokens: bps, last: time.Now()}
}

// wait consumes n bytes worth of tokens, sleeping (respecting ctx) if the
// bucket doesn't have enough — called by hashBackfillJobHandler after
// hashing each file with n = that file's size.
func (t *hashThrottle) wait(ctx context.Context, n int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if t.bytesPerSec <= 0 {
		return nil
	}
	t.mu.Lock()
	now := time.Now()
	t.tokens += now.Sub(t.last).Seconds() * t.bytesPerSec
	if t.tokens > t.bytesPerSec {
		t.tokens = t.bytesPerSec // cap the burst at one second's worth
	}
	t.last = now
	t.tokens -= float64(n)
	deficit := -t.tokens
	t.mu.Unlock()

	if deficit <= 0 {
		return nil
	}
	sleep := time.Duration(deficit / t.bytesPerSec * float64(time.Second))
	timer := time.NewTimer(sleep)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hashThrottleFromSettings reads settings.hash_throttle_mb_s, defaulting
// to 40 (matching seedDefaultSettings in main.go) when unset or invalid.
func hashThrottleFromSettings(ctx context.Context, store *db.DB) *hashThrottle {
	mb := 40
	if raw, err := store.SettingsGet(ctx, "hash_throttle_mb_s"); err == nil {
		var v int
		if json.Unmarshal(raw, &v) == nil && v >= 0 {
			mb = v
		}
	}
	return newHashThrottle(mb)
}

// shareAvailability memoizes share mount/reachability checks for the
// life of one hash_backfill run: a batch can mix rows from several
// shares, and re-probing the same share's mount for every one of its rows
// would be wasteful (and, for a cifs share, a syscall each time).
type shareAvailability struct {
	checked map[int64]bool
}

func newShareAvailability() *shareAvailability {
	return &shareAvailability{checked: make(map[int64]bool)}
}

// available reports whether shareID's mountPoint (given its kind) is
// currently usable to read from — local storages use LocalDirReadable
// (read-only probe, matching scanJobHandler's own choice for local
// sources); cifs storages use IsMounted. Mirrors the availability check
// scanJobHandler already applies before scanning a share-backed source.
func (a *shareAvailability) available(shareID int64, mountPoint, kind string) bool {
	if ok, seen := a.checked[shareID]; seen {
		return ok
	}
	ok := true
	if kind == db.KindLocal {
		ok = share.LocalDirReadable(mountPoint) == nil
	} else {
		mounted, err := share.IsMounted(mountPoint)
		ok = err == nil && mounted
	}
	a.checked[shareID] = ok
	return ok
}

// hashBackfillJobHandler computes sha256 for storage_files rows (task 5's
// scan-time index of every share/local-backed storage's files) and
// segments rows backed by a share-kind source, in batches of
// hashBackfillBatchSize, throttled by settings.hash_throttle_mb_s and
// cancellable via ctx between files. sha256 IS NULL is the resume cursor
// (StorageFilesNeedingHash/SegmentsNeedingHash), so re-enqueuing this job
// after a partial run (or a process restart) just picks up where the last
// one left off — no separate checkpoint needed.
//
// A file that vanished between being indexed and being hashed (deleted,
// or a race with a concurrent scan) is skipped, leaving its sha256 NULL —
// it will be picked up again by the next scan's storage_files upsert (if
// it reappears) or simply stay unhashed forever (if it's really gone,
// harmless: it just never counts toward a duplicate group). A share that
// isn't currently mountable/readable is skipped entirely for this run
// (its rows stay NULL) rather than erroring the whole job — the recorder,
// or an operator, may have simply unplugged it.
//
// To avoid ever spinning on a batch that makes no forward progress (every
// row in it vanished or belongs to an unavailable share), the loop stops
// once a whole batch hashes zero files.
func hashBackfillJobHandler(store *db.DB, q *jobs.Queue) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var params hashBackfillJobParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			return nil, fmt.Errorf("hash_backfill job: invalid params: %w", err)
		}

		throttle := hashThrottleFromSettings(ctx, store)
		avail := newShareAvailability()
		// A vanished file's row is never mutated (its sha256 stays NULL
		// so a future scan/rehash can still pick it up), which means it
		// keeps reappearing in every subsequent StorageFilesNeedingHash/
		// SegmentsNeedingHash batch for the rest of this run. These sets
		// make sure each such row is only opened, and only counted as
		// skipped, once per run rather than once per batch it happens to
		// land in.
		seenVanishedStorageFile := map[int64]bool{}
		seenVanishedSegment := map[int64]bool{}

		var hashed, skipped int
		// budget tracks remaining files this run may hash when
		// params.Limit > 0; unlimited stays true (and budget is simply
		// ignored) when no limit was given — 0 is not a valid "used up"
		// sentinel on its own since it's also params.Limit's zero value.
		budget := params.Limit
		unlimited := params.Limit <= 0

		reportProgress := func() {
			remaining, err := remainingHashCount(ctx, store, params.ShareID)
			if err != nil {
				return
			}
			total := int64(hashed+skipped) + remaining
			var p float64
			if total > 0 {
				p = float64(int64(hashed+skipped)) / float64(total)
				if p > 0.99 {
					p = 0.99
				}
			}
			report(p, fmt.Sprintf("hashed %d, skipped %d, remaining %d", hashed, skipped, remaining))
		}

		// Pass 1: storage_files (every share/local-backed storage's full
		// tree, task 5's scan-time index).
		for {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !unlimited && budget <= 0 {
				break
			}
			n := hashBackfillBatchSize
			if !unlimited && budget < n {
				n = budget
			}
			batch, err := store.StorageFilesNeedingHash(ctx, n, params.ShareID)
			if err != nil {
				return nil, fmt.Errorf("hash_backfill: %w", err)
			}
			if len(batch) == 0 {
				break
			}
			progressed := 0
			for _, c := range batch {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if !avail.available(c.ShareID, c.ShareMountPoint, c.ShareKind) {
					continue
				}
				if seenVanishedStorageFile[c.ID] {
					continue
				}
				path := filepath.Join(c.ShareMountPoint, c.RelPath)
				got, size, err := hashFileFn(path)
				if err != nil {
					if os.IsNotExist(err) {
						// Not forward progress: this row's sha256 stays
						// NULL forever, so it will be right back in the
						// next batch's results. Only a successful hash
						// (which clears sha256 for good) may reset the
						// zero-progress counter below — otherwise a
						// storage full of vanished files would spin this
						// loop forever re-fetching the exact same rows.
						// seenVanishedStorageFile above stops it from
						// being re-opened (and re-counted) every batch.
						seenVanishedStorageFile[c.ID] = true
						skipped++
						continue
					}
					return nil, fmt.Errorf("hash_backfill: storage_files %d (%s): %w", c.ID, path, err)
				}
				if err := throttle.wait(ctx, size); err != nil {
					return nil, err
				}
				if err := store.StorageFileSetSHA256(ctx, c.ID, got, time.Now().UTC().Format(time.RFC3339)); err != nil {
					return nil, fmt.Errorf("hash_backfill: set sha256 for storage_files %d: %w", c.ID, err)
				}
				hashed++
				progressed++
				if !unlimited {
					budget--
				}
				reportProgress()
			}
			if progressed == 0 {
				break
			}
		}

		// Pass 2: segments backed directly by a share-kind source (not
		// yet mirrored into storage_files by a scan, or hashed
		// independently of the storage_files table).
		for {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !unlimited && budget <= 0 {
				break
			}
			n := hashBackfillBatchSize
			if !unlimited && budget < n {
				n = budget
			}
			batch, err := store.SegmentsNeedingHash(ctx, n, params.ShareID)
			if err != nil {
				return nil, fmt.Errorf("hash_backfill: %w", err)
			}
			if len(batch) == 0 {
				break
			}
			progressed := 0
			for _, c := range batch {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if !avail.available(c.ShareID, c.ShareMountPoint, c.ShareKind) {
					continue
				}
				if seenVanishedSegment[c.SegmentID] {
					continue
				}
				path := filepath.Join(c.RootPath, c.RelPath)
				got, size, err := hashFileFn(path)
				if err != nil {
					if os.IsNotExist(err) {
						// See the matching comment in pass 1: a vanished
						// segment's sha256 also stays NULL forever, so it
						// must not count as progress, or be re-opened and
						// re-counted, more than once per run.
						seenVanishedSegment[c.SegmentID] = true
						skipped++
						continue
					}
					return nil, fmt.Errorf("hash_backfill: segment %d (%s): %w", c.SegmentID, path, err)
				}
				if err := throttle.wait(ctx, size); err != nil {
					return nil, err
				}
				if err := store.SegmentSetSHA256(ctx, c.SegmentID, got); err != nil {
					return nil, fmt.Errorf("hash_backfill: set sha256 for segment %d: %w", c.SegmentID, err)
				}
				hashed++
				progressed++
				if !unlimited {
					budget--
				}
				reportProgress()
			}
			if progressed == 0 {
				break
			}
		}

		// A completed run (whether or not it hashed anything: fresh
		// storage_files/segments rows without a hash change may still
		// have altered the "at least one unhashed" gate a size_name
		// probable group depends on) enqueues a duplicates_refresh job so
		// GET /api/duplicates never depends on an operator remembering to
		// hit POST /api/duplicates/refresh separately. Enqueued (not run
		// inline) so this handler never blocks on the rebuild. SetLimit(1)
		// only caps *concurrent* runs, not queue depth, so check
		// ExistsActive first — several shares' hash_backfills finishing
		// close together would otherwise each queue their own redundant
		// rebuild; a failed enqueue/check is logged, not fatal — the
		// hashing this run already did is real and shouldn't be
		// discarded/retried just because scheduling the report rebuild
		// had trouble.
		if active, activeErr := q.ExistsActive(ctx, "duplicates_refresh"); activeErr != nil {
			log.Printf("[jobs] hash_backfill %d: check duplicates_refresh active: %v", job.ID, activeErr)
		} else if !active {
			if _, enqErr := q.Enqueue(ctx, "duplicates_refresh", duplicatesRefreshJobParams{}, -5); enqErr != nil {
				log.Printf("[jobs] hash_backfill %d: enqueue duplicates_refresh: %v", job.ID, enqErr)
			}
		}

		remaining, _ := remainingHashCount(ctx, store, params.ShareID)
		return hashBackfillResult{Hashed: hashed, Skipped: skipped, Remaining: int(remaining)}, nil
	}
}

// remainingHashCount sums StorageFilesNeedingHashCount and
// SegmentsNeedingHashCount for shareID (0 = every share), used both for
// the job's final result and its per-file progress reports.
func remainingHashCount(ctx context.Context, store *db.DB, shareID int64) (int64, error) {
	a, err := store.StorageFilesNeedingHashCount(ctx, shareID)
	if err != nil {
		return 0, err
	}
	b, err := store.SegmentsNeedingHashCount(ctx, shareID)
	if err != nil {
		return 0, err
	}
	return a + b, nil
}
