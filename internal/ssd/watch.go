// Package ssd watches for the recorder's USB SSD appearing/disappearing
// and mounts/unmounts it accordingly, from inside the app container.
//
// Why inotify on /dev/disk/by-uuid rather than kernel uevents: uevents
// (NETLINK_KOBJECT_UEVENT) are network-namespace scoped, so a Compose
// container never receives them without host networking. Host udev
// creates /dev/disk/by-uuid/<UUID> on devtmpfs; bind-mounting /dev/disk
// into the container (see compose.yaml) makes those inodes show up live,
// and inotify on the bind-mounted directory fires on create/delete
// exactly when the host does — an exact hotplug signal with zero
// polling.
package ssd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"dashcam/internal/config"
	"dashcam/internal/share"
)

// byUUIDDir is where udev creates /dev/disk/by-uuid/<UUID> symlinks. A
// package var (not a const) so tests can point it at a fake directory
// (e.g. t.TempDir()) instead of requiring real hardware and a real
// /dev/disk bind.
var byUUIDDir = "/dev/disk/by-uuid"

// isMountedFn reports whether a path is currently mounted. A package
// var (like ssdMountedFn in cmd/api) rather than calling share.IsMounted
// directly, so tests can stub "already mounted" without needing a real
// mount — which matters here in particular since a real mount can't
// safely be set up (or torn down, via shutdownUnmount) inside a test.
var isMountedFn = share.IsMounted

// watchMask is the set of inotify events Watch cares about on byUUIDDir:
// a symlink (created, or moved in) means the device just appeared; a
// symlink removed (or moved out) means it just disappeared.
const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM

// addWatchRetryInitial/Max bound the backoff used while byUUIDDir doesn't
// exist yet (e.g. at first boot, before any filesystem with a UUID has
// ever been plugged in, or between the container starting and udev
// finishing populating /dev/disk).
const (
	addWatchRetryInitial = 500 * time.Millisecond
	addWatchRetryMax     = 30 * time.Second
)

// ResolveFunc resolves uuid (the name of a symlink under byUUIDDir) to a
// device path suitable for passing to a MountFunc.
type ResolveFunc func(uuid string) (string, error)

// MountFunc mounts the resolved device path dev (as returned by a
// ResolveFunc) at target.
type MountFunc func(dev, target string) error

// UnmountFunc unmounts target.
type UnmountFunc func(target string) error

// Hooks customizes Watch's resolve/mount/unmount behavior and notifies
// the caller of state transitions, so cmd/api can update the SSD
// source's stored status and in-container hotplug UI state without this
// package needing to depend on internal/db.
type Hooks struct {
	// Resolve resolves uuid to a device path. Defaults to
	// DefaultResolveDevice if nil.
	Resolve ResolveFunc
	// Mount mounts dev at target. Required.
	Mount MountFunc
	// Unmount unmounts target, called when the device disappears (and
	// once more, best-effort, on shutdown if still mounted). Required.
	Unmount UnmountFunc
	// OnMounted is called after a successful Mount, with the resolved
	// device path.
	OnMounted func(dev string)
	// OnUnmounted is called after the device disappears — regardless of
	// whether Unmount itself succeeded, since the device is physically
	// gone either way. Not called for the best-effort shutdown unmount.
	OnUnmounted func()
	// OnError is called with a non-fatal error from a resolve/mount/
	// unmount attempt; Watch logs to the standard logger and continues
	// if this is nil.
	OnError func(error)
}

// Watch watches byUUIDDir for uuid's symlink appearing/disappearing and
// mounts/unmounts target via hooks accordingly. If uuid is already
// present when Watch starts, it mounts immediately (covers the container
// starting up with the drive already plugged in). If byUUIDDir doesn't
// exist yet (e.g. nothing with a filesystem UUID has ever been plugged
// in), it retries adding the watch with backoff rather than failing
// outright. It blocks, processing inotify events, until ctx is
// cancelled, at which point it makes a best-effort clean (then lazy, on
// EBUSY) unmount of target if still mounted, and returns nil.
func Watch(ctx context.Context, uuid, target string, hooks Hooks) error {
	if !config.ValidSSDUUID(uuid) {
		// Defense in depth: cmd/api already validates DASHCAM_SSD_UUID /
		// the "ssd_uuid" setting before ever calling Watch, but uuid is
		// joined directly onto byUUIDDir and then mounted — never trust a
		// single layer of validation alone for something that privileged.
		return fmt.Errorf("ssd: watch: invalid uuid %q", uuid)
	}
	if hooks.Mount == nil || hooks.Unmount == nil {
		return fmt.Errorf("ssd: watch: Hooks.Mount and Hooks.Unmount are required")
	}
	if hooks.Resolve == nil {
		hooks.Resolve = DefaultResolveDevice
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("ssd: inotify init: %w", err)
	}
	defer unix.Close(fd)

	wd, err := addWatchWithRetry(ctx, fd)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer unix.InotifyRmWatch(fd, uint32(wd))

	// Best-effort clean shutdown: if ctx was what ended the loop (as
	// opposed to an unexpected error) and target is still mounted, try to
	// unmount it before the process exits out from under it.
	defer func() {
		if ctx.Err() != nil {
			shutdownUnmount(target, hooks)
		}
	}()

	// Self-pipe trick: unix.Poll can't select on a context directly, so a
	// goroutine closes the write end when ctx is cancelled, which wakes
	// the poll loop below via a POLLHUP/POLLERR on the read end.
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("ssd: watch: create wakeup pipe: %w", err)
	}
	defer pipeR.Close()
	go func() {
		<-ctx.Done()
		_ = pipeW.Close()
	}()

	// Mount immediately if the device is already plugged in at startup.
	if _, statErr := os.Lstat(filepath.Join(byUUIDDir, uuid)); statErr == nil {
		tryMount(uuid, target, hooks)
	}

	buf := make([]byte, 4096)
	fds := []unix.PollFd{
		{Fd: int32(fd), Events: unix.POLLIN},
		{Fd: int32(pipeR.Fd()), Events: unix.POLLIN},
	}
	for {
		_, err := unix.Poll(fds, -1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("ssd: watch: poll: %w", err)
		}
		if fds[1].Revents != 0 {
			// ctx was cancelled (pipe closed).
			return nil
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}

		n, err := unix.Read(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ssd: watch: read inotify events: %w", err)
		}
		for _, ev := range parseInotifyEvents(buf[:n]) {
			if ev.Name != uuid {
				continue
			}
			switch {
			case ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0:
				tryMount(uuid, target, hooks)
			case ev.Mask&(unix.IN_DELETE|unix.IN_MOVED_FROM) != 0:
				tryUnmount(target, hooks)
			}
		}
	}
}

// addWatchWithRetry adds an inotify watch on byUUIDDir, retrying with
// exponential backoff (capped at addWatchRetryMax) if the directory
// doesn't exist yet — expected at first boot, before any filesystem with
// a UUID has ever been plugged in, or momentarily while /dev/disk is
// still being populated. Any other error is returned immediately.
func addWatchWithRetry(ctx context.Context, fd int) (int, error) {
	backoff := addWatchRetryInitial
	warned := false
	for {
		wd, err := unix.InotifyAddWatch(fd, byUUIDDir, watchMask)
		if err == nil {
			return wd, nil
		}
		if !errors.Is(err, unix.ENOENT) {
			return 0, fmt.Errorf("ssd: inotify add watch %s: %w", byUUIDDir, err)
		}
		if !warned {
			log.Printf("[ssd] %s does not exist yet; will retry (no filesystem with a UUID has appeared, or /dev/disk isn't populated yet)", byUUIDDir)
			warned = true
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < addWatchRetryMax {
			backoff *= 2
			if backoff > addWatchRetryMax {
				backoff = addWatchRetryMax
			}
		}
	}
}

// shutdownUnmount makes a best-effort attempt to cleanly unmount target
// on Watch's shutdown path (ctx cancelled), trying a clean unmount first
// and falling back to a lazy one on EBUSY — mirrors POST
// /api/ingest/eject's own Eject-then-ForceEject behavior, since a
// container restart shouldn't leave the SSD in a half-unmounted state
// any more than a user-initiated eject would.
func shutdownUnmount(target string, hooks Hooks) {
	mounted, err := isMountedFn(target)
	if err != nil || !mounted {
		return
	}
	if err := Eject(target); err != nil {
		if errors.Is(err, unix.EBUSY) {
			if ferr := ForceEject(target); ferr != nil {
				reportErr(hooks, fmt.Errorf("ssd: shutdown: lazy unmount %s: %w", target, ferr))
			}
			return
		}
		reportErr(hooks, fmt.Errorf("ssd: shutdown: unmount %s: %w", target, err))
	}
}

func tryMount(uuid, target string, hooks Hooks) {
	if mounted, err := isMountedFn(target); err != nil {
		reportErr(hooks, fmt.Errorf("ssd: check already mounted: %w", err))
		// Fall through: being unable to check isn't a reason to never
		// mount.
	} else if mounted {
		// Already mounted — e.g. a duplicate IN_CREATE/IN_MOVED_TO, or
		// Watch's own startup check racing the first inotify event for
		// the same appearance. Mounting again would stack a second mount
		// on top for no benefit.
		return
	}

	dev, err := hooks.Resolve(uuid)
	if err != nil {
		reportErr(hooks, err)
		return
	}
	if err := hooks.Mount(dev, target); err != nil {
		reportErr(hooks, fmt.Errorf("ssd: mount %s at %s: %w", dev, target, err))
		return
	}
	if hooks.OnMounted != nil {
		hooks.OnMounted(dev)
	}
}

func tryUnmount(target string, hooks Hooks) {
	if err := hooks.Unmount(target); err != nil {
		reportErr(hooks, fmt.Errorf("ssd: unmount %s: %w", target, err))
	}
	// The device is physically gone either way — always notify, even if
	// the unmount itself errored (e.g. it was already unmounted).
	if hooks.OnUnmounted != nil {
		hooks.OnUnmounted()
	}
}

func reportErr(hooks Hooks, err error) {
	if hooks.OnError != nil {
		hooks.OnError(err)
		return
	}
	log.Printf("[ssd] %v", err)
}

// inotifyEvent is the subset of a decoded inotify_event this package
// cares about.
type inotifyEvent struct {
	Mask uint32
	Name string
}

// inotifyEventHeaderSize is sizeof(struct inotify_event) excluding the
// variable-length, NUL-padded name field: wd(int32) + mask(uint32) +
// cookie(uint32) + len(uint32).
const inotifyEventHeaderSize = 16

// parseInotifyEvents decodes zero or more inotify_event records packed
// into buf (as returned by a single read() of the inotify fd). Split out
// as a pure function so it's directly unit-testable against a
// hand-built byte buffer, without needing a real inotify fd.
func parseInotifyEvents(buf []byte) []inotifyEvent {
	var events []inotifyEvent
	offset := 0
	for offset+inotifyEventHeaderSize <= len(buf) {
		mask := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
		nameLen := int(binary.LittleEndian.Uint32(buf[offset+12 : offset+16]))
		nameStart := offset + inotifyEventHeaderSize
		nameEnd := nameStart + nameLen
		if nameEnd > len(buf) {
			break
		}
		name := ""
		if nameLen > 0 {
			raw := buf[nameStart:nameEnd]
			// The name field is NUL-padded to a multiple of the struct's
			// alignment; trim at the first NUL.
			if i := indexByte(raw, 0); i >= 0 {
				raw = raw[:i]
			}
			name = string(raw)
		}
		events = append(events, inotifyEvent{Mask: mask, Name: name})
		offset = nameEnd
	}
	return events
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
