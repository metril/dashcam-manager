package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SettingsGet returns the raw JSON value stored for key, or ErrNotFound if
// no row exists.
func (d *DB) SettingsGet(ctx context.Context, key string) (json.RawMessage, error) {
	var raw string
	err := d.read.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, key,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: settings get %q: %w", key, err)
	}
	return json.RawMessage(raw), nil
}

// SettingsSet upserts a single key to value (which must be valid JSON).
func (d *DB) SettingsSet(ctx context.Context, key string, value json.RawMessage) error {
	return d.SettingsSetMany(ctx, map[string]json.RawMessage{key: value})
}

// SettingsSetMany upserts every key in values in one write transaction.
// Every value is validated as JSON before the transaction opens, so a bad
// value in the batch fails without ever touching the database (Tx would
// roll back an in-flight failure anyway, but validating up front avoids
// opening a write transaction — and taking the single write connection —
// for a request that was always going to fail).
func (d *DB) SettingsSetMany(ctx context.Context, values map[string]json.RawMessage) error {
	for k, v := range values {
		if !json.Valid(v) {
			return fmt.Errorf("db: settings set %q: invalid JSON value", k)
		}
	}

	return d.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO settings (key, value_json) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for k, v := range values {
			if _, err := stmt.ExecContext(ctx, k, string(v)); err != nil {
				return fmt.Errorf("db: settings set %q: %w", k, err)
			}
		}
		return nil
	})
}

// SettingsGetAll returns every settings row as key -> raw JSON value.
func (d *DB) SettingsGetAll(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT key, value_json FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("db: settings get all: %w", err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// SettingsSeedDefaults inserts each key in defaults only if it doesn't
// already exist (INSERT OR IGNORE), so a redeploy never clobbers values a
// user has already changed via PUT /api/settings.
func (d *DB) SettingsSeedDefaults(ctx context.Context, defaults map[string]json.RawMessage) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO settings (key, value_json) VALUES (?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for k, v := range defaults {
			if _, err := stmt.ExecContext(ctx, k, string(v)); err != nil {
				return fmt.Errorf("db: settings seed %q: %w", k, err)
			}
		}
		return nil
	})
}

// SettingsDelete removes the given keys (missing keys are ignored).
func (d *DB) SettingsDelete(ctx context.Context, keys ...string) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, k); err != nil {
				return fmt.Errorf("db: settings delete %q: %w", k, err)
			}
		}
		return nil
	})
}
