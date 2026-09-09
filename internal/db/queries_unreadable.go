package db

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SegmentMarkUnreadable records that segment id's media file cannot be
// decoded: reason is a short human-readable classification (e.g. "Missing
// moov atom"), unreadable_at is set to now, and thumb_status is set to
// 'unreadable' so SegmentsForThumbs (internal/db/queries_thumbs.go) skips
// it on later thumbs jobs instead of retrying a file that can never
// decode. Cleared by SegmentClearUnreadable.
func (d *DB) SegmentMarkUnreadable(ctx context.Context, id int64, reason string) error {
	_, err := d.write.ExecContext(ctx, `
		UPDATE segments SET unreadable_reason = ?, unreadable_at = ?, thumb_status = 'unreadable'
		WHERE id = ?
	`, reason, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("db: segment mark unreadable %d: %w", id, err)
	}
	return nil
}

// SegmentClearUnreadable clears segment id's unreadable flags and resets
// thumb_status to 'pending' so the next "thumbs" job tries it again (used
// by POST /api/segments/unreadable/recheck).
func (d *DB) SegmentClearUnreadable(ctx context.Context, id int64) error {
	_, err := d.write.ExecContext(ctx, `
		UPDATE segments SET unreadable_reason = NULL, unreadable_at = NULL, thumb_status = 'pending'
		WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("db: segment clear unreadable %d: %w", id, err)
	}
	return nil
}

// SegmentClearUnreadableAll clears every segment's unreadable flags and
// returns how many rows were affected (used by POST
// /api/segments/unreadable/recheck when called with no ids, i.e. "recheck
// all").
func (d *DB) SegmentClearUnreadableAll(ctx context.Context) (int64, error) {
	res, err := d.write.ExecContext(ctx, `
		UPDATE segments SET unreadable_reason = NULL, unreadable_at = NULL, thumb_status = 'pending'
		WHERE unreadable_reason IS NOT NULL
	`)
	if err != nil {
		return 0, fmt.Errorf("db: segment clear unreadable all: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: segment clear unreadable all: %w", err)
	}
	return n, nil
}

// SegmentsClearUnreadableByIDs clears unreadable flags for exactly the
// given ids in one statement (used by POST /api/segments/unreadable/
// recheck for a scoped recheck; SegmentClearUnreadableAll handles the
// "recheck all" case).
func (d *DB) SegmentsClearUnreadableByIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`
		UPDATE segments SET unreadable_reason = NULL, unreadable_at = NULL, thumb_status = 'pending'
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))
	if _, err := d.write.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("db: segments clear unreadable by ids: %w", err)
	}
	return nil
}

// UnreadableSegment is one row of GET /api/segments/unreadable.
type UnreadableSegment struct {
	ID            int64    `json:"id"`
	TripID        *int64   `json:"trip_id"`
	Camera        string   `json:"camera"`
	RelPath       string   `json:"rel_path"`
	Seq           int      `json:"seq"`
	DurationS     *float64 `json:"duration_s"`
	Reason        string   `json:"reason"`
	UnreadableAt  string   `json:"unreadable_at"`
	TripStartedAt *string  `json:"trip_started_at"`
}

// unreadableCursor is the decoded form of SegmentsUnreadable's opaque
// cursor: the (unreadable_at, id) of the last row on the previous page,
// matching its ORDER BY so keyset pagination can resume exactly after it
// instead of an OFFSET (which re-scans and re-discards every prior row on
// each page, and can skip or repeat rows if the unreadable set changes
// between pages — e.g. a recheck clearing a row ahead of the cursor).
type unreadableCursor struct {
	unreadableAt string
	id           int64
}

// encodeUnreadableCursor base64-encodes cur into the opaque token handed
// back to callers as next_cursor — opaque the same way every other list
// endpoint's cursor is (callers pass it back verbatim, never parse it).
func encodeUnreadableCursor(cur unreadableCursor) string {
	raw := cur.unreadableAt + "|" + strconv.FormatInt(cur.id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeUnreadableCursor reverses encodeUnreadableCursor.
func decodeUnreadableCursor(cursor string) (unreadableCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return unreadableCursor{}, fmt.Errorf("invalid cursor %q", cursor)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return unreadableCursor{}, fmt.Errorf("invalid cursor %q", cursor)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return unreadableCursor{}, fmt.Errorf("invalid cursor %q", cursor)
	}
	return unreadableCursor{unreadableAt: parts[0], id: id}, nil
}

// SegmentsUnreadable returns a page of segments with unreadable_reason set
// (most recently marked first), plus a next-page cursor (empty when there
// are no more). limit<=0 defaults to 50. Paginated by keyset on
// (unreadable_at DESC, id DESC) rather than OFFSET: cursor (from a
// previous page's next-page cursor) names the last row already returned,
// so a row inserted/cleared ahead of it can't shift a later page's
// contents the way an offset would.
func (d *DB) SegmentsUnreadable(ctx context.Context, limit int, cursor string) ([]UnreadableSegment, string, error) {
	var after *unreadableCursor
	if cursor != "" {
		c, err := decodeUnreadableCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("db: segments unreadable: %w", err)
		}
		after = &c
	}
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT s.id, s.trip_id, s.camera, s.rel_path, s.seq, s.duration_s,
			s.unreadable_reason, s.unreadable_at, t.started_at
		FROM segments s
		LEFT JOIN trips t ON t.id = s.trip_id
		WHERE s.unreadable_reason IS NOT NULL
	`
	args := []any{}
	if after != nil {
		query += ` AND (s.unreadable_at < ? OR (s.unreadable_at = ? AND s.id < ?))`
		args = append(args, after.unreadableAt, after.unreadableAt, after.id)
	}
	query += `
		ORDER BY s.unreadable_at DESC, s.id DESC
		LIMIT ?
	`
	args = append(args, limit+1)

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("db: segments unreadable: %w", err)
	}
	defer rows.Close()

	var out []UnreadableSegment
	for rows.Next() {
		var s UnreadableSegment
		if err := rows.Scan(&s.ID, &s.TripID, &s.Camera, &s.RelPath, &s.Seq, &s.DurationS,
			&s.Reason, &s.UnreadableAt, &s.TripStartedAt); err != nil {
			return nil, "", fmt.Errorf("db: segments unreadable: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("db: segments unreadable: %w", err)
	}

	nextCursor := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		nextCursor = encodeUnreadableCursor(unreadableCursor{unreadableAt: last.UnreadableAt, id: last.ID})
	}
	if out == nil {
		out = []UnreadableSegment{}
	}
	return out, nextCursor, nil
}

// SegmentsUnreadableCount returns the total number of segments with
// unreadable_reason set, for the "Unreadable segments" stats tile.
func (d *DB) SegmentsUnreadableCount(ctx context.Context) (int64, error) {
	var n int64
	if err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE unreadable_reason IS NOT NULL`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: segments unreadable count: %w", err)
	}
	return n, nil
}

// SegmentIDsToRecheck resolves the id list POST /api/segments/unreadable/
// recheck should re-enqueue thumbnail generation for: every currently
// unreadable segment's id if ids is empty ("recheck all"), or the
// intersection of ids with currently-unreadable segments otherwise (an id
// that isn't (or is no longer) marked unreadable is silently skipped
// rather than erroring, since a stale client-side list is harmless here).
func (d *DB) SegmentIDsToRecheck(ctx context.Context, ids []int64) ([]int64, error) {
	query := `SELECT id FROM segments WHERE unreadable_reason IS NOT NULL`
	var args []any
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += fmt.Sprintf(" AND id IN (%s)", strings.Join(placeholders, ","))
	}
	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: segment ids to recheck: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: segment ids to recheck: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
