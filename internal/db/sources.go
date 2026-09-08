package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Source is a scannable root (the read-only SSD mount, or a mounted SMB
// share) that segments/trips are attached to via source_id.
type Source struct {
	ID         int64
	Kind       string // "ssd" | "share"
	ShareID    *int64
	RootPath   string
	Label      string
	LastScanAt *string
	Status     string
}

// SourcesList returns every configured source, ordered by id.
func (d *DB) SourcesList(ctx context.Context) ([]Source, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id, kind, share_id, root_path, label, last_scan_at, status FROM sources ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: sources list: %w", err)
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ID, &s.Kind, &s.ShareID, &s.RootPath, &s.Label, &s.LastScanAt, &s.Status); err != nil {
			return nil, fmt.Errorf("db: sources list: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SourceGet returns the source with the given id, or ErrNotFound.
func (d *DB) SourceGet(ctx context.Context, id int64) (Source, error) {
	var s Source
	err := d.read.QueryRowContext(ctx,
		`SELECT id, kind, share_id, root_path, label, last_scan_at, status FROM sources WHERE id = ?`, id,
	).Scan(&s.ID, &s.Kind, &s.ShareID, &s.RootPath, &s.Label, &s.LastScanAt, &s.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, fmt.Errorf("db: source get %d: %w", id, err)
	}
	return s, nil
}

// SourceCreate inserts a new source and returns its id. kind must be "ssd"
// or "share" (enforced by the schema's CHECK constraint).
func (d *DB) SourceCreate(ctx context.Context, kind, rootPath, label string) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO sources (kind, root_path, label) VALUES (?, ?, ?) RETURNING id`,
			kind, rootPath, label,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: source create: %w", err)
	}
	return id, nil
}

// SourceEnsure returns the id of the source with the given root_path,
// creating it (with kind and label) if it doesn't already exist. Used to
// seed the SSD source from DASHCAM_SSD_PATH at boot without ever
// duplicating or clobbering a user-edited row.
func (d *DB) SourceEnsure(ctx context.Context, kind, rootPath, label string) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM sources WHERE root_path = ?`, rootPath).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.QueryRowContext(ctx,
			`INSERT INTO sources (kind, root_path, label) VALUES (?, ?, ?) RETURNING id`,
			kind, rootPath, label,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: source ensure: %w", err)
	}
	return id, nil
}

// SourceGetByShareID returns the source with kind='share' and the given
// share_id, or ErrNotFound.
func (d *DB) SourceGetByShareID(ctx context.Context, shareID int64) (Source, error) {
	var s Source
	err := d.read.QueryRowContext(ctx,
		`SELECT id, kind, share_id, root_path, label, last_scan_at, status FROM sources WHERE kind = 'share' AND share_id = ?`, shareID,
	).Scan(&s.ID, &s.Kind, &s.ShareID, &s.RootPath, &s.Label, &s.LastScanAt, &s.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, fmt.Errorf("db: source get by share_id %d: %w", shareID, err)
	}
	return s, nil
}

// SourceSegmentCount returns how many segments rows reference sourceID —
// used by handleSharesDelete to decide whether deleting a share would
// (without ?purge_index=1) discard an otherwise-healthy index.
func (d *DB) SourceSegmentCount(ctx context.Context, sourceID int64) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE source_id = ?`, sourceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: source segment count %d: %w", sourceID, err)
	}
	return n, nil
}

// SourceDelete deletes the source row with id, cascading (per the schema's
// ON DELETE CASCADE from segments/trips.source_id) to every segment, trip,
// and detection indexed under it. Returns ErrNotFound if no such source
// exists.
func (d *DB) SourceDelete(ctx context.Context, id int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("db: source delete %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("db: source delete %d: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SourceSetStatus updates a source's status column (e.g. "idle", "scanning",
// "error").
func (d *DB) SourceSetStatus(ctx context.Context, id int64, status string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sources SET status = ? WHERE id = ?`, status, id)
		return err
	})
}

// SourceSetLastScanAt updates a source's last_scan_at (RFC3339 UTC string)
// and status together, as a scan completes.
func (d *DB) SourceSetLastScanAt(ctx context.Context, id int64, lastScanAt, status string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sources SET last_scan_at = ?, status = ? WHERE id = ?`, lastScanAt, status, id)
		return err
	})
}
