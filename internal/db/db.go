// Package db owns the sqlite schema and a small typed data-access layer for
// dashcam-manager. It wraps two connection pools over the same database
// file: a single-connection write pool (serializes all writes, per sqlite's
// single-writer model) and a read pool (safe for concurrent reads under
// WAL). Callers should Exec/Query through Write()/Read() or the Tx helper.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by lookups (e.g. SettingsGet) that find no row.
var ErrNotFound = errors.New("db: not found")

// DB wraps a write pool (max 1 open connection) and a read pool over the
// same sqlite database file.
type DB struct {
	write *sql.DB
	read  *sql.DB
}

// dsn builds a sqlite DSN for path with the pragmas required for a
// multi-goroutine web server: WAL journaling (so readers don't block the
// writer), a busy timeout so concurrent access retries instead of failing
// immediately, foreign key enforcement, and NORMAL sync (safe under WAL,
// much faster than FULL for the write-heavy scan/ingest milestones).
func dsn(path string) string {
	return fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)",
		path,
	)
}

// Open opens the sqlite database at path (creating its parent directory and
// the file itself if needed) and returns a DB with write and read pools
// configured. It does not run schema migrations; call InitSchema for that.
func Open(path string) (*DB, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("db: create dir %s: %w", dir, err)
			}
		}
	}

	d := dsn(path)

	write, err := sql.Open("sqlite", d)
	if err != nil {
		return nil, fmt.Errorf("db: open write pool: %w", err)
	}
	// sqlite allows exactly one writer at a time; funnelling all writes
	// through a single connection avoids SQLITE_BUSY churn under load and
	// makes the Tx helper below safe to use from many goroutines.
	write.SetMaxOpenConns(1)

	read, err := sql.Open("sqlite", d)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("db: open read pool: %w", err)
	}

	if err := write.Ping(); err != nil {
		write.Close()
		read.Close()
		return nil, fmt.Errorf("db: ping write pool: %w", err)
	}
	if err := read.Ping(); err != nil {
		write.Close()
		read.Close()
		return nil, fmt.Errorf("db: ping read pool: %w", err)
	}

	return &DB{write: write, read: read}, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	writeErr := d.write.Close()
	readErr := d.read.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// Write returns the single-connection write pool, for callers that need
// direct access (e.g. bulk-insert helpers in later milestones).
func (d *DB) Write() *sql.DB { return d.write }

// Read returns the read pool.
func (d *DB) Read() *sql.DB { return d.read }

// Tx runs fn inside a write transaction, committing on success and rolling
// back on error (including a panic, which is re-thrown after rollback).
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit tx: %w", err)
	}
	return nil
}
