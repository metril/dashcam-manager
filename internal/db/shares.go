package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// KindCIFS and KindLocal are the valid values of Share.Kind /
// ShareCreateParams.Kind (schema v9). KindCIFS (the default, matching the
// column's DEFAULT 'cifs') is mounted via mount.cifs the same as always;
// KindLocal is an already-local/bind-mounted directory — ResolveTargetMount
// (internal/ingest/ingest.go) and share/health.go dispatch on this instead
// of the removed allow_local_share_paths hidden setting.
const (
	KindCIFS  = "cifs"
	KindLocal = "local"
)

// Share is a configured storage share, either CIFS (mounted via
// mount.cifs, internal/share mounts it under mount_point and
// internal/share's health loop keeps Status current) or, since schema v9,
// a local directory (Kind == KindLocal). PasswordEnc holds the
// AES-256-GCM ciphertext of the share's password (never the plaintext) —
// nil until a password has been set; always nil/unused for a local share.
type Share struct {
	ID            int64
	Name          string
	Server        string
	ShareName     string // "share" column: the SMB share name, e.g. "footage"
	Subpath       string
	Username      string
	PasswordEnc   []byte
	Domain        string
	MountPoint    string
	Options       string
	Enabled       bool
	Status        string
	LastMountedAt *string
	Kind          string // KindCIFS (default) or KindLocal
}

const shareSelectCols = `SELECT id, name, server, share, subpath, username, password_enc, domain, mount_point, options, enabled, status, last_mounted_at, kind`

// SharesList returns every configured share, ordered by id.
func (d *DB) SharesList(ctx context.Context) ([]Share, error) {
	rows, err := d.read.QueryContext(ctx, shareSelectCols+` FROM shares ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: shares list: %w", err)
	}
	defer rows.Close()

	var out []Share
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, fmt.Errorf("db: shares list: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ShareGet returns the share with the given id, or ErrNotFound.
func (d *DB) ShareGet(ctx context.Context, id int64) (Share, error) {
	row := d.read.QueryRowContext(ctx, shareSelectCols+` FROM shares WHERE id = ?`, id)
	s, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, ErrNotFound
	}
	if err != nil {
		return Share{}, fmt.Errorf("db: share get %d: %w", id, err)
	}
	return s, nil
}

// ShareCreateParams is the input to ShareCreate.
type ShareCreateParams struct {
	Name        string
	Server      string
	ShareName   string
	Subpath     string
	Username    string
	PasswordEnc []byte
	Domain      string
	MountPoint  string
	Options     string
	Enabled     bool
	// Kind is KindCIFS or KindLocal; empty defaults to KindCIFS. Callers
	// outside this package (the /api/shares handlers) are responsible for
	// validating it — see internal/db.KindCIFS/KindLocal's doc comment —
	// before it reaches here.
	Kind string
}

// normalizeKind defaults an empty Kind to KindCIFS, matching the shares.kind
// column's own DEFAULT — callers that build the INSERT/UPDATE explicitly
// listing every column (as ShareCreate/ShareCreateWithSource do) bypass
// that column default, so it has to be reproduced here.
func normalizeKind(kind string) string {
	if kind == "" {
		return KindCIFS
	}
	return kind
}

// ShareCreate inserts a new share and returns its id. Exposed mainly for
// tests; production code should use ShareCreateWithSource so the share row
// and its linked source are created atomically.
func (d *DB) ShareCreate(ctx context.Context, p ShareCreateParams) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO shares (name, server, share, subpath, username, password_enc, domain, mount_point, options, enabled, status, kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'unknown', ?) RETURNING id`,
			p.Name, p.Server, p.ShareName, p.Subpath, p.Username, p.PasswordEnc, p.Domain, p.MountPoint, p.Options, p.Enabled, normalizeKind(p.Kind),
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: share create: %w", err)
	}
	return id, nil
}

// ShareCreateWithSource inserts a new share and its linked "share"-kind
// source in one transaction, so a failure creating the source (e.g. a
// unique root_path collision) rolls back the share insert too rather than
// leaving an orphaned share with no source. mountPointFor is called with
// the newly-assigned share id (mount_point can't be computed until the id
// exists, since the mount point is scoped to it — see share.Slug) and its
// result is both stored as the share's mount_point and used as the
// source's root_path.
func (d *DB) ShareCreateWithSource(ctx context.Context, p ShareCreateParams, mountPointFor func(id int64) string, sourceLabel string) (shareID int64, sourceID int64, err error) {
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO shares (name, server, share, subpath, username, password_enc, domain, mount_point, options, enabled, status, kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, 'unknown', ?) RETURNING id`,
			p.Name, p.Server, p.ShareName, p.Subpath, p.Username, p.PasswordEnc, p.Domain, p.Options, p.Enabled, normalizeKind(p.Kind),
		).Scan(&shareID); err != nil {
			return fmt.Errorf("insert share: %w", err)
		}

		mountPoint := mountPointFor(shareID)
		if _, err := tx.ExecContext(ctx, `UPDATE shares SET mount_point = ? WHERE id = ?`, mountPoint, shareID); err != nil {
			return fmt.Errorf("set mount_point: %w", err)
		}

		if err := tx.QueryRowContext(ctx,
			`INSERT INTO sources (kind, share_id, root_path, label) VALUES ('share', ?, ?, ?) RETURNING id`,
			shareID, mountPoint, sourceLabel,
		).Scan(&sourceID); err != nil {
			return fmt.Errorf("insert source: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("db: share create with source: %w", err)
	}
	return shareID, sourceID, nil
}

// ShareUpdateParams is the input to ShareUpdate. PasswordEnc is only
// updated when SetPassword is true, so a PATCH that omits a new password
// leaves the stored ciphertext untouched.
type ShareUpdateParams struct {
	Name        string
	Server      string
	ShareName   string
	Subpath     string
	Username    string
	SetPassword bool
	PasswordEnc []byte
	Domain      string
	Options     string
	Enabled     bool
}

// ShareUpdate updates the editable fields of a share. Returns ErrNotFound
// if no share with id exists.
func (d *DB) ShareUpdate(ctx context.Context, id int64, p ShareUpdateParams) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if p.SetPassword {
			res, err = tx.ExecContext(ctx,
				`UPDATE shares SET name=?, server=?, share=?, subpath=?, username=?, password_enc=?, domain=?, options=?, enabled=? WHERE id=?`,
				p.Name, p.Server, p.ShareName, p.Subpath, p.Username, p.PasswordEnc, p.Domain, p.Options, p.Enabled, id)
		} else {
			res, err = tx.ExecContext(ctx,
				`UPDATE shares SET name=?, server=?, share=?, subpath=?, username=?, domain=?, options=?, enabled=? WHERE id=?`,
				p.Name, p.Server, p.ShareName, p.Subpath, p.Username, p.Domain, p.Options, p.Enabled, id)
		}
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ShareUpdateLocal updates the editable fields of a kind=local share:
// name, mount_point (the validated local directory path — see
// cmd/api's validateLocalStoragePath), and enabled. Local shares have no
// CIFS credentials/server/share/subpath/options to update; see ShareUpdate
// for those. It also updates the linked "share"-kind source's root_path
// (and label, kept in sync with name) in the same Tx — without this, a
// PATCHed local storage's mount_point would drift out of sync with the
// source's root_path it was created from (ShareCreateWithSource), leaving
// the source pointed at the old, no-longer-configured directory.
// storage_files.rel_path is relative to the root and stays valid across
// the move, so nothing else needs rewriting. Returns ErrNotFound if no
// share with id exists.
func (d *DB) ShareUpdateLocal(ctx context.Context, id int64, name, mountPoint string, enabled bool) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE shares SET name=?, mount_point=?, enabled=? WHERE id=?`,
			name, mountPoint, enabled, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sources SET root_path=?, label=? WHERE kind='share' AND share_id=?`,
			mountPoint, name, id); err != nil {
			return err
		}
		return nil
	})
}

// ShareDelete removes a share by id. The `sources` row(s) pointing at it
// (kind='share', share_id=id) cascade per the schema's ON DELETE CASCADE.
// Returns ErrNotFound if no share with id exists.
func (d *DB) ShareDelete(ctx context.Context, id int64) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ShareSetStatus updates a share's status column (e.g. "mounted",
// "error:<msg>", "disabled", "unknown") and, when mounted is true, its
// last_mounted_at timestamp (RFC3339 UTC string).
func (d *DB) ShareSetStatus(ctx context.Context, id int64, status string, mounted bool, lastMountedAt string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if mounted {
			_, err = tx.ExecContext(ctx, `UPDATE shares SET status = ?, last_mounted_at = ? WHERE id = ?`, status, lastMountedAt, id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE shares SET status = ? WHERE id = ?`, status, id)
		}
		return err
	})
}

type rowOrRows interface {
	Scan(dest ...any) error
}

func scanShare(row rowOrRows) (Share, error) {
	var s Share
	var enabled int
	var lastMountedAt sql.NullString
	err := row.Scan(&s.ID, &s.Name, &s.Server, &s.ShareName, &s.Subpath, &s.Username, &s.PasswordEnc,
		&s.Domain, &s.MountPoint, &s.Options, &enabled, &s.Status, &lastMountedAt, &s.Kind)
	if err != nil {
		return Share{}, err
	}
	s.Enabled = enabled != 0
	if lastMountedAt.Valid {
		s.LastMountedAt = &lastMountedAt.String
	}
	return s, nil
}
