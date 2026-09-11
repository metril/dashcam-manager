// queries_dashcam.go is the data-access layer for schema v8's dashcam
// support: kind='dashcam' rows in `sources` (a network dashcam pulled over
// its own HTTP(S) API, rather than an SSD scanned off a mount or a share
// scanned off SMB) and the `dashcam_pulls` table that tracks each remote
// file pulled from one. See internal/dashcam (a separate package, owned by
// a concurrent workstream) for the HTTP client and pull job that call
// these.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DashcamList returns every kind='dashcam' source, ordered by id.
func (d *DB) DashcamList(ctx context.Context) ([]Source, error) {
	rows, err := d.read.QueryContext(ctx, sourceSelectCols+` WHERE kind = 'dashcam' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: dashcam list: %w", err)
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("db: dashcam list: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DashcamGet returns the kind='dashcam' source with the given id, or
// ErrNotFound (including for an id whose source exists but isn't a
// dashcam).
func (d *DB) DashcamGet(ctx context.Context, id int64) (Source, error) {
	s, err := scanSource(d.read.QueryRowContext(ctx,
		sourceSelectCols+` WHERE id = ? AND kind = 'dashcam'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, fmt.Errorf("db: dashcam get %d: %w", id, err)
	}
	return s, nil
}

// DashcamCreate inserts a new kind='dashcam' source and returns its id.
// A dashcam has no local mount path (its files live behind baseURL, over
// the network) — but sources.root_path is UNIQUE and NOT NULL, so leaving
// it '' would only work for the first dashcam ever created; every
// subsequent one would fail that UNIQUE constraint. Instead root_path is
// set, in a follow-up UPDATE once the new id is known, to the synthetic
// "dashcam:<id>" — guaranteed unique (ids are), and stable regardless of
// whether two dashcams happen to share a base_url. status starts 'unknown'
// (matching shares' own not-yet-probed default; sources otherwise default
// to 'idle', which doesn't fit a source nothing has connected to yet).
func (d *DB) DashcamCreate(ctx context.Context, label, baseURL string, targetShareID int64) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO sources (kind, root_path, label, status, base_url, target_share_id)
			 VALUES ('dashcam', '', ?, 'unknown', ?, ?) RETURNING id`,
			label, baseURL, targetShareID,
		).Scan(&id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE sources SET root_path = ? WHERE id = ?`, fmt.Sprintf("dashcam:%d", id), id)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("db: dashcam create: %w", err)
	}
	return id, nil
}

// DashcamUpdate updates a dashcam source's label, base URL and pull
// target. enabled is accepted for forward compatibility with a future
// "pause this dashcam's pull job" toggle, but is a no-op today: sources
// has no enabled column (only shares does), and nothing in this task adds
// one. Returns ErrNotFound if id isn't a kind='dashcam' source.
func (d *DB) DashcamUpdate(ctx context.Context, id int64, label, baseURL string, targetShareID int64, enabled bool) error {
	_ = enabled
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sources SET label = ?, base_url = ?, target_share_id = ?
			 WHERE id = ? AND kind = 'dashcam'`,
			label, baseURL, targetShareID, id)
		if err != nil {
			return fmt.Errorf("db: dashcam update %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: dashcam update %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DashcamDelete deletes the kind='dashcam' source with id, cascading (per
// dashcam_pulls.dashcam_source_id's ON DELETE CASCADE) to every pull
// recorded for it. Returns ErrNotFound if id isn't a kind='dashcam'
// source.
func (d *DB) DashcamDelete(ctx context.Context, id int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id = ? AND kind = 'dashcam'`, id)
		if err != nil {
			return fmt.Errorf("db: dashcam delete %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: dashcam delete %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DashcamSetCursor records the dashcam pull job's resume position (an
// opaque token from the dashcam's list endpoint) and when the pull last
// completed. Returns ErrNotFound if id isn't a kind='dashcam' source.
func (d *DB) DashcamSetCursor(ctx context.Context, id int64, cursor string, lastPullAt time.Time) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sources SET pull_cursor = ?, last_pull_at = ? WHERE id = ? AND kind = 'dashcam'`,
			cursor, lastPullAt.UTC().Format(time.RFC3339Nano), id)
		if err != nil {
			return fmt.Errorf("db: dashcam set cursor %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: dashcam set cursor %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DashcamPull is one row of the `dashcam_pulls` table: a file pulled from
// a dashcam source, tracked through copy -> verify -> (once the
// destination share is scanned) segment linkage -> cleanup-on-the-dashcam,
// mirroring ingests' segment-out-to-share lifecycle for a dashcam-in
// instead.
type DashcamPull struct {
	ID               int64
	DashcamSourceID  int64
	RemoteRelPath    string
	ShareID          int64
	DestRelPath      string
	Bytes            int64
	SHA256           *string
	Mtime            *string
	PulledAt         string
	VerifiedAt       *string
	IndexedSegmentID *int64
	DeletedRemoteAt  *string
}

const dashcamPullSelectCols = `SELECT id, dashcam_source_id, remote_rel_path, share_id, dest_rel_path, bytes, sha256, mtime, pulled_at, verified_at, indexed_segment_id, deleted_remote_at FROM dashcam_pulls`

func scanDashcamPull(row interface{ Scan(...any) error }) (DashcamPull, error) {
	var p DashcamPull
	err := row.Scan(&p.ID, &p.DashcamSourceID, &p.RemoteRelPath, &p.ShareID, &p.DestRelPath, &p.Bytes,
		&p.SHA256, &p.Mtime, &p.PulledAt, &p.VerifiedAt, &p.IndexedSegmentID, &p.DeletedRemoteAt)
	return p, err
}

// PullUpsert records a copy attempt for (p.DashcamSourceID,
// p.RemoteRelPath): insert if this is the first time that remote file has
// been pulled, or update in place if it's been pulled before (a retry, or
// the dashcam job re-scanning its remote listing).
//
// On an update, verified_at and deleted_remote_at are reset to NULL only
// when bytes or sha256 differ from the stored row — i.e. only when the
// remote file has actually changed since it was last pulled/verified. An
// update that just repeats the same bytes/hash (e.g. a re-run of the same
// pull) leaves a previously-verified row verified, rather than forcing an
// unnecessary re-verify. indexed_segment_id is left untouched either way:
// it's PullsLinkSegments' job to set it once, later, and an upsert here
// shouldn't be able to un-link an already-indexed pull.
func (d *DB) PullUpsert(ctx context.Context, p DashcamPull) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO dashcam_pulls
				(dashcam_source_id, remote_rel_path, share_id, dest_rel_path, bytes, sha256, mtime, pulled_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(dashcam_source_id, remote_rel_path) DO UPDATE SET
				share_id = excluded.share_id,
				dest_rel_path = excluded.dest_rel_path,
				bytes = excluded.bytes,
				sha256 = excluded.sha256,
				mtime = excluded.mtime,
				pulled_at = excluded.pulled_at,
				verified_at = CASE
					WHEN dashcam_pulls.bytes != excluded.bytes
					     OR IFNULL(dashcam_pulls.sha256, '') != IFNULL(excluded.sha256, '')
					THEN NULL ELSE dashcam_pulls.verified_at END,
				deleted_remote_at = CASE
					WHEN dashcam_pulls.bytes != excluded.bytes
					     OR IFNULL(dashcam_pulls.sha256, '') != IFNULL(excluded.sha256, '')
					THEN NULL ELSE dashcam_pulls.deleted_remote_at END
			RETURNING id`,
			p.DashcamSourceID, p.RemoteRelPath, p.ShareID, p.DestRelPath, p.Bytes, p.SHA256, p.Mtime, p.PulledAt,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: dashcam pull upsert: %w", err)
	}
	return id, nil
}

// PullMarkVerified sets verified_at, and — only when sha256 is non-empty —
// sha256, on an existing dashcam_pulls row. sha256 is optional here (a
// caller that already recorded the hash via PullUpsert and is just
// confirming the destination's size/bytes doesn't need to pass it again)
// and an empty string must never clobber a previously-recorded hash;
// COALESCE(NULLIF(?,''), sha256) keeps the existing value whenever the
// argument is empty.
func (d *DB) PullMarkVerified(ctx context.Context, id int64, sha256 string, at time.Time) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE dashcam_pulls SET verified_at = ?, sha256 = COALESCE(NULLIF(?, ''), sha256) WHERE id = ?`,
			at.UTC().Format(time.RFC3339Nano), sha256, id)
		if err != nil {
			return fmt.Errorf("db: dashcam pull mark verified %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: dashcam pull mark verified %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// PullGet returns the dashcam_pulls row for (dashcamID, remoteRelPath), or
// (zero value, false, nil) if none exists yet — the normal case for a
// remote file's first pull attempt, not treated as an error (matching
// IngestGet's convention for the same "not found yet" case in `ingests`).
func (d *DB) PullGet(ctx context.Context, dashcamID int64, remoteRelPath string) (DashcamPull, bool, error) {
	p, err := scanDashcamPull(d.read.QueryRowContext(ctx,
		dashcamPullSelectCols+` WHERE dashcam_source_id = ? AND remote_rel_path = ?`, dashcamID, remoteRelPath))
	if errors.Is(err, sql.ErrNoRows) {
		return DashcamPull{}, false, nil
	}
	if err != nil {
		return DashcamPull{}, false, fmt.Errorf("db: dashcam pull get (dashcam %d, %q): %w", dashcamID, remoteRelPath, err)
	}
	return p, true, nil
}

// PullCleanupCandidates returns every dashcamID pull that's verified,
// linked to an indexed segment, and not yet marked deleted off the
// dashcam — i.e. safe for the pull job to delete the remote copy of, the
// same three-part guard CleanupCandidates (queries_ingest.go) applies to
// an SSD original before it's deleted.
func (d *DB) PullCleanupCandidates(ctx context.Context, dashcamID int64) ([]DashcamPull, error) {
	rows, err := d.read.QueryContext(ctx,
		dashcamPullSelectCols+`
			WHERE dashcam_source_id = ?
			  AND verified_at IS NOT NULL
			  AND indexed_segment_id IS NOT NULL
			  AND deleted_remote_at IS NULL
			ORDER BY id`, dashcamID)
	if err != nil {
		return nil, fmt.Errorf("db: dashcam pull cleanup candidates %d: %w", dashcamID, err)
	}
	defer rows.Close()

	var out []DashcamPull
	for rows.Next() {
		p, err := scanDashcamPull(rows)
		if err != nil {
			return nil, fmt.Errorf("db: dashcam pull cleanup candidates %d: %w", dashcamID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PullMarkDeletedRemote stamps deleted_remote_at (now, UTC RFC3339Nano) on
// every dashcam_pulls row in ids — called once the pull job has confirmed
// it deleted each file off the dashcam itself. A no-op for an empty ids.
func (d *DB) PullMarkDeletedRemote(ctx context.Context, ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		const chunk = 500
		stamp := at.UTC().Format(time.RFC3339Nano)
		for i := 0; i < len(ids); i += chunk {
			end := i + chunk
			if end > len(ids) {
				end = len(ids)
			}
			part := ids[i:end]
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
			args := make([]any, 0, len(part)+1)
			args = append(args, stamp)
			for _, id := range part {
				args = append(args, id)
			}
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE dashcam_pulls SET deleted_remote_at = ? WHERE id IN (%s)`, placeholders),
				args...); err != nil {
				return fmt.Errorf("db: dashcam pull mark deleted remote: %w", err)
			}
		}
		return nil
	})
}

// PullsLinkSegments links every not-yet-linked dashcam_pulls row under
// dashcamID to the segments row IndexDetections created for it, once
// shareSourceID (the share the dashcam's files were pulled onto) has been
// scanned and indexed: a pull's dest_rel_path is exactly the rel_path the
// scanner records for the resulting segment (both are paths relative to
// the same share mount), so the two are joined on that. Returns how many
// rows were linked.
func (d *DB) PullsLinkSegments(ctx context.Context, dashcamID, shareSourceID int64) (int, error) {
	var n int
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE dashcam_pulls
			SET indexed_segment_id = (
				SELECT s.id FROM segments s
				WHERE s.source_id = ? AND s.rel_path = dashcam_pulls.dest_rel_path
			)
			WHERE dashcam_pulls.dashcam_source_id = ?
			  AND dashcam_pulls.indexed_segment_id IS NULL
			  AND EXISTS (
				SELECT 1 FROM segments s
				WHERE s.source_id = ? AND s.rel_path = dashcam_pulls.dest_rel_path
			  )`,
			shareSourceID, dashcamID, shareSourceID)
		if err != nil {
			return fmt.Errorf("db: dashcam pulls link segments: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: dashcam pulls link segments: %w", err)
		}
		n = int(affected)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
