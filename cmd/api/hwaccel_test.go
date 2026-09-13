package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"dashcam/internal/media"
)

// resetCapsForTest points capsReady/currentCapabilities at fresh,
// test-local state and restores the originals on cleanup, so this test can
// control exactly when "detection" completes without depending on (or
// racing) any other test's boot-time registerJobHandlers goroutine.
func resetCapsForTest(t *testing.T) {
	t.Helper()
	origReady := capsReady
	var origCaps media.Capabilities
	if v, ok := currentCapabilities.Load().(media.Capabilities); ok {
		origCaps = v
	}
	capsReady = make(chan struct{})
	currentCapabilities = atomic.Value{}
	t.Cleanup(func() {
		capsReady = origReady
		currentCapabilities = atomic.Value{}
		currentCapabilities.Store(origCaps)
	})
}

// TestWaitCapabilitiesBlocksUntilDetectionCompletes covers fix round 2:
// a request landing before the boot goroutine's DetectCapabilities call
// finishes must see the real detected capabilities, not the all-false
// default getCapabilities() returns pre-detection.
func TestWaitCapabilitiesBlocksUntilDetectionCompletes(t *testing.T) {
	resetCapsForTest(t)

	detected := media.Capabilities{HEVCQSV: true, Libx265: true}
	go func() {
		time.Sleep(50 * time.Millisecond)
		setCapabilities(detected)
	}()

	// Immediately after starting the "probe" goroutine, getCapabilities()
	// must still report the pre-detection default...
	if got := getCapabilities(); got != (media.Capabilities{}) {
		t.Fatalf("getCapabilities() before detection = %+v, want zero value", got)
	}

	// ...but waitCapabilities must block for the real result rather than
	// racing it.
	got := waitCapabilities(context.Background())
	if got != detected {
		t.Fatalf("waitCapabilities() = %+v, want %+v", got, detected)
	}
}

// TestWaitCapabilitiesRespectsContextDeadline covers the "cap at 30s" (and
// generally: respect a caller's own shorter deadline) requirement — if
// detection never completes, waitCapabilities must still return once ctx
// is done rather than blocking forever.
func TestWaitCapabilitiesRespectsContextDeadline(t *testing.T) {
	resetCapsForTest(t)
	// No setCapabilities call at all: detection never "completes".

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	got := waitCapabilities(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waitCapabilities took %s, want it to return promptly once ctx is done", elapsed)
	}
	if got != (media.Capabilities{}) {
		t.Fatalf("waitCapabilities() on ctx deadline = %+v, want zero value", got)
	}
}

// TestResolveHWModeAcceptsNVENCOverride covers the "nvenc" settings.hw_mode
// override added alongside NVIDIA support: resolveHWMode must return it
// straight from settings without falling through to a real media.Detect
// probe (which would need actual hardware/ffmpeg to succeed).
func TestResolveHWModeAcceptsNVENCOverride(t *testing.T) {
	_, store := newTestMuxWithStore(t)
	ctx := context.Background()
	if err := store.SettingsSet(ctx, "hw_mode", []byte(`"nvenc"`)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	got := resolveHWMode(ctx, store, media.Devices{})
	if got != media.ModeNVENC {
		t.Fatalf("resolveHWMode() = %q, want %q", got, media.ModeNVENC)
	}
}
