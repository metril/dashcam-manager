package ssd

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testUUID = "F9B1-23CB"

// recorder records mount/unmount/resolve invocations so tests can assert
// on what Watch actually did. All access goes through the locked
// accessor methods below — the fields themselves are written from
// Watch's own goroutine and read from the test goroutine, so touching
// them directly from a test (as an earlier version of this file did)
// races under `go test -race`.
type recorder struct {
	mu       sync.Mutex
	mounted  []string // dev paths passed to Mount, in order
	unmounts int
}

// resolve is a stub ResolveFunc that just returns dev unchanged — tests
// exercise Watch's inotify/mount/unmount orchestration against fake
// devices, not DefaultResolveDevice's real sysfs+mknod dance (which needs
// an actual block device and CAP_MKNOD).
func (r *recorder) resolve(dev string) ResolveFunc {
	return func(uuid string) (string, error) { return dev, nil }
}

func (r *recorder) mount(dev, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mounted = append(r.mounted, dev)
	return nil
}

func (r *recorder) unmount(target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unmounts++
	return nil
}

func (r *recorder) mountCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.mounted)
}

// firstMounted returns the first dev path passed to Mount (locked, unlike
// indexing r.mounted directly from a test goroutine).
func (r *recorder) firstMounted() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.mounted) == 0 {
		return ""
	}
	return r.mounted[0]
}

func (r *recorder) unmountCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unmounts
}

// waitFor polls cond every few ms until it's true or the timeout elapses,
// failing the test on timeout — Watch's inotify->hook path is
// asynchronous relative to the test goroutine creating/removing the
// symlink.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// withFakeByUUIDDir points byUUIDDir at a fresh t.TempDir() for the
// duration of the test, restoring the real path afterward.
func withFakeByUUIDDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := byUUIDDir
	byUUIDDir = dir
	t.Cleanup(func() { byUUIDDir = orig })
	return dir
}

// makeFakeDevice creates a regular file (standing in for a block device
// node) and a symlink to it named uuid inside dir, as udev would create
// under /dev/disk/by-uuid/<UUID>. The recorder's stub Resolve ignores the
// symlink entirely and just returns devPath, so this doesn't need to
// resemble a real by-uuid target.
func makeFakeDevice(t *testing.T, dir, uuid string) (devPath, linkPath string) {
	t.Helper()
	devPath = filepath.Join(dir, "fake-dev")
	if err := os.WriteFile(devPath, nil, 0o644); err != nil {
		t.Fatalf("write fake device: %v", err)
	}
	linkPath = filepath.Join(dir, uuid)
	if err := os.Symlink(devPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return devPath, linkPath
}

func TestWatchMountsAlreadyPresentDeviceOnStart(t *testing.T) {
	dir := withFakeByUUIDDir(t)
	target := t.TempDir()
	devPath, _ := makeFakeDevice(t, dir, testUUID)

	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, testUUID, target, Hooks{Resolve: rec.resolve(devPath), Mount: rec.mount, Unmount: rec.unmount})
	}()

	waitFor(t, func() bool { return rec.mountCount() == 1 })
	if got := rec.firstMounted(); got != devPath {
		t.Errorf("mounted dev = %q, want %q", got, devPath)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after ctx cancellation")
	}
}

func TestWatchMountsOnCreateAndUnmountsOnDelete(t *testing.T) {
	dir := withFakeByUUIDDir(t)
	target := t.TempDir()

	var onMountedCalls, onUnmountedCalls int
	var mu sync.Mutex
	rec := &recorder{}
	hooks := Hooks{
		Resolve: rec.resolve(filepath.Join(dir, "fake-dev")),
		Mount:   rec.mount,
		Unmount: rec.unmount,
		OnMounted: func(dev string) {
			mu.Lock()
			onMountedCalls++
			mu.Unlock()
		},
		OnUnmounted: func() {
			mu.Lock()
			onUnmountedCalls++
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, testUUID, target, hooks) }()

	// Device not present yet: no mount should have happened.
	time.Sleep(50 * time.Millisecond)
	if rec.mountCount() != 0 {
		t.Fatalf("mountCount = %d before device appears, want 0", rec.mountCount())
	}

	_, linkPath := makeFakeDevice(t, dir, testUUID)
	waitFor(t, func() bool { return rec.mountCount() == 1 })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return onMountedCalls == 1 })

	if err := os.Remove(linkPath); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	waitFor(t, func() bool { return rec.unmountCount() == 1 })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return onUnmountedCalls == 1 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after ctx cancellation")
	}
}

func TestWatchSkipsMountWhenAlreadyMounted(t *testing.T) {
	dir := withFakeByUUIDDir(t)
	target := t.TempDir()

	// Stub "already mounted" rather than setting up (and, on shutdown,
	// tearing down) a real mount, which a test can't safely do — Watch's
	// own shutdownUnmount will call the real Eject/ForceEject syscalls
	// against target regardless, but those are no-ops (EINVAL, swallowed)
	// against a plain directory that was never actually mounted.
	origIsMounted := isMountedFn
	isMountedFn = func(path string) (bool, error) { return path == target, nil }
	t.Cleanup(func() { isMountedFn = origIsMounted })

	rec := &recorder{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, testUUID, target, Hooks{Resolve: rec.resolve(filepath.Join(dir, "fake-dev")), Mount: rec.mount, Unmount: rec.unmount})
	}()

	makeFakeDevice(t, dir, testUUID)
	time.Sleep(150 * time.Millisecond)
	if rec.mountCount() != 0 {
		t.Errorf("mountCount = %d for an already-mounted target, want 0 (mount should have been skipped)", rec.mountCount())
	}

	cancel()
	<-done
}

func TestWatchIgnoresUnrelatedFilenames(t *testing.T) {
	dir := withFakeByUUIDDir(t)
	target := t.TempDir()
	rec := &recorder{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, testUUID, target, Hooks{Resolve: rec.resolve(filepath.Join(dir, "fake-dev")), Mount: rec.mount, Unmount: rec.unmount})
	}()

	// A different UUID appearing must not trigger a mount of our target.
	makeFakeDevice(t, dir, "DEAD-BEEF")
	time.Sleep(100 * time.Millisecond)
	if rec.mountCount() != 0 {
		t.Errorf("mountCount = %d for unrelated uuid, want 0", rec.mountCount())
	}

	cancel()
	<-done
}

func TestWatchRequiresHooks(t *testing.T) {
	withFakeByUUIDDir(t)
	if err := Watch(context.Background(), testUUID, t.TempDir(), Hooks{}); err == nil {
		t.Fatal("expected error for missing Mount/Unmount hooks")
	}
}

func TestWatchRejectsInvalidUUID(t *testing.T) {
	withFakeByUUIDDir(t)
	rec := &recorder{}
	hooks := Hooks{Resolve: rec.resolve("x"), Mount: rec.mount, Unmount: rec.unmount}
	for _, bad := range []string{"../../../etc/passwd", "foo/bar", "foo bar", ""} {
		if err := Watch(context.Background(), bad, t.TempDir(), hooks); err == nil {
			t.Errorf("Watch(%q, ...) = nil error, want an error for an invalid uuid", bad)
		}
	}
}

// TestWatchRetriesWhenByUUIDDirMissing exercises the "directory doesn't
// exist yet" path: byUUIDDir is pointed at a not-yet-created subdirectory,
// so the initial InotifyAddWatch fails with ENOENT and Watch must retry
// with backoff (rather than returning an error) until the directory (and
// the device symlink in it) shows up.
func TestWatchRetriesWhenByUUIDDirMissing(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "by-uuid")
	orig := byUUIDDir
	byUUIDDir = missing
	t.Cleanup(func() { byUUIDDir = orig })

	target := t.TempDir()
	rec := &recorder{}
	devPath := filepath.Join(missing, "fake-dev")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, testUUID, target, Hooks{Resolve: rec.resolve(devPath), Mount: rec.mount, Unmount: rec.unmount})
	}()

	// Give Watch a moment to hit ENOENT and enter its retry loop before
	// the directory (and device) actually appear.
	time.Sleep(50 * time.Millisecond)
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeFakeDevice(t, missing, testUUID)

	waitFor(t, func() bool { return rec.mountCount() == 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after ctx cancellation")
	}
}

// TestParseInotifyEvents exercises the pure decoder against a hand-built
// buffer, independent of a real inotify fd.
func TestParseInotifyEvents(t *testing.T) {
	buf := buildInotifyEvent(t, 1, 0x100, "F9B1-23CB")
	buf = append(buf, buildInotifyEvent(t, 1, 0x200, "other")...)

	events := parseInotifyEvents(buf)
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Mask != 0x100 || events[0].Name != "F9B1-23CB" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if events[1].Mask != 0x200 || events[1].Name != "other" {
		t.Errorf("events[1] = %+v", events[1])
	}
}

// buildInotifyEvent packs a single inotify_event record (wd, mask,
// cookie, len, name padded to a multiple of 4 with NULs) as the kernel
// would return from read(2) on an inotify fd.
func buildInotifyEvent(t *testing.T, wd int32, mask uint32, name string) []byte {
	t.Helper()
	nameBytes := []byte(name)
	padded := len(nameBytes) + 1
	for padded%4 != 0 {
		padded++
	}
	buf := make([]byte, inotifyEventHeaderSize+padded)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(wd))
	binary.LittleEndian.PutUint32(buf[4:8], mask)
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(padded))
	copy(buf[inotifyEventHeaderSize:], nameBytes)
	return buf
}
