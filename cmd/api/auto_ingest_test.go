package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

func mkLocalShare(t *testing.T, store *db.DB, name string) int64 {
	t.Helper()
	id, err := store.ShareCreate(context.Background(), db.ShareCreateParams{
		Name: name, Server: "s", ShareName: "x", MountPoint: t.TempDir(), Enabled: true, Kind: db.KindLocal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func setAutoIngest(t *testing.T, store *db.DB, shareID int64) {
	t.Helper()
	if err := store.SettingsSetMany(context.Background(), map[string]json.RawMessage{
		"ssd_ingest_target_share_id": mustJSON(shareID),
	}); err != nil {
		t.Fatal(err)
	}
}

func countJobs(t *testing.T, store *db.DB, typ string) int {
	t.Helper()
	var n int
	if err := store.Write().QueryRow(`SELECT COUNT(*) FROM jobs WHERE type = ?`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMaybeEnqueueAutoIngest(t *testing.T) {
	_, store, q := newTestMuxWithStoreAndQueueNoWorkers(t)
	ctx := context.Background()
	srcID, err := store.SourceCreate(ctx, "ssd", t.TempDir(), "ssd")
	if err != nil {
		t.Fatal(err)
	}

	maybeEnqueueAutoIngest(ctx, store, q, srcID)
	if n := countJobs(t, store, "ingest"); n != 0 {
		t.Fatalf("setting off: %d ingest jobs, want 0", n)
	}

	shareID := mkLocalShare(t, store, "arch")
	setAutoIngest(t, store, shareID)
	maybeEnqueueAutoIngest(ctx, store, q, srcID)
	if n := countJobs(t, store, "ingest"); n != 1 {
		t.Fatalf("enabled: %d ingest jobs, want 1", n)
	}
	list, _ := q.List(ctx, "queued", 10)
	var p ingestJobParams
	for _, j := range list {
		if j.Type == "ingest" {
			_ = json.Unmarshal(j.Params, &p)
		}
	}
	if p.SourceID != srcID || p.TargetShareID != shareID || p.Verify != "sha256" {
		t.Fatalf("params = %+v", p)
	}

	// An ingest for that source is already queued: no second one.
	maybeEnqueueAutoIngest(ctx, store, q, srcID)
	if n := countJobs(t, store, "ingest"); n != 1 {
		t.Fatalf("dedupe: %d ingest jobs, want 1", n)
	}
}

func TestMaybeEnqueueAutoIngestSkipsUnavailableShare(t *testing.T) {
	_, store, q := newTestMuxWithStoreAndQueueNoWorkers(t)
	ctx := context.Background()
	srcID, _ := store.SourceCreate(ctx, "ssd", t.TempDir(), "ssd")
	setAutoIngest(t, store, 9999) // no such share
	maybeEnqueueAutoIngest(ctx, store, q, srcID)
	if n := countJobs(t, store, "ingest"); n != 0 {
		t.Fatalf("%d ingest jobs, want 0", n)
	}
}

func TestSettingsPutAutoIngestShareValidation(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	if rec := putSettings(t, mux, `{"ssd_ingest_target_share_id": 4242}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing share: status = %d, body=%s", rec.Code, rec.Body)
	}
	if rec := putSettings(t, mux, `{"ssd_ingest_target_share_id": -1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative: status = %d", rec.Code)
	}
	id := mkLocalShare(t, store, "arch")
	if rec := putSettings(t, mux, fmt.Sprintf(`{"ssd_ingest_target_share_id": %d}`, id)); rec.Code != http.StatusOK {
		t.Fatalf("valid share: status = %d, body=%s", rec.Code, rec.Body)
	}
	if rec := putSettings(t, mux, `{"ssd_ingest_target_share_id": 0}`); rec.Code != http.StatusOK {
		t.Fatalf("zero (off): status = %d, body=%s", rec.Code, rec.Body)
	}
}

// A successful scan of the SSD source auto-enqueues an ingest when the
// setting is on, and not when it is off.
func TestScanJobAutoEnqueuesIngest(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", on), func(t *testing.T) {
			_, store := newTestMuxWithStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			root := t.TempDir()
			dir := filepath.Join(root, "front/2026/09/03/recording_20260903_192528")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(dir, "segment_00000.mp4"), []byte("abc"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "segment_00000.timestamp.json"),
				[]byte(`{"segment":"segment_00000.mp4","camera":"front","start_time":"2026-09-03T19:25:28.000","end_time":"2026-09-03T19:25:58.000"}`), 0o644)
			srcID, err := store.SourceCreate(ctx, "ssd", root, "ssd")
			if err != nil {
				t.Fatal(err)
			}
			orig := ssdMountedFn
			ssdMountedFn = func(string) (bool, error) { return true, nil }
			t.Cleanup(func() { ssdMountedFn = orig })
			if on {
				setAutoIngest(t, store, mkLocalShare(t, store, "arch"))
			}

			queue := jobs.New(store)
			registerJobHandlers(queue, store, config.Load())
			if err := queue.Start(ctx, 2); err != nil {
				t.Fatal(err)
			}
			job, err := queue.Enqueue(ctx, "scan", scanJobParams{SourceID: srcID}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if final := waitForJobTerminal(t, queue, job.ID, 15*time.Second); final.State != jobs.StateDone {
				t.Fatalf("scan state = %s: %s", final.State, final.Error)
			}
			want := 0
			if on {
				want = 1
			}
			if n := countJobs(t, store, "ingest"); n != want {
				t.Fatalf("ingest jobs = %d, want %d", n, want)
			}
		})
	}
}
