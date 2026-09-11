package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"

	"dashcam/internal/config"
	"dashcam/internal/db"
)

// maxSettingsBodyBytes caps the PUT /api/settings request body. Settings are
// a small flat key/value map; anything near this size is either abuse or a
// caller bug, not a legitimate request.
const maxSettingsBodyBytes = 64 * 1024

// settingsKind is the accepted JSON type for a known settings key.
type settingsKind int

const (
	settingsString settingsKind = iota
	settingsInt
	// settingsJSON is a structured JSON value validated by a per-key
	// custom check in validateSettingsPatch (currently only
	// "detection_filter" -> DetectionFilter), rather than a bare
	// string/number type check.
	settingsJSON
)

// knownSettings is the whitelist of keys PUT /api/settings may write, with
// their expected JSON type. Unknown keys or type mismatches are rejected
// with 400 rather than silently stored — settings drive real behavior
// (ffmpeg device paths, worker counts) and a typo should fail loudly, not
// become an inert extra key. schema_version is deliberately absent: it's
// system-managed by internal/db.InitSchema, not user-editable.
var knownSettings = map[string]settingsKind{
	"ssd_path":        settingsString,
	"ssd_uuid":        settingsString,
	"qsv_device":      settingsString,
	"max_ffmpeg_jobs": settingsInt,
	"workers":         settingsInt,
	"timezone":        settingsString,
	"hw_mode":         settingsString, // "auto" | "qsv" | "vaapi" | "software"
	// detection_filter is the browser UI's persisted default
	// DetectionFilter (see detection_filter.go) — the same shape POST
	// /api/export accepts as overlay_filter.
	"detection_filter": settingsJSON,
	// dashcam_poll_interval_s: 0 disables dashcam.RunPollLoop's per-tick
	// auto-pull (see main.go's seedDefaultSettings); any positive value is
	// the minimum interval, in seconds, between automatic dashcam_pull
	// enqueues for a given dashcam source.
	"dashcam_poll_interval_s": settingsInt,
}

// handleSettingsGet returns every settings row as a flat JSON object of
// key -> value (value already being JSON, so it's embedded raw rather than
// double-encoded as a string).
func handleSettingsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all, err := store.SettingsGetAll(r.Context())
		if err != nil {
			writeInternalError(w, "settings get", err)
			return
		}
		writeJSON(w, http.StatusOK, all)
	}
}

// handleSettingsPut merges the JSON object in the request body into
// settings: every key present is upserted, keys not present are left
// untouched. The body must be a JSON object whose keys are all in
// knownSettings and whose values match the expected type; anything else is
// a 400. Returns the full merged settings object on success.
func handleSettingsPut(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBodyBytes)

		var patch map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if patch == nil {
			// Decoding a JSON `null` body succeeds with a nil map and no
			// error; a settings PUT with no keys to set is almost
			// certainly a caller bug, so reject it explicitly.
			writeJSONError(w, http.StatusBadRequest, "request body must be a JSON object")
			return
		}

		if err := validateSettingsPatch(patch); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		if err := store.SettingsSetMany(r.Context(), patch); err != nil {
			writeInternalError(w, "settings set", err)
			return
		}
		all, err := store.SettingsGetAll(r.Context())
		if err != nil {
			writeInternalError(w, "settings get after put", err)
			return
		}
		writeJSON(w, http.StatusOK, all)
	}
}

// validateSettingsPatch checks every key against knownSettings and its
// value against the expected JSON type. It validates the whole patch before
// the caller writes anything, so a PUT either applies in full or not at
// all rather than partially failing partway through.
func validateSettingsPatch(patch map[string]json.RawMessage) error {
	for key, raw := range patch {
		kind, ok := knownSettings[key]
		if !ok {
			return fmt.Errorf("unknown setting key %q", key)
		}
		switch kind {
		case settingsString:
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("setting %q must be a JSON string", key)
			}
			if key == "ssd_uuid" {
				if u := strings.TrimSpace(s); u != "" && !config.ValidSSDUUID(u) {
					return fmt.Errorf("setting %q must be a valid filesystem UUID", key)
				}
			}
		case settingsInt:
			var f float64
			if err := json.Unmarshal(raw, &f); err != nil {
				return fmt.Errorf("setting %q must be a JSON number", key)
			}
			if math.Trunc(f) != f {
				return fmt.Errorf("setting %q must be an integer", key)
			}
		case settingsJSON:
			if key == "detection_filter" {
				var f DetectionFilter
				if err := json.Unmarshal(raw, &f); err != nil {
					return fmt.Errorf("setting %q must be a JSON object: %w", key, err)
				}
				if err := f.Validate(); err != nil {
					return fmt.Errorf("setting %q: %w", key, err)
				}
				// Persist the validated (min_conf-clamped) value rather
				// than whatever the caller sent, so a later GET always
				// reflects a filter that would itself pass Validate.
				canon, err := json.Marshal(f)
				if err != nil {
					return fmt.Errorf("setting %q: re-marshal: %w", key, err)
				}
				patch[key] = canon
			}
		default:
			// Fix round 1 (minor): defense-in-depth — every settingsKind
			// added to knownSettings must have a case above that actually
			// validates it; without this default, a future kind with no
			// matching case would silently skip validation instead of
			// failing loudly (the `ok` check above only rejects keys
			// missing from knownSettings entirely, not a kind that's
			// present but unhandled here).
			return fmt.Errorf("setting %q: unhandled settings kind %d", key, kind)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeInternalError logs the real error (which may contain sqlite
// internals — file paths, constraint names) server-side and returns a
// generic message to the client, so a 500 never leaks implementation
// details.
func writeInternalError(w http.ResponseWriter, context string, err error) {
	log.Printf("[api] settings: %s: %v", context, err)
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}
