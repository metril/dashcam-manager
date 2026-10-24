package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/ssd"
)

type watchCall struct{ uuid, path string }

// fakeWatcher records each Watch start and, like ssd.Watch, blocks until its
// context is cancelled, then records the stop.
type fakeWatcher struct {
	mu      sync.Mutex
	started []watchCall
	stopped []watchCall
	startCh chan watchCall
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{startCh: make(chan watchCall, 10)} }

func (f *fakeWatcher) watch(ctx context.Context, uuid, target string, _ ssd.Hooks) error {
	c := watchCall{uuid, target}
	f.mu.Lock()
	f.started = append(f.started, c)
	f.mu.Unlock()
	f.startCh <- c
	<-ctx.Done()
	f.mu.Lock()
	f.stopped = append(f.stopped, c)
	f.mu.Unlock()
	return nil
}

func (f *fakeWatcher) wantStart(t *testing.T, want watchCall) {
	t.Helper()
	select {
	case got := <-f.startCh:
		if got != want {
			t.Fatalf("watcher started with %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher %+v was not started", want)
	}
}

func (f *fakeWatcher) counts() (started, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), len(f.stopped)
}

func TestSSDControllerRestartsOnUUIDOrPathChange(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := store.SourceEnsure(ctx, "ssd", "/mnt/a", "SSD")
	if err != nil {
		t.Fatal(err)
	}

	fw := newFakeWatcher()
	parent, cancel := context.WithCancel(ctx)
	defer cancel()
	c := newSSDController(parent, store, id)
	c.watch = fw.watch

	c.Start("UUID-1", "/mnt/a")
	fw.wantStart(t, watchCall{"UUID-1", "/mnt/a"})

	// Unchanged values: no restart.
	if err := c.Apply(ctx, "UUID-1", "/mnt/a"); err != nil {
		t.Fatal(err)
	}
	if started, _ := fw.counts(); started != 1 {
		t.Errorf("unchanged apply restarted the watcher (%d starts)", started)
	}

	// New UUID: old watcher stopped (waited for), new one started.
	if err := c.Apply(ctx, "UUID-2", "/mnt/a"); err != nil {
		t.Fatal(err)
	}
	fw.wantStart(t, watchCall{"UUID-2", "/mnt/a"})
	if _, stopped := fw.counts(); stopped != 1 {
		t.Errorf("old watcher stopped %d times, want 1", stopped)
	}

	// New path: the SSD source is repointed in place and the watcher restarts.
	if err := c.Apply(ctx, "UUID-2", "/mnt/b"); err != nil {
		t.Fatal(err)
	}
	fw.wantStart(t, watchCall{"UUID-2", "/mnt/b"})
	if src, err := store.SourceGet(ctx, id); err != nil || src.RootPath != "/mnt/b" {
		t.Errorf("source after path change = %+v, %v", src, err)
	}

	// Empty UUID stops the watcher and starts none.
	if err := c.Apply(ctx, "", "/mnt/b"); err != nil {
		t.Fatal(err)
	}
	if started, stopped := fw.counts(); started != 3 || stopped != 3 {
		t.Errorf("after clearing uuid: %d starts, %d stops, want 3 and 3", started, stopped)
	}
	select {
	case got := <-fw.startCh:
		t.Errorf("unexpected watcher start %+v with empty uuid", got)
	case <-time.After(50 * time.Millisecond):
	}
}
