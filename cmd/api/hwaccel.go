package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/media"
)

// currentHWMode holds the hardware-acceleration mode resolved once at boot
// (registerJobHandlers, before the HTTP server starts accepting requests)
// so the on-demand thumbnail/sprite handlers don't re-run ffmpeg's
// `-hwaccels` probe on every request.
var currentHWMode atomic.Value // media.Mode

func setHWMode(m media.Mode) { currentHWMode.Store(m) }

// getHWMode returns the last mode set by setHWMode, defaulting to
// ModeSoftware before boot-time detection has run (e.g. a stray handler
// call in a test that skips registerJobHandlers).
func getHWMode() media.Mode {
	if v, ok := currentHWMode.Load().(media.Mode); ok {
		return v
	}
	return media.ModeSoftware
}

// resolveHWMode returns the settings.hw_mode override if it names a known
// mode ("nvenc"/"qsv"/"vaapi"/"software"), otherwise probes ffmpeg via
// media.Detect against devs.
func resolveHWMode(ctx context.Context, store *db.DB, devs media.Devices) media.Mode {
	if raw, err := store.SettingsGet(ctx, "hw_mode"); err == nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch media.Mode(s) {
			case media.ModeNVENC, media.ModeQSV, media.ModeVAAPI, media.ModeSoftware:
				return media.Mode(s)
			}
		}
	}
	return media.Detect(ctx, devs)
}

// currentCapabilities mirrors currentHWMode: resolved once at boot
// (registerJobHandlers) via media.DetectCapabilities, read by both the
// export validation path (validateExportCodecAvailable) and GET
// /api/export/capabilities so the UI can disable codecs this host can't
// encode.
var currentCapabilities atomic.Value // media.Capabilities

// capsReady is closed by registerJobHandlers' boot goroutine once
// DetectCapabilities has stored a real result via setCapabilities. Unlike
// getHWMode (where a stray software-path thumbnail from an in-flight probe
// is harmless), an all-false media.Capabilities{} read before detection
// finishes makes validateExportCodecAvailable wrongly 400 every h265/av1
// export request for the ~20-40s the probe takes — waitCapabilities lets
// callers block for the real result instead of racing it.
var capsReady = make(chan struct{})

func setCapabilities(c media.Capabilities) {
	currentCapabilities.Store(c)
	// Guard against a second call (e.g. a test re-running
	// registerJobHandlers against a fresh capsReady would panic on a
	// double-close, but within one process capsReady is only ever closed
	// once here).
	select {
	case <-capsReady:
	default:
		close(capsReady)
	}
}

// getCapabilities returns the last value set by setCapabilities, defaulting
// to an all-false media.Capabilities{} before boot-time detection has run
// (or in a test that skips registerJobHandlers) — same "software/least
// capable until proven otherwise" rationale as getHWMode. Prefer
// waitCapabilities where the caller can afford to block briefly for the
// real, detected result instead of racing the boot probe.
func getCapabilities() media.Capabilities {
	if v, ok := currentCapabilities.Load().(media.Capabilities); ok {
		return v
	}
	return media.Capabilities{}
}

// waitCapabilities blocks until boot-time capability detection has
// completed (setCapabilities called at least once) or ctx is done,
// whichever comes first, then returns getCapabilities(). Used by both GET
// /api/export/capabilities and export codec validation so a request that
// lands in the ~20-40s window before the boot goroutine's
// media.DetectCapabilities probe finishes waits for the real result
// instead of seeing an all-false media.Capabilities{} and wrongly
// rejecting h265/av1.
func waitCapabilities(ctx context.Context) media.Capabilities {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case <-capsReady:
	case <-ctx.Done():
	}
	return getCapabilities()
}
