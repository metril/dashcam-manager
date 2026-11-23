package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// RecorderDBStats reports what PruneRecorderDB did.
type RecorderDBStats struct {
	// SegmentsDeleted counts recorder `segments` rows removed (their
	// detections/tracked vehicles/objects/plates cascade via foreign keys).
	SegmentsDeleted int
	// Skipped explains a no-op (no dashcam.db under the root).
	Skipped string
}

// recorderRelPath maps a recorder absolute path (e.g.
// "/data/front/2026/02/27/recording_.../segment_00000.mp4") to our rel_path
// form by keeping everything from the first "front/" or "rear/" component.
func recorderRelPath(p string) string {
	p = filepath.ToSlash(p)
	for _, cam := range []string{"front/", "rear/"} {
		if i := strings.Index(p, cam); i >= 0 {
			return p[i:]
		}
	}
	return ""
}

// PruneRecorderDB deletes the recorder's dashcam.db rows for the segments
// at relPaths (relative to root), mirroring the recorder's own
// cleanup_recording_dir: the segments row (cascading to detections etc.)
// and the track_events rows keyed by segment_path. vehicle_events and
// trips are left alone. A missing dashcam.db is a no-op.
func PruneRecorderDB(ctx context.Context, root string, relPaths []string) (RecorderDBStats, error) {
	var stats RecorderDBStats
	if len(relPaths) == 0 {
		return stats, nil
	}
	dbPath := filepath.Join(root, "dashcam.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			stats.Skipped = "no dashcam.db"
			return stats, nil
		}
		return stats, err
	}

	rdb, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return stats, err
	}
	// One connection so the pragmas apply to every statement.
	rdb.SetMaxOpenConns(1)
	defer rdb.Close()

	want := make(map[string]bool, len(relPaths))
	for _, r := range relPaths {
		want[filepath.ToSlash(r)] = true
	}

	type row struct {
		id   int64
		path string
	}
	var matched []row
	rows, err := rdb.QueryContext(ctx, `SELECT id, path FROM segments`)
	if err != nil {
		return stats, fmt.Errorf("recorder db: list segments: %w", err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			rows.Close()
			return stats, err
		}
		if want[recorderRelPath(r.path)] {
			matched = append(matched, r)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, err
	}
	rows.Close()

	if len(matched) > 0 {
		tx, err := rdb.BeginTx(ctx, nil)
		if err != nil {
			return stats, err
		}
		for _, m := range matched {
			if _, err := tx.ExecContext(ctx, `DELETE FROM track_events WHERE segment_path = ?`, m.path); err != nil && !isNoSuchTable(err) {
				tx.Rollback()
				return stats, fmt.Errorf("recorder db: delete track_events: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM segments WHERE id = ?`, m.id); err != nil {
				tx.Rollback()
				return stats, fmt.Errorf("recorder db: delete segment: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return stats, err
		}
		stats.SegmentsDeleted = len(matched)
	}

	// Leave no stale -wal behind: the SSD vehicle import skips on a
	// non-empty WAL.
	if _, err := rdb.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return stats, fmt.Errorf("recorder db: checkpoint: %w", err)
	}
	return stats, nil
}

func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
