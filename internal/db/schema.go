package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// SchemaVersion is recorded in settings["schema_version"]. InitSchema bumps
// an older (or missing) stored version up to this on success, and refuses
// to touch a database stamped with a newer version than this binary knows
// about.
const SchemaVersion = 9

// TracksVersion identifies the current tracking/frame-hygiene logic
// (internal/scan/tracks.go's BuildTracks plus sidecar.go's
// dropEchoedEntries/mergeSamePTSFrames). IndexDetections stamps
// segments.tracks_version with this value every time it (re)builds a
// segment's det_tracks rows, and SegmentsNeedingTracksBackfill (below)
// selects segments whose stamped version is older (or NULL) — bump this
// whenever a change to tracking or frame-hygiene logic warrants
// re-tracking the whole corpus, in place of the one-shot "reset
// tracks_built_at to NULL" migration this replaced (see the v6->v7
// migration comment above for why that corpus-wide reset was a problem).
// Lives in this package rather than internal/scan (which is where the
// tracker itself lives) because internal/scan already imports internal/db
// (see scan/index.go) and this constant needs to be read from db's own
// SegmentsNeedingTracksBackfill query — scan importing db back would be a
// cycle. Currently 2: the v5->v6 fix (det_tracks.make,
// det_vehicles.make_confidence, sidecar frame-hygiene, same-frame NMS) is
// the first change tracked this way, so it starts one above the implicit
// "1" every pre-existing tracked segment is treated as.
const TracksVersion = 2

// settingsTableStmt is applied before anything else, and before the
// schema_version check, since that check itself reads from settings.
const settingsTableStmt = `CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value_json TEXT NOT NULL
)`

// schemaStatements is the full DDL for dashcam-manager, matching the "DB
// schema" section of docs/superpowers/specs/2026-09-08-dashcam-manager-design.md.
// Every statement is idempotent (IF NOT EXISTS) so InitSchema can run on
// every boot.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS shares (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		server TEXT NOT NULL,
		share TEXT NOT NULL,
		subpath TEXT NOT NULL DEFAULT '',
		username TEXT NOT NULL DEFAULT '',
		password_enc BLOB,
		domain TEXT NOT NULL DEFAULT '',
		mount_point TEXT NOT NULL DEFAULT '',
		options TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'unknown',
		last_mounted_at TEXT,
		-- kind (schema v9): 'cifs' (mounted via mount.cifs, the original
		-- shape) or 'local' (an already-local/bind-mounted directory —
		-- what the removed allow_local_share_paths hidden setting used to
		-- fake for every share row at once; see migrateSharesToV9Kind).
		-- Validated in Go (internal/db/shares.go), not via CHECK, so a
		-- future kind can be added without sqlite's ALTER-TABLE-can't-
		-- widen-a-CHECK dance (see the v7->v8 migration comment below for
		-- what that dance costs).
		kind TEXT NOT NULL DEFAULT 'cifs'
	)`,

	// share_id SET NULLs on share delete (final-fix-wave item 2): deleting
	// a share definition used to cascade straight through to the source and
	// wipe every segment/trip/detection indexed under it, with no
	// confirmation beyond the share-delete click — a single misclick could
	// silently destroy an otherwise-healthy index. Deletion now goes
	// through handleSharesDelete's own guard (cmd/api/handlers_shares.go),
	// which refuses to delete a share whose source still has segments
	// unless the caller passes ?purge_index=1; when it's allowed to
	// proceed without purging, the source survives (still browsable,
	// re-mountable under a fresh share) with share_id NULL instead of being
	// cascaded away by the FK.
	// base_url/target_share_id/pull_cursor/last_pull_at (schema v8) are only
	// ever populated for kind='dashcam' sources: base_url is the dashcam's
	// HTTP(S) API root, target_share_id names the share its pulled footage
	// lands under (dashcam_pulls.share_id, below), pull_cursor is an opaque
	// resume token the dashcam's list endpoint hands back, and
	// last_pull_at records when the pull job last completed. A fresh
	// install gets this shape directly from CREATE TABLE; an existing
	// database is brought here by migrateSourcesToV8DashcamKind (see
	// InitSchema) rebuilding the table, since sqlite has no ALTER TABLE for
	// widening a CHECK constraint.
	`CREATE TABLE IF NOT EXISTS sources (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL CHECK (kind IN ('ssd','share','dashcam')),
		share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
		root_path TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		last_scan_at TEXT,
		status TEXT NOT NULL DEFAULT 'idle',
		base_url TEXT,
		target_share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
		pull_cursor TEXT,
		last_pull_at TEXT,
		UNIQUE(root_path)
	)`,
	// idx_sources_share_id (fix round 1, Important 2): file_copies' v9
	// storage_files exclusion (below) filters segments by joining through
	// sources on share_id — root_path's own UNIQUE index doesn't help
	// that lookup, and without this the join was an unindexed scan of
	// sources per storage_files row.
	`CREATE INDEX IF NOT EXISTS idx_sources_share_id ON sources(share_id) WHERE share_id IS NOT NULL`,

	`CREATE TABLE IF NOT EXISTS trips (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		trip_key TEXT NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT,
		duration_s REAL,
		front_dir TEXT,
		rear_dir TEXT,
		pair_offset_s REAL,
		segment_count INTEGER NOT NULL DEFAULT 0,
		bytes INTEGER NOT NULL DEFAULT 0,
		distance_m REAL,
		max_speed_mps REAL,
		start_lat REAL,
		start_lon REAL,
		end_lat REAL,
		end_lon REAL,
		cover_thumb TEXT,
		UNIQUE(source_id, trip_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_trips_started_at ON trips(started_at)`,

	`CREATE TABLE IF NOT EXISTS segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		trip_id INTEGER REFERENCES trips(id) ON DELETE SET NULL,
		camera TEXT NOT NULL CHECK (camera IN ('front','rear')),
		rel_path TEXT NOT NULL,
		seq INTEGER NOT NULL DEFAULT 0,
		start_time TEXT NOT NULL,
		end_time TEXT,
		duration_s REAL,
		bytes INTEGER NOT NULL DEFAULT 0,
		mtime INTEGER,
		sha256 TEXT,
		width INTEGER,
		height INTEGER,
		has_detections INTEGER NOT NULL DEFAULT 0,
		has_gps INTEGER NOT NULL DEFAULT 0,
		thumb_status TEXT NOT NULL DEFAULT 'pending',
		indexed_at TEXT,
		-- archived_at (final-fix-wave item 7): set when a re-scan finds this
		-- segment's file gone from its source but a verified ingests row
		-- still references it — rather than deleting the row (which would
		-- cascade to ingests and lose all provenance of where the footage
		-- actually is now), the row is kept with its summary/detections
		-- intact and archived_at records when that happened. An archived
		-- segment is excluded from playback/media (see handlers_media.go)
		-- but still shows up in browse/trip listings and counts toward
		-- stats' ingest coverage. NULL for a normal, on-disk segment.
		archived_at TEXT,
		-- unreadable_reason/unreadable_at: set by the "thumbs" job
		-- (cmd/api/jobs_runtime.go) when ffmpeg's stderr for this segment's
		-- file matches a known unreadable-media signature (missing moov
		-- atom, corrupt header, no decodable frames at all) rather than a
		-- transient/hardware-fallback failure. thumb_status is set to
		-- 'unreadable' alongside these (see SegmentsForThumbs) so later
		-- thumbs jobs don't keep retrying a file that can never decode.
		-- Both NULL for a normal segment; cleared together by
		-- SegmentClearUnreadable (POST /api/segments/unreadable/recheck).
		unreadable_reason TEXT,
		unreadable_at TEXT,
		-- tracks_built_at (schema v5): stamped by IndexDetections every time
		-- it (re)builds this segment's det_tracks rows, whether or not any
		-- track actually survived the noise filter (see BuildTracks'
		-- minTrackFrames) — a segment with detections but zero real tracks
		-- must still count as "done" or SegmentsNeedingTracksBackfill would
		-- keep re-selecting it forever. NULL means "never processed by a
		-- tracker-aware IndexDetections", which is exactly what
		-- SegmentsNeedingTracksBackfill looks for (see queries_tracks.go);
		-- checking det_objects/det_plates/det_vehicles.track_id IS NULL
		-- instead doesn't converge, since noise-dropped boxes keep a NULL
		-- track_id forever even after a segment has been fully processed.
		tracks_built_at TEXT,
		-- tracks_version (schema v7): the TracksVersion the segment was
		-- last (re)tracked with, stamped alongside tracks_built_at every
		-- time IndexDetections (re)builds det_tracks. Lets a future tracker
		-- change trigger a targeted re-track (SegmentsNeedingTracksBackfill
		-- selects tracks_version < TracksVersion) without touching
		-- tracks_built_at itself — see the v6->v7 migration comment below
		-- for why that matters (searchStructuralHits' fast/slow path
		-- split keys off tracks_built_at, not tracks_version). NULL means
		-- "tracked before this column existed" (schema v5/v6 data).
		tracks_version INTEGER,
		UNIQUE(source_id, rel_path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_start_time ON segments(start_time)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_trip_camera_seq ON segments(trip_id, camera, seq)`,
	// M12: db.Stats' per-source storage and ingest-coverage panels
	// (internal/db/queries_browse.go) group/filter by source_id.
	`CREATE INDEX IF NOT EXISTS idx_segments_source_id ON segments(source_id)`,
	// idx_segments_source_rel_path (final-fix-wave item 3): explicit
	// covering index for (source_id, rel_path) lookups (e.g. re-pull scope
	// walks) independent of the UNIQUE(source_id, rel_path) constraint
	// above ever changing shape.
	`CREATE INDEX IF NOT EXISTS idx_segments_source_rel_path ON segments(source_id, rel_path)`,
	// idx_segments_unreadable_at (GET /api/segments/unreadable's ordering
	// index) is deliberately NOT here: schemaStatements runs unconditionally
	// on every boot, including before the v3->v4 migration below has added
	// the unreadable_at column to a pre-existing database — it's created
	// instead by postMigrationStatements, once every database (fresh or
	// migrated) definitely has the column.

	`CREATE TABLE IF NOT EXISTS segment_gps (
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		t TEXT NOT NULL,
		lat REAL NOT NULL,
		lon REAL NOT NULL,
		speed_mps REAL,
		heading REAL,
		PRIMARY KEY (segment_id, t)
	)`,

	`CREATE TABLE IF NOT EXISTS segment_summary (
		segment_id INTEGER PRIMARY KEY REFERENCES segments(id) ON DELETE CASCADE,
		n_car INTEGER NOT NULL DEFAULT 0,
		n_truck INTEGER NOT NULL DEFAULT 0,
		n_bus INTEGER NOT NULL DEFAULT 0,
		n_person INTEGER NOT NULL DEFAULT 0,
		n_bicycle INTEGER NOT NULL DEFAULT 0,
		n_motorcycle INTEGER NOT NULL DEFAULT 0,
		n_plates INTEGER NOT NULL DEFAULT 0,
		top_classes TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS detections (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		pts_offset REAL NOT NULL,
		wall_time TEXT,
		frame_index INTEGER,
		lat REAL,
		lon REAL,
		speed_mps REAL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_detections_segment_pts ON detections(segment_id, pts_offset)`,

	`CREATE TABLE IF NOT EXISTS det_objects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		det_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE,
		class TEXT NOT NULL,
		confidence REAL,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL,
		-- track_id (schema v5): the det_tracks row this box was assigned to
		-- by the IoU tracker (internal/scan/tracks.go), NULL until
		-- IndexDetections/tracks_backfill has run for this segment. Not a
		-- SQL foreign key: det_tracks rows are deleted and reinserted
		-- alongside detections on re-index (see IndexDetections), and
		-- tying it to an ON DELETE behavior here would fight that.
		track_id INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_det_objects_class ON det_objects(class)`,
	// M12 fix round 1: GET /api/search's class/confidence filters run as a
	// correlated "EXISTS (SELECT 1 FROM det_objects o WHERE o.det_id = d.id
	// AND o.class IN (...) [AND o.confidence >= ?])" per candidate
	// detection. det_id is the correlation key and is highly selective
	// (a handful of objects per detection at most), so a det_id-leading
	// covering index makes each EXISTS an O(1) index seek — that, combined
	// with ORDER BY s.start_time DESC LIMIT ? on the outer segments scan,
	// lets sqlite stop as soon as it's found `limit` matching segments
	// instead of evaluating the filter against the whole table. (An
	// earlier version of this index led with `class` instead, for a since-
	// abandoned uncorrelated-IN-subquery strategy — see git history /
	// m12-report.md's "fix round 1" section for the measured before/after.)
	`CREATE INDEX IF NOT EXISTS idx_det_objects_det_id_class_confidence ON det_objects(det_id, class, confidence)`,
	`DROP INDEX IF EXISTS idx_det_objects_class_det_id`,

	`CREATE TABLE IF NOT EXISTS det_plates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		det_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE,
		text TEXT NOT NULL,
		confidence REAL,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL,
		-- track_id: see det_objects.track_id's comment above.
		track_id INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_det_plates_text ON det_plates(text)`,
	// M12: search's plate filter is a correlated
	// "EXISTS (... WHERE det_id = d.id AND text LIKE ?)" — plate substring
	// matching can't use a leading-column index for the LIKE itself, but
	// det_id still narrows to a handful of rows per detection first.
	`CREATE INDEX IF NOT EXISTS idx_det_plates_det_id ON det_plates(det_id)`,

	`CREATE TABLE IF NOT EXISTS det_vehicles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		det_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE,
		type TEXT,
		make TEXT,
		confidence REAL,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL,
		-- track_id: see det_objects.track_id's comment above.
		track_id INTEGER,
		-- make_confidence (schema v6): the recorder's classifier confidence
		-- for make, previously parsed but dropped before insert (see
		-- internal/scan/sidecar.go's Vehicle.MakeConfidence and
		-- internal/scan/tracks.go's per-track make vote, which weights each
		-- box's vote by max(make_confidence, makeVoteFloor)). NULL for a
		-- vehicle entry with no make.
		make_confidence REAL
	)`,
	// M12: search's make/type filters, same det_id-correlated EXISTS
	// story as det_objects above (there's typically ~1 det_vehicles row
	// per detection, so a plain det_id index already makes the EXISTS an
	// O(1) seek for both make and type).
	`CREATE INDEX IF NOT EXISTS idx_det_vehicles_det_id ON det_vehicles(det_id)`,
	`DROP INDEX IF EXISTS idx_det_vehicles_make_det_id`,
	`DROP INDEX IF EXISTS idx_det_vehicles_type_det_id`,
	// WP8: GET /api/catalog groups by make/type (GROUP BY t.make / t.type)
	// and, per group, looks up the most recent sample segment via a
	// correlated "WHERE t2.make = t.make" subquery — both value-leading,
	// not det_id-leading like search's EXISTS lookups above, so they need
	// their own indexes rather than reusing idx_det_vehicles_det_id.
	`CREATE INDEX IF NOT EXISTS idx_det_vehicles_make ON det_vehicles(make)`,
	`CREATE INDEX IF NOT EXISTS idx_det_vehicles_type ON det_vehicles(type)`,

	// det_tracks (schema v5): one row per object/plate/vehicle followed
	// across a segment's analyzed frames by the greedy IoU tracker
	// (internal/scan/tracks.go BuildTracks), built alongside det_objects/
	// det_plates/det_vehicles by IndexDetections and reference by their
	// track_id columns above. kind is "object", "plate", or "vehicle";
	// class is the object/vehicle class, or '' for plate (plates aren't
	// classified). x1/y1/x2/y2 are the track's last known box.
	`CREATE TABLE IF NOT EXISTS det_tracks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		class TEXT NOT NULL DEFAULT '',
		first_pts REAL NOT NULL,
		last_pts REAL NOT NULL,
		n_frames INTEGER NOT NULL,
		best_conf REAL,
		best_text TEXT,
		x1 REAL, y1 REAL, x2 REAL, y2 REAL,
		-- make (schema v6): the track's canonical per-track vehicle make,
		-- voted across every member and NMS-absorbed box (see
		-- internal/scan/tracks.go's trackState.makeVotes / resolveMake).
		-- Deliberately its own column rather than overloading best_text
		-- (which stays plate-only, e.g. queries_search.go's LIKE matches
		-- against it); NULL/'' for kind != "vehicle" or a vehicle track
		-- whose members never reported a make.
		make TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_det_tracks_segment ON det_tracks(segment_id)`,
	// Task 9 round 2: searchStructuralHitsFromTracks and searchQHits' tracked
	// branch (queries_search.go) drive straight off det_tracks for segments
	// that are fully tracked (segments.tracks_built_at IS NOT NULL) instead
	// of grouping raw det_objects/plates/vehicles rows per search — this
	// covering index lets that per-segment "tracks of these kinds, oldest
	// first" lookup stay index-only.
	`CREATE INDEX IF NOT EXISTS idx_det_tracks_seg_kind_class ON det_tracks(segment_id, kind, class, first_pts)`,

	// Gzipped per-segment raw detection JSON, kept for re-indexing /
	// re-normalizing without re-parsing sidecars; class/plate/make rows
	// above are the exploded, queryable projection.
	`CREATE TABLE IF NOT EXISTS detections_raw (
		segment_id INTEGER PRIMARY KEY REFERENCES segments(id) ON DELETE CASCADE,
		gz BLOB NOT NULL
	)`,

	// search_terms is the ordinary table of record for full-text search
	// entries (plate text, vehicle make, object class, ...), one row per
	// indexed term. search_fts is an external-content FTS5 index over it:
	// FTS5 doesn't enforce foreign keys or support ON DELETE CASCADE
	// itself, so the content lives in a normal table that *does* cascade
	// when its segment is deleted, and the triggers below keep the FTS
	// shadow tables in sync with every insert/update/delete.
	`CREATE TABLE IF NOT EXISTS search_terms (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		det_id INTEGER,
		kind TEXT NOT NULL,
		text TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_search_terms_segment_id ON search_terms(segment_id)`,

	`CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(
		text,
		kind UNINDEXED,
		content='search_terms',
		content_rowid='id',
		tokenize='trigram'
	)`,

	// Standard FTS5 external-content sync triggers (per the sqlite docs'
	// recipe): keep the search_fts shadow tables consistent with
	// search_terms on every write, including the cascade deletes that fire
	// when a segment (and transitively its search_terms rows) is removed.
	`CREATE TRIGGER IF NOT EXISTS search_terms_ai AFTER INSERT ON search_terms BEGIN
		INSERT INTO search_fts (rowid, text, kind) VALUES (new.id, new.text, new.kind);
	END`,
	`CREATE TRIGGER IF NOT EXISTS search_terms_ad AFTER DELETE ON search_terms BEGIN
		INSERT INTO search_fts (search_fts, rowid, text, kind) VALUES ('delete', old.id, old.text, old.kind);
	END`,
	`CREATE TRIGGER IF NOT EXISTS search_terms_au AFTER UPDATE ON search_terms BEGIN
		INSERT INTO search_fts (search_fts, rowid, text, kind) VALUES ('delete', old.id, old.text, old.kind);
		INSERT INTO search_fts (rowid, text, kind) VALUES (new.id, new.text, new.kind);
	END`,

	`CREATE TABLE IF NOT EXISTS jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		type TEXT NOT NULL,
		state TEXT NOT NULL DEFAULT 'queued',
		priority INTEGER NOT NULL DEFAULT 0,
		params_json TEXT,
		progress REAL NOT NULL DEFAULT 0,
		message TEXT,
		result_json TEXT,
		error TEXT,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		started_at TEXT,
		finished_at TEXT,
		cancel_requested INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS exports (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
		name TEXT NOT NULL,
		path TEXT,
		share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
		bytes INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		expires_at TEXT
	)`,
	// idx_exports_share_path (fix round 1, Important 2): file_copies' own
	// exports-vs-storage_files matching (both the storage_files
	// exclusion and the exports branch's sha256 COALESCE, below) looks up
	// "is there an export at (share_id, path)" per storage_files row.
	`CREATE INDEX IF NOT EXISTS idx_exports_share_path ON exports(share_id, path)`,

	// ingests records a verified (or attempted) copy of a segment from an
	// SSD source to a share destination — one row per (segment_id,
	// share_id) so an ingest job restart can tell an already-verified
	// segment apart from one that needs (re)copying (see internal/ingest).
	// verified_at is set only once the destination file has been re-opened
	// and its size (and sha256, when verify=="sha256") confirmed to match;
	// a row with verified_at NULL means a copy was attempted but not yet
	// (or no longer) confirmed.
	`CREATE TABLE IF NOT EXISTS ingests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
		share_id INTEGER NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
		dest_rel_path TEXT NOT NULL,
		bytes INTEGER NOT NULL DEFAULT 0,
		sha256 TEXT,
		verified_at TEXT,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		UNIQUE(segment_id, share_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ingests_share_id ON ingests(share_id)`,
	// idx_ingests_share_dest_path (fix round 1, Important 2): file_copies'
	// storage_files exclusion (below) looks up "is there a verified
	// ingest at (share_id, dest_rel_path)" per storage_files row —
	// without this it's an unindexed scan of ingests every time.
	`CREATE INDEX IF NOT EXISTS idx_ingests_share_dest_path ON ingests(share_id, dest_rel_path)`,
	// M12: db.Stats' ingest-coverage panel checks, per SSD-sourced segment,
	// "EXISTS (SELECT 1 FROM ingests WHERE segment_id = ? AND verified_at
	// IS NOT NULL)" — segment_id already leads the UNIQUE(segment_id,
	// share_id) index above, but an explicit index makes that independent
	// of the unique constraint's column order ever changing.
	`CREATE INDEX IF NOT EXISTS idx_ingests_segment_id ON ingests(segment_id)`,

	// dashcam_pulls (schema v8) records one row per remote file pulled from
	// a kind='dashcam' source: the mirror-image of ingests (which tracks an
	// SSD segment copied out to a share) for a dashcam that's pulled in
	// over its network API instead of scanned off a mounted SSD. It's
	// keyed by (dashcam_source_id, remote_rel_path) rather than by
	// segment_id because the pulled file usually isn't indexed into
	// `segments` (and so has no segment_id) until IndexDetections has
	// scanned it off the destination share — indexed_segment_id is filled
	// in later, by PullsLinkSegments, once that's happened.
	// verified_at mirrors ingests.verified_at (set once the destination
	// file's size/hash have been re-confirmed); deleted_remote_at records
	// when the pull job deleted the now-safely-copied file off the dashcam
	// itself, so PullCleanupCandidates doesn't keep re-offering it.
	// last_seen_at/remote_has_detections/remote_has_gps (final-fix-wave
	// item 1, "late sidecars lost then deleted"): stamped on *every*
	// manifest listing that includes this remote file — whether it's
	// freshly pulled (PullUpsert) or skipped because it's already pulled
	// and verified (PullTouchSeen) — not just on a pull attempt.
	// remote_has_detections/remote_has_gps record what the manifest most
	// recently claimed about this file so the cleanup job's local check
	// (dashcamCleanupJobHandler) can refuse to delete a remote file whose
	// local .detections.json/.gps.json sidecar hasn't actually landed
	// yet, even though the mp4 itself is verified.
	//
	// remote_start_time/settled_at (final-fix-wave item 1, second pass —
	// replacing an earlier last_seen_at>=last_pull_at + 24h-mtime guard
	// that was provably unsatisfiable in steady state: an incremental
	// pull only ever lists segments newer than persisted-cursor-24h, so
	// anything it just stamped last_seen_at on is by construction never
	// >24h old, and cleanup would never find a candidate). remote_start_time
	// is the recorder's own start_time string for this segment, copied
	// verbatim from the manifest — the recorder's clock, not this
	// manager's wall clock or dashcam_pulls.mtime. settled_at is stamped
	// by PullMarkSettled (queries_dashcam.go) once the recorder has
	// demonstrably produced a segment materially (>=1h) newer than this
	// one while this row was still being observed — comparing the
	// recorder's own clock to itself sidesteps every cross-clock
	// reliability problem the mtime-based version had. PullCleanupCandidates
	// requires settled_at IS NOT NULL.
	`CREATE TABLE IF NOT EXISTS dashcam_pulls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		dashcam_source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
		remote_rel_path TEXT NOT NULL,
		share_id INTEGER NOT NULL,
		dest_rel_path TEXT NOT NULL,
		bytes INTEGER NOT NULL,
		sha256 TEXT,
		mtime TEXT,
		pulled_at TEXT NOT NULL,
		verified_at TEXT,
		indexed_segment_id INTEGER REFERENCES segments(id) ON DELETE SET NULL,
		deleted_remote_at TEXT,
		last_seen_at TEXT,
		remote_has_detections INTEGER NOT NULL DEFAULT 0,
		remote_has_gps INTEGER NOT NULL DEFAULT 0,
		remote_start_time TEXT,
		settled_at TEXT,
		UNIQUE(dashcam_source_id, remote_rel_path)
	)`,
	// PullCleanupCandidates filters by (dashcam_source_id, verified_at)
	// (plus indexed_segment_id/deleted_remote_at/settled_at, not worth
	// indexing — PullCleanupCandidates rows are a small fraction of a
	// dashcam's pulls); dashcam_source_id already leads the UNIQUE index
	// above, but an explicit index keeps that independent of the unique
	// constraint's column order. (PullGet filters by (dashcam_source_id,
	// remote_rel_path) instead, which the UNIQUE index above already
	// covers directly.)
	`CREATE INDEX IF NOT EXISTS idx_dashcam_pulls_source_verified ON dashcam_pulls(dashcam_source_id, verified_at)`,
	// PullMarkSettled's UPDATE filters by (dashcam_source_id,
	// last_seen_at, settled_at) — settling runs once per dashcam_pull job
	// over that run's own freshly-stamped rows, so this keeps that lookup
	// an index seek rather than a scan over the whole table as a
	// dashcam's pull history grows.
	`CREATE INDEX IF NOT EXISTS idx_dashcam_pulls_source_lastseen_settled ON dashcam_pulls(dashcam_source_id, last_seen_at, settled_at)`,
	// idx_dashcam_pulls_share_dest_path (fix round 1, Important 2): same
	// reasoning as idx_ingests_share_dest_path above, for file_copies'
	// storage_files exclusion's dashcam_pulls lookup.
	`CREATE INDEX IF NOT EXISTS idx_dashcam_pulls_share_dest_path ON dashcam_pulls(share_id, dest_rel_path)`,
	// idx_dashcam_pulls_indexed_segment_verified (task 10 fix round 1):
	// Stats' "does this SSD segment have a persistent copy" check does
	// EXISTS (SELECT 1 FROM dashcam_pulls WHERE indexed_segment_id = ? AND
	// verified_at IS NOT NULL) per SSD segment -- without this, indexed_
	// segment_id has no index at all and that's a full table scan per row.
	`CREATE INDEX IF NOT EXISTS idx_dashcam_pulls_indexed_segment_verified ON dashcam_pulls(indexed_segment_id, verified_at)`,

	// WP8 fix: catalog_terms is a precomputed cache of GET /api/catalog's
	// distinct-value listing (make/type/plate/class), rebuilt wholesale by
	// db.RefreshCatalog rather than queried live — a live GROUP BY over a
	// multi-million-row det_vehicles measured at ~24s per request. See
	// queries_catalog.go's package doc comment for when RefreshCatalog runs.
	`CREATE TABLE IF NOT EXISTS catalog_terms (
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		count INTEGER NOT NULL DEFAULT 0,
		first_seen TEXT,
		last_seen TEXT,
		sample_segment_id INTEGER,
		PRIMARY KEY (kind, value)
	)`,
	// CatalogList's default listing orders by count desc within a kind.
	`CREATE INDEX IF NOT EXISTS idx_catalog_terms_kind_count ON catalog_terms(kind, count DESC)`,
	// Redundant with the PRIMARY KEY(kind, value) above for an exact-value
	// lookup, but explicit so a `value LIKE ?` scan (CatalogList's `q`
	// filter) stays scoped to one kind via a kind-leading index regardless
	// of how sqlite names/orders the primary key's implicit index.
	`CREATE INDEX IF NOT EXISTS idx_catalog_terms_kind_value ON catalog_terms(kind, value)`,

	// storage_files (schema v9, local-storage plan task 5): one row per
	// regular file the scan walker finds under a share/local-backed
	// source's root — every file, not just segment_NNNNN.mp4s, so exports
	// and sidecars copied onto a storage are also tracked. Feeds the
	// duplicate-detection view planned for later tasks. Never populated for
	// an ssd-kind source (the SSD is never a storage — see the plan's
	// Context section); share_id CASCADEs with its shares row, same as
	// ingests/dashcam_pulls/exports. sha256/hashed_at are NULL until the
	// hash_backfill job (task 6) processes the row; first_seen_at/
	// last_seen_at are stamped by the scan walker (StorageFileUpsert) and
	// last_seen_at is how StorageFilesPruneNotSeenSince tells a file that's
	// gone from the latest scan apart from one still there.
	`CREATE TABLE IF NOT EXISTS storage_files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		share_id INTEGER NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
		rel_path TEXT NOT NULL,
		bytes INTEGER NOT NULL DEFAULT 0,
		mtime INTEGER,
		sha256 TEXT,
		hashed_at TEXT,
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		UNIQUE(share_id, rel_path)
	)`,
	// Duplicate detection (later tasks) groups by sha256 first, falling
	// back to (bytes, lower(basename)) — see the plan's Design section.
	`CREATE INDEX IF NOT EXISTS idx_storage_files_sha256 ON storage_files(sha256)`,
	`CREATE INDEX IF NOT EXISTS idx_storage_files_bytes ON storage_files(bytes)`,
	// hash_backfill's resume cursor (task 6) is "sha256 IS NULL" — a
	// partial index keeps StorageFilesNeedingHash an index-only scan
	// regardless of how large a fully-hashed corpus grows.
	`CREATE INDEX IF NOT EXISTS idx_storage_files_unhashed ON storage_files(share_id) WHERE sha256 IS NULL`,

	// file_copies (schema v9, local-storage plan task 7): the single view
	// definition of "a copy of a file on persistent storage", unioning
	// every table that records one — segments (only share/local-backed
	// ones: src.share_id IS NOT NULL excludes kind=ssd, since the SSD is
	// never a storage), ingests, dashcam_pulls, exports, and storage_files.
	// Consumed by DuplicateReportRefresh/SegmentCopyCounts (queries_storage.go)
	// rather than queried directly by API handlers.
	//
	// storage_key identifies the persistent storage a row's file lives on:
	// 'share:<share_id>' for anything on a shares row (cifs or local),
	// 'cache' for a share_id-NULL export (the DASHCAM_CACHE exports dir —
	// there is no shares row for the cache, so it needs its own identity;
	// see the plan's "Cache exports" note). Two rows on the same
	// storage_key are the same storage, never a cross-storage duplicate.
	//
	// sha256 is COALESCE'd from the row's own column to its linked
	// segment's sha256 for ingest/pull rows: "ingest/pull rows are
	// verified members of their segment's group by construction" (plan's
	// Duplicate semantics) — an ingest/pull was already byte-verified
	// against its source segment at copy time (ingests.verified_at/
	// dashcam_pulls.verified_at), so once *either* row has a stamped
	// sha256 the other is known to share it, even before hash_backfill
	// gets around to that particular row. Only verified_at IS NOT NULL
	// ingest/pull rows count as copies at all — an attempted-but-not-yet-
	// confirmed copy may not actually exist at the destination.
	//
	// The storage_files branch excludes any row that's already
	// represented by a segment/ingest/pull/export row on the very same
	// (share_id, rel_path) — storage_files' scan walker (task 5) walks a
	// share/local storage's *entire* tree, including files those other
	// tables already track (an ingested/share-scanned segment's own
	// mp4, an export landed on a share target), so without this exclusion
	// every such file would count as two copies of itself on the same
	// storage instead of one copy on that storage. This is exactly what
	// storage_files' own schema comment above ("not already covered by
	// segments/ingests/pulls/exports") promises. exports.path is stored
	// as an absolute path (share mount_point + filename), not a rel_path
	// like the others, so the comparison re-derives one via the share's
	// mount_point before comparing.
	`CREATE VIEW IF NOT EXISTS file_copies AS
		SELECT
			'segment' AS ref_kind, s.id AS ref_id, src.share_id AS share_id, s.source_id AS source_id,
			'share:' || src.share_id AS storage_key,
			s.rel_path AS rel_path, s.bytes AS bytes, s.sha256 AS sha256
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE src.share_id IS NOT NULL

		UNION ALL

		SELECT
			'ingest', i.id, i.share_id, NULL,
			'share:' || i.share_id,
			i.dest_rel_path, i.bytes, COALESCE(i.sha256, seg.sha256)
		FROM ingests i
		LEFT JOIN segments seg ON seg.id = i.segment_id
		WHERE i.verified_at IS NOT NULL

		UNION ALL

		SELECT
			'pull', p.id, p.share_id, NULL,
			'share:' || p.share_id,
			p.dest_rel_path, p.bytes, COALESCE(p.sha256, pseg.sha256)
		FROM dashcam_pulls p
		LEFT JOIN segments pseg ON pseg.id = p.indexed_segment_id
		WHERE p.verified_at IS NOT NULL

		UNION ALL

		SELECT
			'export', e.id, e.share_id, NULL,
			CASE WHEN e.share_id IS NULL THEN 'cache' ELSE 'share:' || e.share_id END,
			e.path, e.bytes,
			-- Fix round 1, Important 3: prefer a matching storage_files
			-- row's sha256 over leaving this NULL forever -- an export
			-- landed on a share target sits on disk exactly where the
			-- scan walker (task 5) also indexes it into storage_files,
			-- which DOES get hashed by hash_backfill; without this an
			-- export could never leave the size_name (probable) bucket
			-- even once its file is fully hashed.
			(SELECT sf3.sha256 FROM storage_files sf3
				JOIN shares esh3 ON esh3.id = e.share_id
				WHERE sf3.share_id = e.share_id AND esh3.mount_point || '/' || sf3.rel_path = e.path
				LIMIT 1)
		FROM exports e
		WHERE e.path IS NOT NULL

		UNION ALL

		SELECT
			'storage_file', sf.id, sf.share_id, NULL,
			'share:' || sf.share_id,
			sf.rel_path, sf.bytes, sf.sha256
		FROM storage_files sf
		WHERE NOT EXISTS (
				SELECT 1 FROM ingests i2
				WHERE i2.share_id = sf.share_id AND i2.dest_rel_path = sf.rel_path AND i2.verified_at IS NOT NULL
			)
			AND NOT EXISTS (
				SELECT 1 FROM dashcam_pulls p2
				WHERE p2.share_id = sf.share_id AND p2.dest_rel_path = sf.rel_path AND p2.verified_at IS NOT NULL
			)
			AND NOT EXISTS (
				SELECT 1 FROM segments s2
				JOIN sources src2 ON src2.id = s2.source_id
				WHERE src2.share_id = sf.share_id AND s2.rel_path = sf.rel_path
			)
			AND NOT EXISTS (
				SELECT 1 FROM exports e2
				JOIN shares esh ON esh.id = e2.share_id
				WHERE e2.share_id = sf.share_id AND e2.path = esh.mount_point || '/' || sf.rel_path
			)
	`,

	// duplicate_groups/duplicate_group_members (schema v9, task 7):
	// precomputed like catalog_terms above — rebuilt wholesale by
	// db.DuplicateReportRefresh from file_copies rather than queried live,
	// same "expensive to compute, cheap to read back" reasoning. kind is
	// 'sha256' (verified: equal non-null hash) or 'size_name' (probable:
	// equal bytes + lower(basename), at least one member still unhashed —
	// see queries_storage.go's DuplicateReportRefresh doc comment for the
	// exact rules, including "hashed-and-different is dropped, not
	// reported"). copies is the count of distinct (storage_key, rel_path)
	// members after db.dedupeFileCopies has collapsed any file_copies
	// rows that describe the exact same physical file via more than one
	// ref_kind (fix round 1, Critical 1 — e.g. a segment ingested onto
	// share X and later scanned as its own kind='share' source produces
	// both a 'segment' and an 'ingest' row for the same file on X);
	// storages is the count of DISTINCT storage_key values among them (a
	// group only exists when that's >= 2 — two copies sitting on the very
	// same storage are not a cross-storage duplicate, and copies can
	// exceed storages when two differently-named files share a storage).
	// bytes is the (MAX, defensively) file size; reclaimable space is
	// bytes*(storages-1) — not bytes*(copies-1), since keeping one copy
	// per storage already keeps every same-storage member — computed at
	// read time (DuplicateReportList / the API layer), not stored here.
	`CREATE TABLE IF NOT EXISTS duplicate_groups (
		group_key TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		copies INTEGER NOT NULL,
		storages INTEGER NOT NULL,
		bytes INTEGER NOT NULL,
		sample_name TEXT NOT NULL,
		refreshed_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_duplicate_groups_kind ON duplicate_groups(kind)`,

	`CREATE TABLE IF NOT EXISTS duplicate_group_members (
		group_key TEXT NOT NULL REFERENCES duplicate_groups(group_key) ON DELETE CASCADE,
		ref_kind TEXT NOT NULL,
		ref_id INTEGER NOT NULL,
		share_id INTEGER,
		source_id INTEGER,
		storage_key TEXT NOT NULL,
		rel_path TEXT NOT NULL,
		-- hashed (schema v9, fix round 1 on the local-storage plan's task
		-- 11 — folded into v9's own DDL rather than a separate v10 bump,
		-- since v9 was still unreleased): this member's own
		-- file_copies.sha256 was non-null at the last DuplicateReportRefresh
		-- — a "size_name" (probable) group can have some members verified
		-- by sha256 already and others not; the group-level kind alone
		-- can't tell them apart, so the Storage page's "Hashed?" column
		-- reads this instead.
		hashed INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (group_key, ref_kind, ref_id)
	)`,
	// DuplicateReportList's per-storage filter (?storage_id=) and
	// SegmentCopyCounts' per-segment lookup both filter members by
	// storage_key/ref, independent of group_key.
	`CREATE INDEX IF NOT EXISTS idx_duplicate_group_members_storage_key ON duplicate_group_members(storage_key)`,
	`CREATE INDEX IF NOT EXISTS idx_duplicate_group_members_ref ON duplicate_group_members(ref_kind, ref_id)`,
}

// baselineSchemaVersion is the oldest stored schema_version InitSchema will
// still boot against. Per the final-fix-wave ruling: no production
// database existed as of SchemaVersion 3, so 3 is the baseline rather than
// something InitSchema tries to migrate up from — a database stamped with
// a version older than this is from a dev/test build predating the
// baseline and is refused rather than silently upgraded, since the DDL
// that would have taken it from e.g. version 1 to 3 was never written.
const baselineSchemaVersion = 3

// migration is a schema change gated on the stored version already being
// at least its From version, applied (and the stored version bumped to To)
// on every boot where that holds. Append a migration here, bump
// SchemaVersion, and any database already at >= baselineSchemaVersion picks
// it up on next boot.
type migration struct {
	From, To   int
	Statements []string
}

var migrations = []migration{
	// v3->v4: segments.unreadable_reason/unreadable_at, for marking
	// segments whose media file can't be decoded (see the segments CREATE
	// TABLE's comment above). The idempotent CREATE TABLE above already
	// gives fresh databases these columns; this migration adds them to a
	// database created before this change. ALTER TABLE ADD COLUMN isn't
	// itself idempotent, but this whole block only runs when the stored
	// version is still 3 (From/To gating in InitSchema), so it applies at
	// most once per database.
	{
		From: 3, To: 4,
		Statements: []string{
			`ALTER TABLE segments ADD COLUMN unreadable_reason TEXT`,
			`ALTER TABLE segments ADD COLUMN unreadable_at TEXT`,
		},
	},
	// v4->v5: det_objects/det_plates/det_vehicles.track_id, linking each
	// exploded detection box to the det_tracks row (schema v5, in
	// schemaStatements above — CREATE TABLE IF NOT EXISTS already covers a
	// fresh database) the tracker assigned it to. NULL until IndexDetections
	// or the tracks_backfill job (re)processes the segment.
	{
		From: 4, To: 5,
		Statements: []string{
			`ALTER TABLE det_objects ADD COLUMN track_id INTEGER`,
			`ALTER TABLE det_plates ADD COLUMN track_id INTEGER`,
			`ALTER TABLE det_vehicles ADD COLUMN track_id INTEGER`,
			`ALTER TABLE segments ADD COLUMN tracks_built_at TEXT`,
		},
	},
	// v5->v6: det_tracks.make (canonical per-track vehicle make vote) and
	// det_vehicles.make_confidence (previously parsed but dropped before
	// insert — see internal/scan/index.go's IndexDetections); both are
	// part of the fix for the recorder's duplicate-vehicle-classification
	// bug (docs/upstream/recorder-duplicate-vehicles.md).
	{
		From: 5, To: 6,
		Statements: []string{
			`ALTER TABLE det_tracks ADD COLUMN make TEXT`,
			`ALTER TABLE det_vehicles ADD COLUMN make_confidence REAL`,
		},
	},
	// v6->v7: segments.tracks_version, a version marker distinct from
	// tracks_built_at. An earlier version of this migration instead reset
	// tracks_built_at to NULL corpus-wide to force a one-shot re-track for
	// the make/make_confidence fix above — but tracks_built_at also gates
	// searchStructuralHits' fast path (segments with it set skip straight
	// to det_tracks instead of the slower raw det_objects/plates/vehicles
	// GROUP BY; see queries_search.go), so nulling it for the whole corpus
	// would have knocked every segment onto the slow path for the entire
	// duration of the backfill. tracks_version decouples the two: it's
	// what SegmentsNeedingTracksBackfill (queries_tracks.go) now selects
	// on (tracks_built_at IS NULL OR tracks_version IS NULL OR
	// tracks_version < TracksVersion), while tracks_built_at is left
	// untouched here so already-tracked segments keep the fast path until
	// tracks_backfill actually reprocesses them (which re-stamps both
	// columns — see internal/scan/index.go). Bumping TracksVersion
	// (currently 2, reflecting this fix) is what future tracker changes
	// should do to trigger a fresh backfill, rather than another
	// corpus-wide UPDATE.
	{
		From: 6, To: 7,
		Statements: []string{
			`ALTER TABLE segments ADD COLUMN tracks_version INTEGER`,
		},
	},
	// v7->v8 (dashcam sources + dashcam_pulls) is deliberately NOT a plain
	// ALTER TABLE ADD COLUMN entry here: sources.kind's CHECK constraint
	// needs widening to allow 'dashcam', and sqlite has no ALTER TABLE form
	// for that — only the create-new-table/copy/drop/rename dance sqlite's
	// own docs recommend for a CHECK or FK change. That dance also needs
	// PRAGMA foreign_keys=OFF for its DROP TABLE step (see
	// migrateSourcesToV8DashcamKind's doc comment for why), and
	// PRAGMA foreign_keys can only be toggled outside a transaction — which
	// this migrations loop, running inside InitSchema's single shared Tx,
	// can't do. migrateSourcesToV8DashcamKind runs as its own step, in its
	// own transaction, before InitSchema opens the Tx that runs everything
	// below (including dashcam_pulls' CREATE TABLE, which is ordinary
	// idempotent DDL in schemaStatements above and needs no migration entry
	// at all).

	// v8->v9: shares.kind, first-class local-directory storages. Plain
	// ALTER TABLE ADD COLUMN (no CHECK to widen, unlike v7->v8's sources
	// table) is enough here — every existing share row gets 'cifs', the
	// DEFAULT. migrateSharesToV9Kind (below, run from InitSchema right
	// after this migration applies, in the same Tx) then promotes every
	// row to 'local' if the hidden allow_local_share_paths setting was on,
	// which is what that setting actually meant, and deletes the setting.
	{
		From: 8, To: 9,
		Statements: []string{
			`ALTER TABLE shares ADD COLUMN kind TEXT NOT NULL DEFAULT 'cifs'`,
			// storage_files (task 5 of the local-storage plan) — a brand
			// new table, not an ALTER, so this is just the same idempotent
			// CREATE TABLE/INDEX statements as schemaStatements above.
			// Listed here too (redundantly, but harmlessly, since both are
			// IF NOT EXISTS) so a database crossing v8->v9 gets the table
			// from this migration's own DDL rather than depending on
			// schemaStatements running first in the shared Tx.
			`CREATE TABLE IF NOT EXISTS storage_files (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				share_id INTEGER NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
				rel_path TEXT NOT NULL,
				bytes INTEGER NOT NULL DEFAULT 0,
				mtime INTEGER,
				sha256 TEXT,
				hashed_at TEXT,
				first_seen_at TEXT NOT NULL,
				last_seen_at TEXT NOT NULL,
				UNIQUE(share_id, rel_path)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_storage_files_sha256 ON storage_files(sha256)`,
			`CREATE INDEX IF NOT EXISTS idx_storage_files_bytes ON storage_files(bytes)`,
			`CREATE INDEX IF NOT EXISTS idx_storage_files_unhashed ON storage_files(share_id) WHERE sha256 IS NULL`,
			// file_copies/duplicate_groups/duplicate_group_members (task
			// 7) -- identical, redundantly-but-harmlessly idempotent DDL to
			// schemaStatements above, listed here too so a database
			// crossing v8->v9 gets them from this migration's own
			// statements rather than depending on schemaStatements running
			// first in the shared Tx (same reasoning as storage_files just
			// above).
			`CREATE VIEW IF NOT EXISTS file_copies AS
				SELECT
					'segment' AS ref_kind, s.id AS ref_id, src.share_id AS share_id, s.source_id AS source_id,
					'share:' || src.share_id AS storage_key,
					s.rel_path AS rel_path, s.bytes AS bytes, s.sha256 AS sha256
				FROM segments s
				JOIN sources src ON src.id = s.source_id
				WHERE src.share_id IS NOT NULL

				UNION ALL

				SELECT
					'ingest', i.id, i.share_id, NULL,
					'share:' || i.share_id,
					i.dest_rel_path, i.bytes, COALESCE(i.sha256, seg.sha256)
				FROM ingests i
				LEFT JOIN segments seg ON seg.id = i.segment_id
				WHERE i.verified_at IS NOT NULL

				UNION ALL

				SELECT
					'pull', p.id, p.share_id, NULL,
					'share:' || p.share_id,
					p.dest_rel_path, p.bytes, COALESCE(p.sha256, pseg.sha256)
				FROM dashcam_pulls p
				LEFT JOIN segments pseg ON pseg.id = p.indexed_segment_id
				WHERE p.verified_at IS NOT NULL

				UNION ALL

				SELECT
					'export', e.id, e.share_id, NULL,
					CASE WHEN e.share_id IS NULL THEN 'cache' ELSE 'share:' || e.share_id END,
					e.path, e.bytes,
					-- Fix round 1, Important 3: prefer a matching storage_files
					-- row's sha256 over leaving this NULL forever -- an export
					-- landed on a share target sits on disk exactly where the
					-- scan walker (task 5) also indexes it into storage_files,
					-- which DOES get hashed by hash_backfill; without this an
					-- export could never leave the size_name (probable) bucket
					-- even once its file is fully hashed.
					(SELECT sf3.sha256 FROM storage_files sf3
						JOIN shares esh3 ON esh3.id = e.share_id
						WHERE sf3.share_id = e.share_id AND esh3.mount_point || '/' || sf3.rel_path = e.path
						LIMIT 1)
				FROM exports e
				WHERE e.path IS NOT NULL

				UNION ALL

				SELECT
					'storage_file', sf.id, sf.share_id, NULL,
					'share:' || sf.share_id,
					sf.rel_path, sf.bytes, sf.sha256
				FROM storage_files sf
				WHERE NOT EXISTS (
						SELECT 1 FROM ingests i2
						WHERE i2.share_id = sf.share_id AND i2.dest_rel_path = sf.rel_path AND i2.verified_at IS NOT NULL
					)
					AND NOT EXISTS (
						SELECT 1 FROM dashcam_pulls p2
						WHERE p2.share_id = sf.share_id AND p2.dest_rel_path = sf.rel_path AND p2.verified_at IS NOT NULL
					)
					AND NOT EXISTS (
						SELECT 1 FROM segments s2
						JOIN sources src2 ON src2.id = s2.source_id
						WHERE src2.share_id = sf.share_id AND s2.rel_path = sf.rel_path
					)
					AND NOT EXISTS (
						SELECT 1 FROM exports e2
						JOIN shares esh ON esh.id = e2.share_id
						WHERE e2.share_id = sf.share_id AND e2.path = esh.mount_point || '/' || sf.rel_path
					)
			`,
			`CREATE TABLE IF NOT EXISTS duplicate_groups (
				group_key TEXT PRIMARY KEY,
				kind TEXT NOT NULL,
				copies INTEGER NOT NULL,
				storages INTEGER NOT NULL,
				bytes INTEGER NOT NULL,
				sample_name TEXT NOT NULL,
				refreshed_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_duplicate_groups_kind ON duplicate_groups(kind)`,
			`CREATE TABLE IF NOT EXISTS duplicate_group_members (
				group_key TEXT NOT NULL REFERENCES duplicate_groups(group_key) ON DELETE CASCADE,
				ref_kind TEXT NOT NULL,
				ref_id INTEGER NOT NULL,
				share_id INTEGER,
				source_id INTEGER,
				storage_key TEXT NOT NULL,
				rel_path TEXT NOT NULL,
				hashed INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (group_key, ref_kind, ref_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_duplicate_group_members_storage_key ON duplicate_group_members(storage_key)`,
			`CREATE INDEX IF NOT EXISTS idx_duplicate_group_members_ref ON duplicate_group_members(ref_kind, ref_id)`,
		},
	},
}

// postMigrationStatements are idempotent (IF NOT EXISTS) DDL that depend on
// a column added by a migration above rather than always being present in
// schemaStatements' CREATE TABLE — see InitSchema, which runs these after
// both schemaStatements and the migrations loop so the column is
// guaranteed to exist by then regardless of whether this database was
// fresh or just migrated.
var postMigrationStatements = []string{
	// GET /api/segments/unreadable pages this set ordered by unreadable_at;
	// partial (WHERE unreadable_at IS NOT NULL) keeps the index small since
	// only a small fraction of segments are ever unreadable.
	`CREATE INDEX IF NOT EXISTS idx_segments_unreadable_at ON segments(unreadable_at) WHERE unreadable_at IS NOT NULL`,
}

// InitSchema creates every table, index, trigger and the FTS5 search table
// if they don't already exist, then applies any pending migrations. It
// reads settings["schema_version"] first:
//   - missing (fresh database): proceed as a brand-new install, run the DDL,
//     and stamp schema_version = SchemaVersion directly.
//   - present and > SchemaVersion: fail fast rather than running
//     (possibly-mismatched) DDL against a database a newer binary has
//     already migrated — upgrade the binary instead.
//   - present and < baselineSchemaVersion: fail with a clear "too old"
//     error — see baselineSchemaVersion's doc comment. The fix is to
//     delete the database file and re-scan from the (read-only, so
//     nothing is lost) source(s).
//   - present and in [baselineSchemaVersion, SchemaVersion]: run the
//     (idempotent) DDL, apply any migrations whose From >= the stored
//     version, and bump the stored version to SchemaVersion.
//
// Safe to call on every boot.
func (d *DB) InitSchema(ctx context.Context) error {
	// settingsTableStmt and the schema_version read/validate run in
	// autocommit (outside any transaction), on the same single connection
	// every write uses (see DB's doc comment — the write pool is capped at
	// one open connection): migrateSourcesToV8DashcamKind, called below
	// when a pre-v8 database is found, needs to toggle PRAGMA foreign_keys
	// off and back on, which sqlite only honors outside a transaction, so
	// this whole preamble has to happen before the main Tx (further down)
	// opens one.
	if _, err := d.write.ExecContext(ctx, settingsTableStmt); err != nil {
		return fmt.Errorf("db: init schema: %w\n%s", err, settingsTableStmt)
	}

	var stored string
	fresh := false
	err := d.write.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = 'schema_version'`,
	).Scan(&stored)
	var version int
	switch {
	case err == nil:
		v, convErr := strconv.Atoi(stored)
		if convErr != nil {
			return fmt.Errorf("db: schema_version value %q is not a valid integer", stored)
		}
		version = v
		if version > SchemaVersion {
			return fmt.Errorf(
				"db: database schema_version %d is newer than this binary supports (%d); refusing to touch it — upgrade the binary",
				version, SchemaVersion)
		}
		if version < baselineSchemaVersion {
			return fmt.Errorf(
				"db: database schema too old (v%d < %d): delete the database file and re-scan (the source(s) it was scanned from are the source of truth and are never modified)",
				version, baselineSchemaVersion)
		}
	case errors.Is(err, sql.ErrNoRows):
		// No stored version yet: fresh database, proceed as a brand-new
		// install straight to SchemaVersion — there's nothing to
		// migrate from.
		fresh = true
		version = SchemaVersion
	default:
		return fmt.Errorf("db: read schema_version: %w", err)
	}

	// A fresh install's sources table is about to be created (below, by
	// schemaStatements) directly in its v8 shape, so there's nothing to
	// rebuild. An existing (< v8) database's sources table predates
	// 'dashcam' being a valid kind and the base_url/target_share_id/
	// pull_cursor/last_pull_at columns — bring it up to the v8 shape now,
	// before schemaStatements/migrations run inside the Tx below.
	if !fresh && version < SchemaVersion {
		if err := d.migrateSourcesToV8DashcamKind(ctx); err != nil {
			return err
		}
	}

	// Captured before the migrations loop below mutates version, so
	// migrateSharesToV9Kind can tell whether this database is actually
	// crossing the v8->v9 boundary (and so might still have the
	// allow_local_share_paths setting to migrate) rather than starting
	// already at or past v9.
	preMigrationVersion := version

	return d.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range schemaStatements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("db: init schema: %w\n%s", err, stmt)
			}
		}

		if !fresh {
			for _, m := range migrations {
				if version < m.From || version >= m.To {
					continue
				}
				for _, stmt := range m.Statements {
					if _, err := tx.ExecContext(ctx, stmt); err != nil {
						// ALTER TABLE ADD COLUMN has no "IF NOT EXISTS" form
						// in sqlite, unlike every idempotent statement in
						// schemaStatements above — a database whose stored
						// schema_version was reset/corrected to below this
						// migration's From (e.g. tests exercising InitSchema
						// against a "baseline version" database that in fact
						// already has the physical column, since it went
						// through the fresh-install CREATE TABLE path) would
						// otherwise fail here even though there's nothing
						// left to do. Treat "duplicate column name" as
						// already-applied rather than an error.
						if isDuplicateColumnErr(err) {
							continue
						}
						return fmt.Errorf("db: migration v%d->v%d: %w\n%s", m.From, m.To, err, stmt)
					}
				}
				version = m.To
			}
		}

		// The v8->v9 data migration (promote shares.kind from the removed
		// allow_local_share_paths setting) only makes sense for a database
		// that actually crossed that boundary just now — a fresh install
		// never had the setting, and one already at v9+ has already had
		// its chance (and the setting, if any, is long gone).
		if !fresh && preMigrationVersion < 9 {
			if err := migrateSharesToV9Kind(ctx, tx); err != nil {
				return err
			}
		}

		// postMigrationStatements run unconditionally, after schemaStatements
		// and any migration above — by this point every database (fresh,
		// via schemaStatements' CREATE TABLE columns, or migrated, via an
		// ALTER TABLE above) definitely has every column any of these
		// statements reference, which schemaStatements itself can't
		// guarantee for a not-yet-migrated existing database.
		for _, stmt := range postMigrationStatements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("db: init schema (post-migration): %w\n%s", err, stmt)
			}
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO settings (key, value_json) VALUES ('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json`,
			strconv.Itoa(SchemaVersion),
		)
		return err
	})
}

// migrateSourcesToV8DashcamKind rebuilds the sources table for the v7->v8
// schema bump — see the v7->v8 comment in `migrations` above for why this
// can't be an ordinary ALTER TABLE ADD COLUMN entry there. The rebuild
// itself is sqlite's standard "create the new shape, copy the data, drop
// the old table, rename" dance for a CHECK/column change ALTER TABLE can't
// express directly.
//
// It runs in its own transaction, bracketed by PRAGMA foreign_keys=OFF /
// ON on the shared write connection, entirely outside InitSchema's own Tx
// — two things forced together: PRAGMA foreign_keys is a silent no-op
// when toggled inside a transaction (verified against modernc.org/sqlite;
// not documented behavior InitSchema's callers should have to rediscover),
// so it must be set before BEGIN; and it must be off for the DROP TABLE
// step, because sqlite treats DROP TABLE as an implicit DELETE FROM when
// foreign_keys=ON — which would fire trips.source_id and segments.source_id's
// ON DELETE CASCADE against every row referencing this source and wipe the
// entire index. (This is exactly the failure mode final-fix-wave item 2
// replaced sources.share_id's CASCADE with SET NULL to avoid — see that
// column's doc comment above.) INSERT INTO sources_new preserves every
// row's original id, so once foreign_keys is back on, every existing
// trips/segments row's source_id still resolves correctly.
//
// Idempotent: if sources already has the v8 marker column (base_url), this
// is a no-op — covers InitSchema being called again after a crash between
// this step's commit and InitSchema's own Tx (which bumps schema_version)
// committing.
func (d *DB) migrateSourcesToV8DashcamKind(ctx context.Context) (err error) {
	has, err := d.hasColumn(ctx, "sources", "base_url")
	if err != nil {
		return fmt.Errorf("db: migrate sources to v8: %w", err)
	}
	if has {
		return nil
	}

	if _, err := d.write.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("db: migrate sources to v8: disable foreign_keys: %w", err)
	}
	// Always try to restore enforcement, even if the rebuild below fails —
	// leaving the shared write connection with foreign_keys silently off
	// for the rest of the process would be a far worse outcome than a
	// failed migration. Deliberately uses context.Background() rather than
	// ctx: this must still run (and its own failure must still be
	// reported, not silently discarded) even if ctx was already canceled
	// or timed out by the time the rebuild below finishes.
	defer func() {
		if _, restoreErr := d.write.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); restoreErr != nil {
			restoreErr = fmt.Errorf("db: migrate sources to v8: re-enable foreign_keys: %w", restoreErr)
			if err != nil {
				err = fmt.Errorf("%w (additionally, restoring foreign_keys failed: %v)", err, restoreErr)
			} else {
				err = restoreErr
			}
		}
	}()

	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: migrate sources to v8: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	stmts := []string{
		// Identical to schemaStatements' sources CREATE TABLE above (the
		// fresh-install shape), just named sources_new for the swap.
		`CREATE TABLE sources_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL CHECK (kind IN ('ssd','share','dashcam')),
			share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
			root_path TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			last_scan_at TEXT,
			status TEXT NOT NULL DEFAULT 'idle',
			base_url TEXT,
			target_share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
			pull_cursor TEXT,
			last_pull_at TEXT,
			UNIQUE(root_path)
		)`,
		`INSERT INTO sources_new (id, kind, share_id, root_path, label, last_scan_at, status)
			SELECT id, kind, share_id, root_path, label, last_scan_at, status FROM sources`,
		`DROP TABLE sources`,
		`ALTER TABLE sources_new RENAME TO sources`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("db: migrate sources to v8: %w\n%s", err, stmt)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: migrate sources to v8: commit: %w", err)
	}
	return nil
}

// migrateSharesToV9Kind is the data half of the v8->v9 migration (the DDL
// half — ALTER TABLE shares ADD COLUMN kind — is the `migrations` entry
// above, which runs immediately before this in the same Tx). The hidden
// allow_local_share_paths setting used to make ResolveTargetMount trust
// every share's stored mount_point as a plain local directory instead of
// requiring a real CIFS mount — i.e. it made every share row behave
// exactly like kind='local' behaves now. So when that setting was on,
// every share row is promoted to kind='local' (logged, since this
// silently changes how ResolveTargetMount/share/health treat them), and
// the setting is deleted either way — kind is the only source of truth
// from here on. A no-op (including the delete) when the setting was never
// set or already off.
func migrateSharesToV9Kind(ctx context.Context, tx *sql.Tx) error {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value_json FROM settings WHERE key = 'allow_local_share_paths'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: migrate shares to v9 kind: read allow_local_share_paths: %w", err)
	}

	if strings.TrimSpace(raw) == "true" {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM shares WHERE kind != 'local'`)
		if err != nil {
			return fmt.Errorf("db: migrate shares to v9 kind: list shares: %w", err)
		}
		var flipped []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("db: migrate shares to v9 kind: scan share id: %w", err)
			}
			flipped = append(flipped, id)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("db: migrate shares to v9 kind: list shares: %w", err)
		}
		if len(flipped) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE shares SET kind = 'local' WHERE kind != 'local'`); err != nil {
				return fmt.Errorf("db: migrate shares to v9 kind: %w", err)
			}
			log.Printf("[db] migrate v8->v9: allow_local_share_paths was on; flipped shares to kind='local': %v", flipped)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = 'allow_local_share_paths'`); err != nil {
		return fmt.Errorf("db: migrate shares to v9 kind: delete setting: %w", err)
	}
	return nil
}

// hasColumn reports whether table has a column named column, via sqlite's
// PRAGMA table_info — used by migrateSourcesToV8DashcamKind to detect
// "already migrated" without depending on the stored schema_version (which
// may not have been bumped yet if a previous InitSchema run crashed
// between the two).
func (d *DB) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := d.write.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// isDuplicateColumnErr reports whether err is sqlite's "duplicate column
// name" error, i.e. an ALTER TABLE ADD COLUMN migration statement running
// against a table that already has that column.
func isDuplicateColumnErr(err error) bool {
	return strings.Contains(err.Error(), "duplicate column name")
}
