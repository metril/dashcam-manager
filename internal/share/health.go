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

// RemountAll mounts every enabled share that isn't already mounted
// (best-effort: a failure for one share is logged and its status set to
// "error:<msg>", but doesn't stop the rest from being attempted). Intended
// to be called once at boot.
func RemountAll(ctx context.Context, store *db.DB, secretKey string) {
	shares, err := store.SharesList(ctx)
	if err != nil {
		log.Printf("[share] remount all: list shares: %v", err)
		return
	}
	for _, s := range shares {
		if !s.Enabled {
			continue
		}
		checkAndMount(store, s, secretKey)
	}
}

// RunHealthLoop polls every enabled share's mount status every
// HealthInterval, remounting anything that has fallen off (server rebooted,
// network blip) and keeping shares.status current, until ctx is done.
// Intended to run for the lifetime of the process in its own goroutine.
func RunHealthLoop(ctx context.Context, store *db.DB, secretKey string) {
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
				refreshStatus(store, s, secretKey)
			}
		}
	}
}

// checkAndMount mounts s if it isn't already mounted, updating its stored
// status either way. Used by RemountAll at boot.
func checkAndMount(store *db.DB, s db.Share, secretKey string) {
	mounted, err := IsMounted(s.MountPoint)
	if err == nil && mounted {
		setStatus(store, s.ID, "mounted", true)
		return
	}

	if err := MountCIFS(context.Background(), s, secretKey); err != nil {
		log.Printf("[share] remount %q: %v", s.Name, err)
		setStatus(store, s.ID, fmt.Sprintf("error:%v", err), false)
		return
	}
	setStatus(store, s.ID, "mounted", true)
}

// refreshStatus checks whether an enabled share is still mounted, remounting
// it (best-effort) if it has fallen off, and persists the observed status.
func refreshStatus(store *db.DB, s db.Share, secretKey string) {
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
	if err := MountCIFS(context.Background(), s, secretKey); err != nil {
		log.Printf("[share] health: remount %q: %v", s.Name, err)
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
