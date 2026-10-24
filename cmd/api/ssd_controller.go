package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/ssd"
)

// errRestartRequired marks an apply failure that only a process restart can
// resolve; the applier surfaces such keys as restart_required.
var errRestartRequired = errors.New("restart required")

func errorsIsRestart(err error) bool { return errors.Is(err, errRestartRequired) }

// ssdStopTimeout bounds how long Apply waits for the old watcher to unmount
// and exit before reporting restart_required.
const ssdStopTimeout = 20 * time.Second

// ssdWatchFn runs the hotplug watcher until ctx is cancelled (ssd.Watch in
// production; tests substitute a fake).
type ssdWatchFn func(ctx context.Context, uuid, target string, hooks ssd.Hooks) error

// ssdController owns the single ssd.Watch goroutine so the ssd_uuid and
// ssd_path settings can change while the server runs: it cancels the old
// watcher (which unmounts the old path if mounted), repoints the SSD source
// at the new path, and starts a new watcher. An empty UUID leaves no
// watcher (the legacy "already mounted by something else" mode).
type ssdController struct {
	store    *db.DB
	parent   context.Context
	watch    ssdWatchFn
	hooksFor func(sourceID int64) ssd.Hooks

	mu       sync.Mutex
	sourceID int64
	uuid     string
	path     string
	cancel   context.CancelFunc
	done     chan struct{}
	hooks    ssd.Hooks // of the running watcher
}

func newSSDController(parent context.Context, store *db.DB, q *jobs.Queue, sourceID int64) *ssdController {
	return &ssdController{store: store, parent: parent, watch: ssd.Watch, hooksFor: defaultSSDHooks(store, q), sourceID: sourceID}
}

// defaultSSDHooks mounts via the real mount(8) wrappers and mirrors
// mount/unmount into the SSD source's status and ssdHotplugState.
func defaultSSDHooks(store *db.DB, q *jobs.Queue) func(int64) ssd.Hooks {
	return func(sourceID int64) ssd.Hooks {
		return ssd.Hooks{
			Mount:   ssd.DefaultMount,
			Unmount: ssd.DefaultUnmount,
			OnMounted: func(dev string) {
				ssdHotplugState.setMounted(dev)
				if err := store.SourceSetStatus(context.Background(), sourceID, "idle"); err != nil {
					log.Printf("[api] ssd: set source status after mount: %v", err)
				}
				// Index the freshly mounted SSD so Ingest has candidates;
				// deduped against any scan already queued/running.
				if q != nil {
					if _, _, err := enqueueScan(context.Background(), q, sourceID, false, false); err != nil {
						log.Printf("[api] ssd: enqueue scan after mount: %v", err)
					}
				}
			},
			OnUnmounted: func() {
				ssdHotplugState.setUnmounted()
				if err := store.SourceSetStatus(context.Background(), sourceID, "offline"); err != nil {
					log.Printf("[api] ssd: set source status after unmount: %v", err)
				}
			},
			OnError: func(err error) { log.Printf("[ssd] %v", err) },
		}
	}
}

// Start launches the watcher for the boot-time values.
func (c *ssdController) Start(uuid, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uuid, c.path = uuid, path
	c.startLocked()
}

func (c *ssdController) startLocked() {
	if c.uuid == "" || c.sourceID == 0 {
		return
	}
	ctx, cancel := context.WithCancel(c.parent)
	done := make(chan struct{})
	c.cancel, c.done = cancel, done
	uuid, path, hooks := c.uuid, c.path, c.hooksFor(c.sourceID)
	c.hooks = hooks
	go func() {
		defer close(done)
		if err := c.watch(ctx, uuid, path, hooks); err != nil {
			log.Printf("[api] ssd watch: %v", err)
		}
	}()
}

// stopLocked cancels the running watcher and waits for it (and its
// best-effort unmount of the old path) to finish.
func (c *ssdController) stopLocked() error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(ssdStopTimeout):
		return errRestartRequired
	}
	c.cancel, c.done = nil, nil
	// ssd.Watch's shutdown unmount doesn't report through OnUnmounted, so
	// record the unmount here or the UI keeps showing a stale "mounted".
	if c.hooks.OnUnmounted != nil {
		c.hooks.OnUnmounted()
	}
	return nil
}

// Apply reconfigures to uuid/path; a no-op if neither changed.
func (c *ssdController) Apply(ctx context.Context, uuid, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if uuid == c.uuid && path == c.path {
		return nil
	}
	if err := c.stopLocked(); err != nil {
		return err
	}
	if path != c.path {
		if c.sourceID != 0 {
			if err := c.store.SourceSetSSDRoot(ctx, c.sourceID, path); err != nil {
				c.startLocked() // keep watching the old values
				return err
			}
		} else if id, err := c.store.SourceEnsure(ctx, "ssd", path, "SSD"); err == nil {
			c.sourceID = id
		} else {
			c.startLocked()
			return err
		}
	}
	c.uuid, c.path = uuid, path
	c.startLocked()
	return nil
}
