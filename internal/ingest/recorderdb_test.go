package ingest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

const recorderTestDDL = `
CREATE TABLE segments (id INTEGER PRIMARY KEY, path TEXT UNIQUE NOT NULL);
CREATE TABLE detections (id INTEGER PRIMARY KEY, segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE);
CREATE TABLE objects (id INTEGER PRIMARY KEY, detection_id INTEGER NOT NULL REFERENCES detections(id) ON DELETE CASCADE);
CREATE TABLE track_events (id INTEGER PRIMARY KEY, segment_path TEXT);
CREATE TABLE trips (id INTEGER PRIMARY KEY);
INSERT INTO trips VALUES (1);
INSERT INTO segments VALUES (1, '/data/front/2026/09/03/recording_a/segment_00000.mp4');
INSERT INTO segments VALUES (2, '/data/front/2026/09/03/recording_a/segment_00001.mp4');
INSERT INTO detections VALUES (1, 1);
INSERT INTO detections VALUES (2, 2);
INSERT INTO objects VALUES (1, 1);
INSERT INTO objects VALUES (2, 2);
INSERT INTO track_events VALUES (1, '/data/front/2026/09/03/recording_a/segment_00000.mp4');
INSERT INTO track_events VALUES (2, '/data/front/2026/09/03/recording_a/segment_00001.mp4');
`

func writeRecorderTestDB(t *testing.T, root string) {
	t.Helper()
	rdb, err := sql.Open("sqlite", "file:"+filepath.Join(root, "dashcam.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	rdb.SetMaxOpenConns(1)
	if _, err := rdb.Exec(recorderTestDDL); err != nil {
		t.Fatal(err)
	}
}

func recorderCount(t *testing.T, root, table string) int {
	t.Helper()
	rdb, err := sql.Open("sqlite", "file:"+filepath.Join(root, "dashcam.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var n int
	if err := rdb.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneRecorderDB(t *testing.T) {
	root := t.TempDir()
	writeRecorderTestDB(t, root)

	st, err := PruneRecorderDB(context.Background(), root, []string{"front/2026/09/03/recording_a/segment_00000.mp4", "front/nope.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if st.SegmentsDeleted != 1 {
		t.Fatalf("SegmentsDeleted = %d, want 1", st.SegmentsDeleted)
	}
	for table, want := range map[string]int{"segments": 1, "detections": 1, "objects": 1, "track_events": 1, "trips": 1} {
		if got := recorderCount(t, root, table); got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(root, "dashcam.db-wal")); err == nil && fi.Size() != 0 {
		t.Errorf("wal size = %d, want 0", fi.Size())
	}
}

func TestPruneRecorderDBMissingIsNoop(t *testing.T) {
	st, err := PruneRecorderDB(context.Background(), t.TempDir(), []string{"front/x.mp4"})
	if err != nil || st.SegmentsDeleted != 0 || st.Skipped == "" {
		t.Fatalf("got %+v, %v", st, err)
	}
}

func TestCleanupPrunesRecorderDB(t *testing.T) {
	for _, dry := range []bool{true, false} {
		store := openTestDB(t)
		sourceID, root := setupSource(t, store)
		shareID, _ := setupTargetShare(t, store)
		ctx := context.Background()
		target, err := ResolveTargetMount(ctx, store, shareID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Run(ctx, store, target, Opts{SourceID: sourceID, TargetShareID: shareID, Verify: VerifySHA256}, nil); err != nil {
			t.Fatal(err)
		}
		segs, _ := store.IngestCandidates(ctx, sourceID, nil)
		// Recorder DB with absolute paths under a different mount prefix.
		rdb, err := sql.Open("sqlite", "file:"+filepath.Join(root, "dashcam.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rdb.Exec(`CREATE TABLE segments (id INTEGER PRIMARY KEY, path TEXT UNIQUE NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		for _, s := range segs {
			if _, err := rdb.Exec(`INSERT INTO segments(path) VALUES (?)`, "/data/"+s.RelPath); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := rdb.Exec(`INSERT INTO segments(path) VALUES ('/data/front/other.mp4')`); err != nil {
			t.Fatal(err)
		}
		rdb.Close()

		stats, err := Cleanup(ctx, store, CleanupOpts{SourceID: sourceID, DryRun: dry}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := len(segs) + 1
		if !dry {
			want = 1
			if stats.RecorderDBRowsDeleted != len(segs) || stats.RecorderDBError != "" {
				t.Fatalf("stats = %+v", stats)
			}
		}
		if got := recorderCount(t, root, "segments"); got != want {
			t.Fatalf("dry=%v segments = %d, want %d", dry, got, want)
		}
	}
}
