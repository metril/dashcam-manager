package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OIDCPendingPut stores (replacing any previous) the candidate OIDC config
// JSON awaiting its test login.
func (d *DB) OIDCPendingPut(ctx context.Context, cfgJSON string, now, expires time.Time) error {
	_, err := d.Write().ExecContext(ctx,
		`INSERT INTO oidc_pending (id, cfg_json, state_hash, created_at, expires_at) VALUES (1, ?, NULL, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET cfg_json = excluded.cfg_json, state_hash = NULL,
		   created_at = excluded.created_at, expires_at = excluded.expires_at`,
		cfgJSON, now.Unix(), expires.Unix())
	if err != nil {
		return fmt.Errorf("db: oidc pending put: %w", err)
	}
	return nil
}

// OIDCPendingBind ties the unexpired pending config to the login state
// stateHash (set when /auth/test-login starts) and returns its JSON.
func (d *DB) OIDCPendingBind(ctx context.Context, stateHash string, now time.Time) (string, bool, error) {
	var cfg string
	err := d.Write().QueryRowContext(ctx,
		`UPDATE oidc_pending SET state_hash = ? WHERE id = 1 AND expires_at > ? RETURNING cfg_json`,
		stateHash, now.Unix()).Scan(&cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("db: oidc pending bind: %w", err)
	}
	return cfg, true, nil
}

// OIDCPendingTake atomically deletes and returns the unexpired pending
// config bound to stateHash, so a replayed callback finds nothing.
func (d *DB) OIDCPendingTake(ctx context.Context, stateHash string, now time.Time) (string, bool, error) {
	var cfg string
	err := d.Write().QueryRowContext(ctx,
		`DELETE FROM oidc_pending WHERE id = 1 AND state_hash = ? AND expires_at > ? RETURNING cfg_json`,
		stateHash, now.Unix()).Scan(&cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("db: oidc pending take: %w", err)
	}
	return cfg, true, nil
}

// OIDCPendingClear drops any pending config.
func (d *DB) OIDCPendingClear(ctx context.Context) error {
	if _, err := d.Write().ExecContext(ctx, `DELETE FROM oidc_pending`); err != nil {
		return fmt.Errorf("db: oidc pending clear: %w", err)
	}
	return nil
}
