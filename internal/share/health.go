package share

import (
	"context"
	"fmt"
	"log"
	"time"

	"dashcam/internal/db"
)

// HealthInterval is how often RunHealthLoop re-checks every enabled share's
// mount status.
const HealthInterval = 60 * time.Second

// mountAttemptTimeout bounds a single MountCIFS attempt made by the health
// loop (or the initial RemountAll pass), so one unreachable/slow share
// can't stall every other share's check, or the loop's next tick.
const mountAttemptTimeout = 30 * time.Second

// RemountAll mounts every enabled share that isn't already mounted
// (best-effort: a failure for one share is logged and its status set to
// "error:<msg>", but doesn't stop the rest from being attempted).
// Each mount attempt is bounded by mountAttemptTimeout, derived from ctx,
// so it still respects ctx's own cancellation (e.g. process shutdown).
func RemountAll(ctx context.Context, store *db.DB, secretKey string) {
	shares, err := store.SharesList(ctx)
	if err != nil {
		log.Printf("[share] remount all: list shares: %v", err)
		return
	}
	for _, s := range shares {
		if ctx.Err() != nil {
			return
		}
		if !s.Enabled {
			continue
		}
		checkAndMount(ctx, store, s, secretKey)
	}
}

// RunHealthLoop runs an initial RemountAll pass, then polls every enabled
// share's mount status every HealthInterval, remounting anything that has
// fallen off (server rebooted, network blip) and keeping shares.status
// current, until ctx is done. Intended to run for the lifetime of the
// process in its own goroutine — callers should not block startup (e.g.
// the HTTP listener) on it, since a slow/unreachable share could otherwise
// delay the server coming up.
func RunHealthLoop(ctx context.Context, store *db.DB, secretKey string) {
	RemountAll(ctx, store, secretKey)

	ticker := time.NewTicker(HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			shares, err := store.SharesList(ctx)
			if err != nil {
				log.Printf("[share] health loop: list shares: %v", err)
				continue
			}
			for _, s := range shares {
				if ctx.Err() != nil {
					return
				}
				if !s.Enabled {
					if err := store.ShareSetStatus(ctx, s.ID, "disabled", false, ""); err != nil {
						log.Printf("[share] health loop: set status %d: %v", s.ID, err)
					}
					continue
				}
				refreshStatus(ctx, store, s, secretKey)
			}
		}
	}
}

// mountWithTimeout runs MountCIFS under a mountAttemptTimeout derived from
// ctx, so a single hung/unreachable share can't block the health loop (or
// RemountAll) indefinitely.
func mountWithTimeout(ctx context.Context, s db.Share, secretKey string) error {
	mountCtx, cancel := context.WithTimeout(ctx, mountAttemptTimeout)
	defer cancel()
	return MountCIFS(mountCtx, s, secretKey)
}

// checkAndMount mounts s if it isn't already mounted, updating its stored
// status either way. Used by RemountAll. A kind=local share (db.KindLocal)
// is never mounted via mount.cifs — its status comes straight from
// ProbeLocal instead, since a local/bind-mounted directory has no CIFS
// mount to attempt or fall off.
func checkAndMount(ctx context.Context, store *db.DB, s db.Share, secretKey string) {
	if s.Kind == db.KindLocal {
		probeAndSetStatus(store, s)
		return
	}

	mounted, err := IsMounted(s.MountPoint)
	if err == nil && mounted {
		setStatus(store, s.ID, "mounted", true)
		return
	}

	if err := mountWithTimeout(ctx, s, secretKey); err != nil {
		log.Printf("[share] remount %q: %v", s.Name, err)
		setStatus(store, s.ID, fmt.Sprintf("error:%v", err), false)
		return
	}
	setStatus(store, s.ID, "mounted", true)
}

// refreshStatus checks whether an enabled share is still mounted, remounting
// it (best-effort) if it has fallen off, and persists the observed status.
// A kind=local share (db.KindLocal) skips all of that — see checkAndMount.
func refreshStatus(ctx context.Context, store *db.DB, s db.Share, secretKey string) {
	if s.Kind == db.KindLocal {
		probeAndSetStatus(store, s)
		return
	}

	mounted, err := IsMounted(s.MountPoint)
	if err != nil {
		setStatus(store, s.ID, fmt.Sprintf("error:%v", err), false)
		return
	}
	if mounted {
		setStatus(store, s.ID, "mounted", true)
		return
	}

	// Fell off (server reboot, network blip) — best-effort remount.
	if err := mountWithTimeout(ctx, s, secretKey); err != nil {
		log.Printf("[share] health: remount %q: %v", s.Name, err)
		setStatus(store, s.ID, fmt.Sprintf("error:%v", err), false)
		return
	}
	setStatus(store, s.ID, "mounted", true)
}

// probeAndSetStatus runs share.ProbeLocal against s.MountPoint and stores
// the resulting status: "mounted" (re-using the same status string a CIFS
// share reports when live, since the UI's meaning — "usable right now" —
// is the same either way) on success, "error:<msg>" on failure.
func probeAndSetStatus(store *db.DB, s db.Share) {
	if err := ProbeLocal(s.MountPoint); err != nil {
		setStatus(store, s.ID, fmt.Sprintf("error:%v", err), false)
		return
	}
	setStatus(store, s.ID, "mounted", true)
}

func setStatus(store *db.DB, id int64, status string, mounted bool) {
	var lastMountedAt string
	if mounted {
		lastMountedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := store.ShareSetStatus(context.Background(), id, status, mounted, lastMountedAt); err != nil {
		log.Printf("[share] set status %d: %v", id, err)
	}
}
