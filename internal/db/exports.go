// exports.go is the data-access layer for the `exports` table (M11): one
// row per finished/failed clip export, tracking where its job landed the
// output file (or, for a share target, that it's on a share rather than
// in the local cache) and when a download-target file expires.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Export is one row of the `exports` table.
type Export struct {
	ID        int64   `json:"id"`
	JobID     *int64  `json:"job_id"`
	Name      string  `json:"name"`
	Path      *string `json:"path,omitempty"`
	ShareID   *int64  `json:"share_id,omitempty"`
	Bytes     int64   `json:"bytes"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	// Copies (local-storage plan task 9): populated by the handler via
	// ExportCopies, not by any query in this file — see cmd/api's
	// handleExportsGet. Always a non-nil (possibly empty) slice.
	Copies []CopyRef `json:"copies"`
}

const exportSelectCols = `SELECT id, job_id, name, path, share_id, bytes, created_at, expires_at`

// ExportCreate inserts a placeholder export row (job_id NULL) — created
// before the "export" job that will fill it in is even enqueued, since the
// job's own params need the export id it's given here. Call ExportSetJobID
// once the job is enqueued to link the two.
func (d *DB) ExportCreate(ctx context.Context, name string) (Export, error) {
	row := d.write.QueryRowContext(ctx, `
		INSERT INTO exports (name) VALUES (?)
		RETURNING id, job_id, name, path, share_id, bytes, created_at, expires_at`, name)
	e, err := scanExport(row)
	if err != nil {
		return Export{}, fmt.Errorf("db: export create: %w", err)
	}
	return e, nil
}

// ExportSetJobID links export id to jobID (see ExportCreate).
func (d *DB) ExportSetJobID(ctx context.Context, id, jobID int64) error {
	if _, err := d.write.ExecContext(ctx, `UPDATE exports SET job_id = ? WHERE id = ?`, jobID, id); err != nil {
		return fmt.Errorf("db: export set job id %d: %w", id, err)
	}
	return nil
}

// ExportFinish records a completed export's output: path (for a download
// target, the on-disk cache path; for a share target, the path under the
// share mount) or shareID (set instead of/alongside path for a share
// target), byte size, and expiresAt (nil for a share target — those don't
// expire the way cached downloads do).
func (d *DB) ExportFinish(ctx context.Context, id int64, path string, shareID *int64, bytes int64, expiresAt *string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE exports SET path = ?, share_id = ?, bytes = ?, expires_at = ? WHERE id = ?`,
		path, shareID, bytes, expiresAt, id)
	if err != nil {
		return fmt.Errorf("db: export finish %d: %w", id, err)
	}
	return nil
}

// ExportsList returns every export, newest first.
func (d *DB) ExportsList(ctx context.Context) ([]Export, error) {
	rows, err := d.read.QueryContext(ctx, exportSelectCols+` FROM exports ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("db: exports list: %w", err)
	}
	defer rows.Close()

	var out []Export
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, fmt.Errorf("db: exports list: %w", err)
		}
		out = append(out, e)
	}
	if out == nil {
		out = []Export{}
	}
	return out, rows.Err()
}

// ExportGet returns the export with the given id, or ErrNotFound.
func (d *DB) ExportGet(ctx context.Context, id int64) (Export, error) {
	row := d.read.QueryRowContext(ctx, exportSelectCols+` FROM exports WHERE id = ?`, id)
	e, err := scanExport(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, ErrNotFound
	}
	if err != nil {
		return Export{}, fmt.Errorf("db: export get %d: %w", id, err)
	}
	return e, nil
}

// ExportDelete removes the export row with id. Not an error if it doesn't
// exist (mirrors a delete's usual idempotency) — callers that need to
// distinguish should ExportGet first.
func (d *DB) ExportDelete(ctx context.Context, id int64) error {
	if _, err := d.write.ExecContext(ctx, `DELETE FROM exports WHERE id = ?`, id); err != nil {
		return fmt.Errorf("db: export delete %d: %w", id, err)
	}
	return nil
}

// ExportsExpired returns every export whose expires_at has passed
// (download-target exports only — share-target rows never get an
// expires_at), for the cache-cleanup loop to remove.
func (d *DB) ExportsExpired(ctx context.Context) ([]Export, error) {
	rows, err := d.read.QueryContext(ctx,
		exportSelectCols+` FROM exports WHERE expires_at IS NOT NULL AND expires_at <= strftime('%Y-%m-%dT%H:%M:%fZ','now')`)
	if err != nil {
		return nil, fmt.Errorf("db: exports expired: %w", err)
	}
	defer rows.Close()

	var out []Export
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, fmt.Errorf("db: exports expired: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanExport(row rowScannerDB) (Export, error) {
	var e Export
	var jobID sql.NullInt64
	var path sql.NullString
	var shareID sql.NullInt64
	var expiresAt sql.NullString
	if err := row.Scan(&e.ID, &jobID, &e.Name, &path, &shareID, &e.Bytes, &e.CreatedAt, &expiresAt); err != nil {
		return Export{}, err
	}
	if jobID.Valid {
		e.JobID = &jobID.Int64
	}
	if path.Valid {
		e.Path = &path.String
	}
	if shareID.Valid {
		e.ShareID = &shareID.Int64
	}
	if expiresAt.Valid {
		e.ExpiresAt = &expiresAt.String
	}
	return e, nil
}

// rowScannerDB is satisfied by both *sql.Row and *sql.Rows (mirrors
// internal/jobs' rowScanner, duplicated here since it's an unexported
// package-local convenience and internal/db doesn't otherwise depend on
// internal/jobs).
type rowScannerDB interface {
	Scan(dest ...any) error
}

// ---- export resolution (segment -> file path/trip, for the export job) ----

// SegmentExportInfo is everything the export job needs to resolve one
// segment_id export item into a file path plus enough metadata to plan
// trims/scaling/rear-pairing.
type SegmentExportInfo struct {
	ID         int64
	TripID     *int64
	Camera     string
	SourceRoot string
	RelPath    string
	StartTime  string
	DurationS  *float64
	Width      *int
	Height     *int
}

// SegmentExportInfoGet returns SegmentExportInfo for id, or ErrNotFound.
func (d *DB) SegmentExportInfoGet(ctx context.Context, id int64) (SegmentExportInfo, error) {
	var s SegmentExportInfo
	err := d.read.QueryRowContext(ctx, `
		SELECT s.id, s.trip_id, s.camera, src.root_path, s.rel_path, s.start_time, s.duration_s, s.width, s.height
		FROM segments s
		JOIN sources src ON src.id = s.source_id
		WHERE s.id = ?
	`, id).Scan(&s.ID, &s.TripID, &s.Camera, &s.SourceRoot, &s.RelPath, &s.StartTime, &s.DurationS, &s.Width, &s.Height)
	if errors.Is(err, sql.ErrNoRows) {
		return SegmentExportInfo{}, ErrNotFound
	}
	if err != nil {
		return SegmentExportInfo{}, fmt.Errorf("db: segment export info %d: %w", id, err)
	}
	return s, nil
}

// TripPairInfo is a trip's started_at (the shared timeline origin) and
// pair_offset_s (rear.start - front.start median; see
// internal/scan/pair.go), for rear-camera offset resolution.
type TripPairInfo struct {
	StartedAt   string
	PairOffsetS *float64
}

// TripPairInfoGet returns TripPairInfo for tripID, or ErrNotFound.
func (d *DB) TripPairInfoGet(ctx context.Context, tripID int64) (TripPairInfo, error) {
	var t TripPairInfo
	err := d.read.QueryRowContext(ctx,
		`SELECT started_at, pair_offset_s FROM trips WHERE id = ?`, tripID,
	).Scan(&t.StartedAt, &t.PairOffsetS)
	if errors.Is(err, sql.ErrNoRows) {
		return TripPairInfo{}, ErrNotFound
	}
	if err != nil {
		return TripPairInfo{}, fmt.Errorf("db: trip pair info %d: %w", tripID, err)
	}
	return t, nil
}

// TripSegmentPlacement is one segment's [OffsetS, OffsetS+DurationS) window
// on a trip's shared timeline (seconds from trips.started_at), plus its
// file path — enough for the export job to both pick the rear segment
// covering a target instant (see media.ResolveRearSegment) and locate its
// file without a second query.
type TripSegmentPlacement struct {
	SegmentID  int64
	OffsetS    float64
	DurationS  float64
	SourceRoot string
	RelPath    string
}

// TripCameraPlacements returns every camera-camera segment of tripID,
// ordered by OffsetS ascending, with OffsetS computed the same way
// TripTimelineGet does (julianday difference from trips.started_at).
func (d *DB) TripCameraPlacements(ctx context.Context, tripID int64, camera string) ([]TripSegmentPlacement, error) {
	rows, err := d.read.QueryContext(ctx, `
		SELECT s.id, (julianday(s.start_time) - julianday(t.started_at)) * 86400.0,
			COALESCE(s.duration_s, 0), src.root_path, s.rel_path
		FROM segments s
		JOIN trips t ON t.id = s.trip_id
		JOIN sources src ON src.id = s.source_id
		WHERE s.trip_id = ? AND s.camera = ?
		ORDER BY s.start_time
	`, tripID, camera)
	if err != nil {
		return nil, fmt.Errorf("db: trip camera placements %d/%s: %w", tripID, camera, err)
	}
	defer rows.Close()

	var out []TripSegmentPlacement
	for rows.Next() {
		var p TripSegmentPlacement
		if err := rows.Scan(&p.SegmentID, &p.OffsetS, &p.DurationS, &p.SourceRoot, &p.RelPath); err != nil {
			return nil, fmt.Errorf("db: trip camera placements %d/%s: %w", tripID, camera, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
