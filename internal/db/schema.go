package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// SchemaVersion is recorded in settings["schema_version"]. InitSchema bumps
// an older (or missing) stored version up to this on success, and refuses
// to touch a database stamped with a newer version than this binary knows
// about.
const SchemaVersion = 3

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
		last_mounted_at TEXT
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
	`CREATE TABLE IF NOT EXISTS sources (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL CHECK (kind IN ('ssd','share')),
		share_id INTEGER REFERENCES shares(id) ON DELETE SET NULL,
		root_path TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		last_scan_at TEXT,
		status TEXT NOT NULL DEFAULT 'idle',
		UNIQUE(root_path)
	)`,

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
		UNIQUE(source_id, rel_path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_start_time ON segments(start_time)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_trip_camera_seq ON segments(trip_id, camera, seq)`,
	// M12: db.Stats' per-source storage and ingest-coverage panels
	// (internal/db/queries_browse.go) group/filter by source_id.
	`CREATE INDEX IF NOT EXISTS idx_segments_source_id ON segments(source_id)`,

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
		x1 REAL, y1 REAL, x2 REAL, y2 REAL
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
		x1 REAL, y1 REAL, x2 REAL, y2 REAL
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
		x1 REAL, y1 REAL, x2 REAL, y2 REAL
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
	// M12: db.Stats' ingest-coverage panel checks, per SSD-sourced segment,
	// "EXISTS (SELECT 1 FROM ingests WHERE segment_id = ? AND verified_at
	// IS NOT NULL)" — segment_id already leads the UNIQUE(segment_id,
	// share_id) index above, but an explicit index makes that independent
	// of the unique constraint's column order ever changing.
	`CREATE INDEX IF NOT EXISTS idx_ingests_segment_id ON ingests(segment_id)`,

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
// on every boot where that holds. Empty for now — SchemaVersion ==
// baselineSchemaVersion, so there's nothing to migrate yet — but gives
// future schema bumps (SchemaVersion > baselineSchemaVersion) a place to
// land without having to touch InitSchema's control flow again: append a
// migration here, bump SchemaVersion, and any database already at >=
// baselineSchemaVersion picks it up on next boot.
type migration struct {
	From, To   int
	Statements []string
}

var migrations = []migration{}

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
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, settingsTableStmt); err != nil {
			return fmt.Errorf("db: init schema: %w\n%s", err, settingsTableStmt)
		}

		var stored string
		fresh := false
		err := tx.QueryRowContext(ctx,
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
						return fmt.Errorf("db: migration v%d->v%d: %w\n%s", m.From, m.To, err, stmt)
					}
				}
				version = m.To
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
