package main

import (
	"context"
	"encoding/json"
	"sync/atomic"

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
// mode ("qsv"/"vaapi"/"software"), otherwise probes ffmpeg via
// media.Detect against device.
func resolveHWMode(ctx context.Context, store *db.DB, device string) media.Mode {
	if raw, err := store.SettingsGet(ctx, "hw_mode"); err == nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch media.Mode(s) {
			case media.ModeQSV, media.ModeVAAPI, media.ModeSoftware:
				return media.Mode(s)
			}
		}
	}
	return media.Detect(ctx, device)
}

// currentCapabilities mirrors currentHWMode: resolved once at boot
// (registerJobHandlers) via media.DetectCapabilities, read by both the
// export validation path (validateExportCodecAvailable) and GET
// /api/export/capabilities so the UI can disable codecs this host can't
// encode.
var currentCapabilities atomic.Value // media.Capabilities

func setCapabilities(c media.Capabilities) { currentCapabilities.Store(c) }

// getCapabilities returns the last value set by setCapabilities, defaulting
// to an all-false media.Capabilities{} before boot-time detection has run
// (or in a test that skips registerJobHandlers) — same "software/least
// capable until proven otherwise" rationale as getHWMode.
func getCapabilities() media.Capabilities {
	if v, ok := currentCapabilities.Load().(media.Capabilities); ok {
		return v
	}
	return media.Capabilities{}
}
