package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
)

// errDashcamKeyNotConfigured is dashcamClientFor's sentinel for "no
// dashcam signing key has been generated/saved yet" — a stable value
// handlers can compare with errors.Is to report a machine-readable
// "key_not_configured" API error code instead of this error's prose.
var errDashcamKeyNotConfigured = errors.New("dashcam key not configured")

// errDashcamReprocessStopWalk is a sentinel dashcam_reprocess's re-pull fn
// returns to end EachSegment's walk early once a camera+date scoped run has
// walked past the target date — not a real error, so the caller treats it
// the same as a clean, unbroken walk (see dashcamReprocessJobHandler).
var errDashcamReprocessStopWalk = errors.New("dashcam_reprocess: stop walk")

// dashcamReprocessPollInterval is how often dashcam_reprocess polls
// ReprocessStatus while a run is active. A var, not a const, so a test can
// shrink it to bound how long a reprocess test takes.
var dashcamReprocessPollInterval = 5 * time.Second

// dashcamReprocessStatusMaxFailures bounds how many *consecutive*
// ReprocessStatus poll failures dashcam_reprocess tolerates before giving
// up — a transient error (network blip, momentary 5xx) must not kill an
// otherwise-healthy reprocess run.
const dashcamReprocessStatusMaxFailures = 5

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
// A var, not a const, so a test can shrink it to exercise multi-batch
// behavior (e.g. an earlier batch's deletions surviving a later batch's
// failure) without seeding 200+ fixture segments.
var dashcamDeleteBatchSize = 200

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

// recorderScopePath rewrites a manifest-relative scope path (path/segment,
// as entered by manager users, e.g.
// "front/2026/09/11/recording_20260911_092630/segment_00000.mp4") into the
// absolute "/recordings/..." form the real recorder expects — see
// resources/dashboard.py's start_reprocess (~3024-3110), which only
// rewrites values already starting with "/recordings/" into
// "<output_folder>/<rest>" and otherwise treats `segment`/`path` as
// filesystem paths it cannot resolve on its own. p is left unchanged when
// empty or already absolute (starts with "/", which covers both a bare
// "/recordings/..." value and any other absolute path a caller passed
// deliberately).
func recorderScopePath(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return "/recordings/" + path.Clean(p)
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
		return db.Source{}, nil, errDashcamKeyNotConfigured
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

// segmentSidecarsPresent reports whether every sidecar seg.Sidecars names
// already exists locally under root — used by the pull job's skip
// fast-path, which must not skip re-downloading a segment just because its
// mp4 already verified if a sidecar the manifest now lists (e.g. a
// .detections.json the recorder finished writing after the mp4 was already
// pulled) hasn't landed locally yet.
func segmentSidecarsPresent(root string, seg dashcam.Segment) bool {
	for _, sidecarURL := range seg.Sidecars {
		rel, err := dashcam.SidecarRelPath(sidecarURL)
		if err != nil {
			return false
		}
		dest, err := dashcamDestPath(root, rel)
		if err != nil {
			return false
		}
		if _, err := os.Stat(dest); err != nil {
			return false
		}
	}
	return true
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

// recorderTimeLayouts lists every timestamp layout the recorder's own
// start_time field (and, historically, the pull_cursor derived from it)
// is known to appear in: a naive isoformat() datetime with no zone (the
// common case — see resources/metadata.py) and, defensively, RFC3339
// with/without a fractional second in case a future recorder version
// adds one.
var recorderTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999",
	"2006-01-02T15:04:05",
}

// parseRecorderTime parses s (a recorder start_time-shaped string) against
// every layout in recorderTimeLayouts, returning both the parsed time and
// the layout that matched — a caller deriving a new timestamp from it
// (e.g. rewindCursorFor24hLateSidecars' rewound cursor, or the settle
// cutoff below) must format the result back using that same layout, since
// the recorder/manifest compares these lexically (plain SQL/string TEXT
// comparison, not a parsed-datetime comparison) and a differently-shaped
// string would compare incorrectly against the recorder's own values.
func parseRecorderTime(s string) (t time.Time, layout string, err error) {
	for _, l := range recorderTimeLayouts {
		if t, err = time.Parse(l, s); err == nil {
			return t, l, nil
		}
	}
	return time.Time{}, "", fmt.Errorf("dashcam: unparseable recorder timestamp %q", s)
}

// rewindCursorFor24hLateSidecars rewinds a persisted pull cursor by 24h
// so dashcam_pull's incremental manifest walk re-lists segments whose
// sidecars (.detections.json/.gps.json) may have been written by the
// recorder up to 24h after the segment itself was first listed —
// has_detections/has_gps flip from 0 to 1 on a later manifest page for
// the same segment once the recorder's analysis finishes (see
// resources/ingest.py/metadata.py). The existing dedupe (PullGet finding
// an already-verified row at the same size) plus the existing sidecar-
// presence re-check (segmentSidecarsPresent) mean rewinding just costs
// re-examining a day's worth of already-pulled segments, not
// re-downloading them. Returns "" (a full listing) if cursor doesn't
// parse as a recognizable start_time.
func rewindCursorFor24hLateSidecars(cursor string) string {
	if cursor == "" {
		return ""
	}
	t, layout, err := parseRecorderTime(cursor)
	if err != nil {
		return ""
	}
	return t.Add(-24 * time.Hour).Format(layout)
}

// intOrZero coalesces a nullable wire int (dashcam.VehicleTrip's MIL/
// HardBrakes/HardAccels, which decode as nil for an older recorder that
// never populated them) to 0, matching the DB columns' NOT NULL DEFAULT 0.
func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// dashcamVehicleTripToDB converts one client-decoded dashcam.VehicleTrip
// into the internal/db shape VehicleTripsUpsert stores, keyed on the
// recorder's own numeric id (stringified — vehicle_trips.remote_trip_id is
// TEXT, matching vehicle_events.remote_trip_id's shape).
func dashcamVehicleTripToDB(t dashcam.VehicleTrip) db.VehicleTrip {
	return db.VehicleTrip{
		RemoteTripID: strconv.FormatInt(t.ID, 10),
		StartTime:    t.StartTime, EndTime: t.EndTime,
		StartT: t.StartT, EndT: t.EndT,
		DistanceM: t.DistanceM, FuelL: t.FuelL,
		FuelPctStart: t.FuelPctStart, FuelPctEnd: t.FuelPctEnd,
		MaxSpeedKph: t.MaxSpeedKph, AvgSpeedKph: t.AvgSpeedKph,
		BattMinV: t.BattMinV, BattMaxV: t.BattMaxV, CoolantMaxC: t.CoolantMaxC,
		SampleCount: t.SampleCount, DTCs: t.DTCs,
		MIL: intOrZero(t.MIL), HardBrakes: intOrZero(t.HardBrakes), HardAccels: intOrZero(t.HardAccels),
		Speeding: t.Speeding, Idling: t.Idling, Overheat: t.Overheat, ChargingFault: t.ChargingFault,
		ColdHard: t.ColdHard, Cornering: t.Cornering, Stall: t.Stall,
	}
}

// dashcamVehicleEventToDB converts one client-decoded dashcam.VehicleEvent
// into the internal/db shape VehicleEventsUpsert stores. remote_trip_id is
// "" (stored SQL NULL) when the event's TripID wasn't set on the wire —
// VehicleEventsUpsert re-resolves the vehicle_trip_id FK against it on
// every upsert, so a later re-pull backfills it once the trip lands too.
func dashcamVehicleEventToDB(e dashcam.VehicleEvent) db.VehicleEvent {
	remoteTripID := ""
	if e.TripID != nil {
		remoteTripID = strconv.FormatInt(*e.TripID, 10)
	}
	return db.VehicleEvent{
		RemoteEventID: strconv.FormatInt(e.ID, 10),
		EventType:     e.EventType, WallTime: e.WallTime,
		T: e.T, DurationSec: e.DurationSec, Peak: e.Peak, SpeedKph: e.SpeedKph, RPM: e.RPM,
		RoadName: e.RoadName, LimitKph: e.LimitKph, RemoteTripID: remoteTripID,
	}
}

// pullVehicleData pulls new-or-changed OBD vehicle trips/events off
// dashcamSourceID since its saved vehicle_trips_cursor/
// vehicle_events_cursor, upserts them, persists the new cursors, and links
// any still-unlinked vehicle_events against shareSourceID's segments/trips
// (VehicleEventsLink). since_id is deliberately rewound from the saved
// cursor — cursor-20 for trips, cursor-500 for events — so a tail
// re-fetch picks up a trip's remote_trip_id landing on an event after the
// event itself was already pulled, and a trip summary the recorder
// finishes writing after its own row was first listed (same idea as
// dashcamPullJobHandler's rewindCursorFor24hLateSidecars, just id-based
// instead of time-based since these list endpoints page by id).
//
// A 404 (dashcam.IsNotFound) — an older recorder firmware without the
// vehicle-telemetry routes — is tolerated: logged, cursors left
// untouched, and the pull otherwise proceeds normally. Any other error
// fails the caller the same way a manifest/download error does.
// maxCursor returns whichever of old (a possibly-nil previously persisted
// cursor) and seen (the last id actually observed this run) is larger, so
// a persisted vehicle_trips_cursor/vehicle_events_cursor can never
// regress — e.g. if the recorder's tail-rewound listing (since_id =
// cursor-20/cursor-500) happens to come back short of one seen on a
// previous run (a since-deleted row, a flaky response, ...), the cursor
// stays at its highest-ever value rather than walking backward and
// re-widening what a later run considers "new".
func maxCursor(old *int64, seen int64) *int64 {
	if old != nil && *old > seen {
		v := *old
		return &v
	}
	v := seen
	return &v
}

func pullVehicleData(ctx context.Context, store *db.DB, client *dashcam.Client, jobID, dashcamSourceID, shareSourceID int64) (trips, events int, err error) {
	tripsCursor, eventsCursor, err := store.DashcamGetVehicleCursors(ctx, dashcamSourceID)
	if err != nil {
		return 0, 0, fmt.Errorf("vehicle cursors: %w", err)
	}

	var sinceTrips int64
	if tripsCursor != nil {
		sinceTrips = *tripsCursor - 20
		if sinceTrips < 0 {
			sinceTrips = 0
		}
	}
	var sinceEvents int64
	if eventsCursor != nil {
		sinceEvents = *eventsCursor - 500
		if sinceEvents < 0 {
			sinceEvents = 0
		}
	}

	// Trips/events are accumulated in memory across the whole pull (up to
	// ~500/pull per the tail re-fetch window) and upserted in one Tx each
	// (VehicleTripsUpsertBulk/VehicleEventsUpsertBulk) rather than one Tx
	// per row — cursors are only persisted below once both bulk upserts
	// have actually committed.
	var pulledTrips []db.VehicleTrip
	newTripsCursor := tripsCursor
	lastTripID, tripsErr := client.EachVehicleTrip(ctx, sinceTrips, func(t dashcam.VehicleTrip) error {
		pulledTrips = append(pulledTrips, dashcamVehicleTripToDB(t))
		return nil
	})
	if tripsErr != nil && !dashcam.IsNotFound(tripsErr) {
		return trips, events, fmt.Errorf("vehicle trips: %w", tripsErr)
	}
	if len(pulledTrips) > 0 {
		if _, err := store.VehicleTripsUpsertBulk(ctx, dashcamSourceID, pulledTrips); err != nil {
			return trips, events, fmt.Errorf("vehicle trips: %w", err)
		}
		trips = len(pulledTrips)
		newTripsCursor = maxCursor(tripsCursor, lastTripID)
	}
	if tripsErr != nil {
		log.Printf("[jobs] dashcam_pull %d: vehicle trips not supported by recorder: %v", jobID, tripsErr)
	}

	var pulledEvents []db.VehicleEvent
	newEventsCursor := eventsCursor
	lastEventID, eventsErr := client.EachVehicleEvent(ctx, sinceEvents, func(e dashcam.VehicleEvent) error {
		pulledEvents = append(pulledEvents, dashcamVehicleEventToDB(e))
		return nil
	})
	if eventsErr != nil && !dashcam.IsNotFound(eventsErr) {
		return trips, events, fmt.Errorf("vehicle events: %w", eventsErr)
	}
	if len(pulledEvents) > 0 {
		if _, err := store.VehicleEventsUpsertBulk(ctx, dashcamSourceID, pulledEvents); err != nil {
			return trips, events, fmt.Errorf("vehicle events: %w", err)
		}
		events = len(pulledEvents)
		newEventsCursor = maxCursor(eventsCursor, lastEventID)
	}
	if eventsErr != nil {
		log.Printf("[jobs] dashcam_pull %d: vehicle events not supported by recorder: %v", jobID, eventsErr)
	}

	if err := store.DashcamSetVehicleCursors(ctx, dashcamSourceID, newTripsCursor, newEventsCursor); err != nil {
		log.Printf("[jobs] dashcam_pull %d: set vehicle cursors: %v", jobID, err)
	}

	if _, err := store.VehicleEventsLink(ctx, dashcamSourceID, shareSourceID); err != nil {
		log.Printf("[jobs] dashcam_pull %d: link vehicle events: %v", jobID, err)
	}

	return trips, events, nil
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

		// runNow is frozen once at the start of the run and reused for
		// every last_seen_at stamp (PullUpsert/PullTouchSeen) and every
		// last_pull_at stamp (persistCursor/the final DashcamSetCursor)
		// this run makes — deliberately not a fresh time.Now() per call,
		// so PullMarkSettled's "last_seen_at = ?" match (below) reliably
		// catches every row this run touched with one exact string, and
		// formatted with db.DashcamTimestampLayout's fixed width so that
		// equality match (and any future range comparison) is safe.
		runNow := time.Now()
		seenAtStamp := runNow.UTC().Format(db.DashcamTimestampLayout)

		// newestStart/newestStartLayout track the latest (parseable)
		// seg.StartTime this run has observed, across every page — used
		// after the walk to derive PullMarkSettled's cutoff (see below).
		// Not every listed segment necessarily updates this (a
		// unparseable start_time — shouldn't happen for a real recorder,
		// but defensively skipped rather than erroring the whole pull).
		var newestStart time.Time
		var newestStartLayout string

		// Cheap and idempotent — the scan that would newly link any of
		// this run's pulls hasn't run yet (it's enqueued below, and runs
		// asynchronously), but an *earlier* pull's segments may only just
		// have become linkable since the last time this ran.
		if _, err := store.PullsLinkSegments(ctx, src.ID, shareSrc.ID); err != nil {
			log.Printf("[jobs] dashcam_pull %d: link segments: %v", job.ID, err)
		}

		since := ""
		if !params.Full && src.PullCursor != nil {
			// Rewind by 24h rather than resuming exactly where the last run
			// left off — see rewindCursorFor24hLateSidecars's doc comment
			// (final-fix-wave item 1: late sidecars lost then deleted).
			since = rewindCursorFor24hLateSidecars(*src.PullCursor)
		}

		var pulled, skipped int
		var bytesTotal int64

		persistCursor := func(cursor string) error {
			return store.DashcamSetCursor(context.Background(), src.ID, cursor, runNow)
		}

		finalCursor, err := client.EachSegmentPaged(ctx, since, "", dashcamPullManifestPageSize, persistCursor, func(seg dashcam.Segment) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			dest, err := dashcamDestPath(root, seg.Path)
			if err != nil {
				return fmt.Errorf("segment %s: %w", seg.Path, err)
			}

			// Track this run's newest observed start_time (for the
			// PullMarkSettled cutoff below) regardless of whether this
			// segment is skipped or (re)pulled — settling only cares
			// about what the recorder's manifest has shown this run.
			if t, layout, err := parseRecorderTime(seg.StartTime); err == nil && t.After(newestStart) {
				newestStart, newestStartLayout = t, layout
			}

			if existing, found, err := store.PullGet(ctx, src.ID, seg.Path); err != nil {
				return err
			} else if found && existing.VerifiedAt != nil && existing.Bytes == seg.Size {
				// The mp4 size matching a verified row isn't enough on its
				// own: a sidecar (e.g. .detections.json) can be written or
				// rewritten by the recorder *after* the mp4 already
				// verified — has_detections flipping true on a later
				// manifest page for the same segment. So only skip when
				// every sidecar the manifest currently lists for this
				// segment also already exists locally; a single missing
				// one re-downloads the whole segment (mp4 included).
				if info, statErr := os.Stat(dest); statErr == nil && info.Size() == seg.Size {
					if segmentSidecarsPresent(root, seg) {
						if err := store.PullTouchSeen(ctx, existing.ID, seenAtStamp, seg.HasDetections, seg.HasGPS, seg.StartTime); err != nil {
							return err
						}
						skipped++
						report(0, fmt.Sprintf("skipped %d, pulled %d", skipped, pulled))
						return nil
					}
					// Sidecar-only fast path: the mp4 itself is verified
					// and already present at the right size — only a
					// sidecar (e.g. .obd.json landing after the mp4 was
					// pulled) is missing, so fetch just that instead of
					// re-streaming the whole (often 10s of MB) mp4 again.
					if err := downloadSegmentSidecars(ctx, client, root, seg); err != nil {
						return fmt.Errorf("segment %s: %w", seg.Path, err)
					}
					if err := store.PullTouchSeen(ctx, existing.ID, seenAtStamp, seg.HasDetections, seg.HasGPS, seg.StartTime); err != nil {
						return err
					}
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
			startTime := seg.StartTime
			id, err := store.PullUpsert(ctx, db.DashcamPull{
				DashcamSourceID: src.ID, RemoteRelPath: seg.Path, ShareID: *src.TargetShareID,
				DestRelPath: seg.Path, Bytes: dlRes.Bytes, SHA256: &sha, Mtime: &mtime,
				PulledAt:   runNow.UTC().Format(time.RFC3339Nano),
				LastSeenAt: &seenAtStamp, RemoteHasDetections: seg.HasDetections, RemoteHasGPS: seg.HasGPS,
				RemoteStartTime: &startTime,
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

		if err := store.DashcamSetCursor(context.Background(), src.ID, finalCursor, runNow); err != nil {
			log.Printf("[jobs] dashcam_pull %d: set cursor: %v", job.ID, err)
		}

		// Settle every row this run touched (last_seen_at == seenAtStamp)
		// whose remote_start_time is old enough relative to the newest
		// start_time this run actually saw — see PullMarkSettled's doc
		// comment. A run that saw no parseable start_time at all (e.g.
		// zero segments listed) has nothing to settle. This intentionally
		// lags: a segment only becomes cleanup-eligible once the recorder
		// has produced something >=1h newer than it, which for the most
		// recently recorded segment(s) in any given run may not happen
		// until a *later* run — see the design doc's "Late sidecars"
		// section for the tradeoff this accepts.
		if !newestStart.IsZero() {
			cutoff := newestStart.Add(-time.Hour).Format(newestStartLayout)
			if _, err := store.PullMarkSettled(context.Background(), src.ID, seenAtStamp, cutoff); err != nil {
				log.Printf("[jobs] dashcam_pull %d: mark settled: %v", job.ID, err)
			}
		}

		// A vehicle-data error (a real one — 404 from an older recorder is
		// already handled inside pullVehicleData and never reaches here)
		// must not abort the job before the scan enqueue below: the files
		// this run already pulled are real and worth indexing regardless
		// of whether the vehicle-telemetry sub-step had trouble, so this
		// is logged and tolerated rather than failing the whole pull.
		vehicleTrips, vehicleEvents, vehicleErr := pullVehicleData(context.Background(), store, client, job.ID, src.ID, shareSrc.ID)
		if vehicleErr != nil {
			log.Printf("[jobs] dashcam_pull %d: vehicle data: %v", job.ID, vehicleErr)
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
			"vehicle_trips": vehicleTrips, "vehicle_events": vehicleEvents,
		}, nil
	}
}

// dashcamCleanupJobHandler deletes remote copies off the dashcam for
// pulls that are verified, linked to an indexed segment, settled (see
// PullMarkSettled — the recorder has demonstrably produced something
// materially newer, so this segment's flags are unlikely to still be in
// flux), and whose local copy under the target share still exists with
// the matching size — the same safety guard ingest.Cleanup applies to an
// SSD original, applied here to a dashcam's own storage instead, plus the
// settle requirement (final-fix-wave item 1). dry_run (the default) only
// reports what would be deleted.
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
		// localMismatch counts a PullCleanupCandidates row that DB-side
		// safety checks already cleared (verified, linked, not yet
		// deleted, settled — see PullMarkSettled) but whose local copy
		// under the target share doesn't actually back it up: the mp4
		// missing or resized since it was verified, or — final-fix-wave
		// item 1 — a sidecar the manifest last reported this file as
		// having (remote_has_detections/remote_has_gps) missing locally,
		// which would otherwise mean deleting the only copy of a
		// detections/gps sidecar the manager never actually pulled.
		// Surfaced in the result rather than silently dropped so an
		// operator can tell "nothing to clean up" apart from "some
		// candidates were refused because their local copy looks wrong".
		var localMismatch int
		for _, p := range candidates {
			dest, err := dashcamDestPath(root, p.DestRelPath)
			if err != nil {
				localMismatch++
				continue
			}
			info, statErr := os.Stat(dest)
			if statErr != nil || info.Size() != p.Bytes {
				localMismatch++
				continue
			}
			if p.RemoteHasDetections || p.RemoteHasGPS {
				base := strings.TrimSuffix(dest, ".mp4")
				missingSidecar := false
				if p.RemoteHasDetections {
					if _, err := os.Stat(base + ".detections.json"); err != nil {
						missingSidecar = true
					}
				}
				if p.RemoteHasGPS {
					if _, err := os.Stat(base + ".gps.json"); err != nil {
						missingSidecar = true
					}
				}
				if missingSidecar {
					localMismatch++
					continue
				}
			}
			eligible = append(eligible, p)
			candidateBytes += p.Bytes
		}

		if dryRun {
			return map[string]any{
				"candidates": len(eligible), "bytes": candidateBytes, "local_mismatch": localMismatch,
			}, nil
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
				// A nil SHA256 (a pull upserted before it was ever
				// verified with a hash — shouldn't happen for a
				// PullCleanupCandidates row, which requires verified_at,
				// but defend anyway) is simply omitted: Delete's sha map
				// is an optional per-path guard, and a Go nil has no JSON
				// representation worth sending as "the expected hash".
				if p.SHA256 != nil {
					sha[p.RemoteRelPath] = *p.SHA256
				}
			}

			res, err := client.Delete(ctx, paths, sha)
			if err != nil {
				return nil, fmt.Errorf("dashcam_cleanup job: delete: %w", err)
			}
			var batchDeletedIDs []int64
			for _, path := range res.Deleted {
				if p, ok := byPath[path]; ok {
					batchDeletedIDs = append(batchDeletedIDs, p.ID)
					deletedBytes += p.Bytes
				}
			}
			// Mark this batch's deletions durable immediately, not after
			// the whole loop: if ctx is cancelled (or a later batch's
			// Delete call fails) partway through, an earlier batch's
			// already-deleted-off-the-dashcam files must not be left
			// looking un-cleaned-up in dashcam_pulls (which would make a
			// retry try to delete them again, harmlessly but pointlessly,
			// or — worse — a later cleanup pass re-derive stale
			// candidates from a row that no longer has a remote file).
			if len(batchDeletedIDs) > 0 {
				// context.Background(), not ctx: this durability write
				// must land even if ctx was what got cancelled — the
				// dashcam-side deletion already happened and is
				// irreversible, so the DB record of it must not be lost
				// to the same cancellation, mirroring persistCursor above.
				if err := store.PullMarkDeletedRemote(context.Background(), batchDeletedIDs, time.Now()); err != nil {
					return nil, fmt.Errorf("dashcam_cleanup job: %w", err)
				}
			}
			deletedIDs = append(deletedIDs, batchDeletedIDs...)
			skipped = append(skipped, res.Skipped...)
			report(float64(end)/float64(len(eligible)), fmt.Sprintf("deleted %d/%d", end, len(eligible)))
		}

		return map[string]any{
			"deleted": len(deletedIDs), "skipped": skipped, "bytes": deletedBytes, "local_mismatch": localMismatch,
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
		defer func() {
			if err := keeper.Close(false); err != nil {
				log.Printf("[jobs] dashcam_reprocess %d: release lease: %v", job.ID, err)
			}
		}()

		tracking := true
		if _, err := client.StartReprocess(ctx, dashcam.ReprocessRequest{
			Camera: params.Camera, Date: params.Date,
			Path: recorderScopePath(params.Path), Segment: recorderScopePath(params.Segment),
			OnlyMissing: params.OnlyMissing, SkipFrames: params.SkipFrames, Tracking: &tracking,
		}); err != nil {
			return nil, fmt.Errorf("dashcam_reprocess job: start reprocess: %w", err)
		}

		// The real recorder (resources/dashboard.py's ReprocessHandler)
		// sets state='starting' *synchronously* inside the POST that
		// StartReprocess just made, before the background thread that
		// walks it through 'running' to a terminal 'done'/'error' even
		// starts — so by the time this call returns, a status GET can
		// never legitimately see 'idle' again. 'idle' only means "no run
		// has ever started" (self._reprocess_status is None); it is NOT
		// the terminal state reached after a run, and 'done'/'error'
		// persist until the *next* run overwrites them. So: poll
		// immediately (no wait before the first status GET — a run can
		// finish inside one poll interval for a tiny/no-op scope) and
		// stop only on a terminal 'done' or 'error', not 'idle'.
		var terminal dashcam.ReprocessStatus
		var statusFailures int
		ticker := time.NewTicker(dashcamReprocessPollInterval)
		defer ticker.Stop()
	pollLoop:
		for first := true; ; first = false {
			if !first {
				select {
				case <-ctx.Done():
					if err := keeper.Close(true); err != nil {
						log.Printf("[jobs] dashcam_reprocess %d: release lease (cancelled): %v", job.ID, err)
					}
					return nil, ctx.Err()
				case <-ticker.C:
				}
			} else if ctx.Err() != nil {
				if err := keeper.Close(true); err != nil {
					log.Printf("[jobs] dashcam_reprocess %d: release lease (cancelled): %v", job.ID, err)
				}
				return nil, ctx.Err()
			}

			st, err := client.ReprocessStatus(ctx)
			if err != nil {
				// A transient failure (network blip, a momentary 5xx)
				// must not kill an otherwise-healthy reprocess run —
				// retry up to dashcamReprocessStatusMaxFailures
				// consecutive failures, waiting a poll interval between
				// each (the ticker select above already provides that
				// wait on the next iteration). Only after that many
				// failures *in a row* does this give up; any successful
				// poll resets the counter.
				statusFailures++
				log.Printf("[jobs] dashcam_reprocess %d: status poll failed (%d/%d consecutive): %v",
					job.ID, statusFailures, dashcamReprocessStatusMaxFailures, err)
				if statusFailures >= dashcamReprocessStatusMaxFailures {
					return nil, fmt.Errorf("dashcam_reprocess job: status: %w (after %d consecutive failures)", err, statusFailures)
				}
				continue
			}
			statusFailures = 0
			switch st.State {
			case "done":
				terminal = st
				break pollLoop
			case "error":
				return nil, fmt.Errorf("dashcam_reprocess job: reprocess failed: %s", st.Error)
			case "idle":
				// Only reachable if the recorder's POST /reprocess
				// response and its status GET somehow disagree (e.g. a
				// proxy/cache serving a stale idle snapshot) — a
				// successful StartReprocess guarantees at least
				// 'starting' by the time any status GET can land.
				return nil, fmt.Errorf("dashcam_reprocess job: reprocess status reported idle unexpectedly after a successful start")
			default: // "starting", "running"
				if st.TotalSegments > 0 {
					report(float64(st.Processed)/float64(st.TotalSegments),
						fmt.Sprintf("%s: %d/%d segments", st.State, st.Processed, st.TotalSegments))
				}
			}
		}

		// Release the lease now, rather than waiting for the re-pull phase
		// below to finish: the recorder has already reached a terminal
		// state, and re-pulling only needs the plain file/manifest
		// endpoints, not the lease. Holding it through a large manifest
		// walk (the recorder's full manifest can be tens of thousands of
		// segments) would otherwise keep recording paused on the car for
		// minutes after the actual reprocess work finished — see this
		// fix's motivating report. The deferred Close above is idempotent
		// (Keeper.Close's sync.Once) and stays as a safety net; ctx
		// cancellation during the re-pull below needs no force-release
		// since the lease is already gone by then.
		if err := keeper.Close(false); err != nil {
			log.Printf("[jobs] dashcam_reprocess %d: release lease: %v", job.ID, err)
		}

		camera := dashcamReprocessScopeCamera(params)
		since := dashcamReprocessScopeSince(params)

		segmentsReprocessed := 0
		sidecarsRefreshed := 0
		_, err = client.EachSegment(ctx, since, camera, dashcamPullManifestPageSize, func(seg dashcam.Segment) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !dashcamReprocessScopeMatches(params, seg) {
				// A camera+date scoped run walks the manifest in ascending
				// start_time order (see EachSegment's paging); once a
				// segment's start_time is past the target date there is
				// nothing further to find, so stop paging instead of
				// walking the rest of the manifest just to filter it all
				// out.
				if params.Camera != "" && params.Date != "" &&
					len(seg.StartTime) >= 10 && seg.StartTime[:10] > params.Date {
					return errDashcamReprocessStopWalk
				}
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
			startTime := seg.StartTime
			seenAt := time.Now().UTC().Format(db.DashcamTimestampLayout)
			id, err := store.PullUpsert(ctx, db.DashcamPull{
				DashcamSourceID: src.ID, RemoteRelPath: seg.Path, ShareID: *src.TargetShareID,
				DestRelPath: seg.Path, Bytes: bytesVal, SHA256: shaVal, Mtime: &mtime,
				PulledAt:   time.Now().UTC().Format(time.RFC3339Nano),
				LastSeenAt: &seenAt, RemoteHasDetections: seg.HasDetections, RemoteHasGPS: seg.HasGPS,
				RemoteStartTime: &startTime,
			})
			if err != nil {
				return err
			}
			// PullMarkVerified's sha256 arg only overwrites the stored
			// hash when non-empty (COALESCE(NULLIF(?, ''), sha256) — see
			// its doc comment); passing "" here on the not-redownloaded
			// branch (shaVal == existing.SHA256 already) is deliberate,
			// not a bug: it just re-stamps verified_at without disturbing
			// the hash PullUpsert above already carried forward unchanged.
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
		if err != nil && !errors.Is(err, errDashcamReprocessStopWalk) {
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
			"total_objects":        terminal.TotalObjects,
			"total_plates":         terminal.TotalPlates,
			"total_vehicles":       terminal.TotalVehicles,
			"total_track_events":   terminal.TotalTrackEvents,
		}, nil
	}
}

// dashcamReprocessScopeMatches reports whether seg falls within the scope
// params.validate() has already confirmed is non-empty: an exact match
// against Path, an exact match against Segment (either the full path or
// just its basename — the recorder's own reprocess route accepts a bare
// segment filename the same way, see resources/dashboard.py's
// _run_reprocess single-segment branch), or a same-day match against the
// segment's start_time (date, only combined with camera — which
// EachSegment already filtered server-side).
func dashcamReprocessScopeMatches(params dashcamReprocessJobParams, seg dashcam.Segment) bool {
	// A user (or a saved job's params) may have supplied the recorder's
	// own "/recordings/..." form directly instead of a manifest-relative
	// path — strip it before comparing against seg.Path, which is always
	// manifest-relative.
	scopePath := strings.TrimPrefix(params.Path, "/recordings/")
	scopeSegment := strings.TrimPrefix(params.Segment, "/recordings/")
	switch {
	case params.Path != "":
		return seg.Path == scopePath
	case params.Segment != "":
		return filepath.Base(seg.Path) == scopeSegment || seg.Path == scopeSegment
	case params.Date != "":
		return len(seg.StartTime) >= 10 && seg.StartTime[:10] == params.Date
	default:
		return false
	}
}

// dashcamReprocessRecordingDirPattern matches a recorder recording
// directory name, e.g. "recording_20260908_100000" in
// "front/2026/09/08/recording_20260908_100000/segment_1.mp4" — the
// directory dashcamReprocessScopeSince mines a nearby "since" cursor from.
var dashcamReprocessRecordingDirPattern = regexp.MustCompile(`recording_(\d{8})_(\d{6})`)

// dashcamReprocessScopeCamera derives the camera the re-pull phase's
// manifest walk should be scoped to: params.Camera when the job was given
// one directly, else the first path element of params.Path/params.Segment
// (after stripping any "/recordings/" prefix a caller may have supplied
// verbatim — the same tolerance recorderScopePath and
// dashcamReprocessScopeMatches apply). Returns "" when neither yields a
// recognizable camera, in which case the walk isn't camera-scoped at all
// (matching the walk's pre-fix behavior for that case).
func dashcamReprocessScopeCamera(params dashcamReprocessJobParams) string {
	if params.Camera != "" {
		return params.Camera
	}
	p := params.Path
	if p == "" {
		p = params.Segment
	}
	p = strings.TrimPrefix(p, "/recordings/")
	p = strings.TrimPrefix(p, "/")
	first, _, _ := strings.Cut(p, "/")
	if first == "front" || first == "rear" {
		return first
	}
	return ""
}

// dashcamReprocessScopeSince derives a manifest "since" cursor near the
// scoped run's target, so the re-pull phase's manifest walk doesn't have to
// page through the entire manifest (tens of thousands of segments on a
// long-lived dashcam) from the very beginning just to reach a single
// segment, a single recording, or one day's segments — see this fix's
// motivating report (re-pull pausing recording for minutes on a
// single-segment reprocess). One minute is subtracted from whatever
// timestamp is derived so the walk starts just *before* the target rather
// than exactly at it (a recorder's start_time comparison in Manifest is
// "since <" — see resources/ingest.py — so landing exactly on the target's
// own timestamp would skip it). Returns "" (a full walk from the
// beginning) when nothing in params yields a parseable timestamp to start
// near.
func dashcamReprocessScopeSince(params dashcamReprocessJobParams) string {
	if params.Camera != "" && params.Date != "" {
		t, err := time.Parse("2006-01-02", params.Date)
		if err != nil {
			return ""
		}
		return t.Add(-time.Minute).Format("2006-01-02T15:04:05")
	}
	p := params.Path
	if p == "" {
		p = params.Segment
	}
	m := dashcamReprocessRecordingDirPattern.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	t, err := time.Parse("20060102_150405", m[1]+"_"+m[2])
	if err != nil {
		return ""
	}
	return t.Add(-time.Minute).Format("2006-01-02T15:04:05")
}
