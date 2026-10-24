package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"dashcam/internal/auth"
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
	settingsBool
	settingsStringList
)

// knownSettings is the whitelist of keys PUT /api/settings may write, with
// their expected JSON type. Unknown keys or type mismatches are rejected
// with 400 rather than silently stored — settings drive real behavior
// (ffmpeg device paths, worker counts) and a typo should fail loudly, not
// become an inert extra key. schema_version is deliberately absent: it's
// system-managed by internal/db.InitSchema, not user-editable, and so are
// the "_enc" keys (the encrypted OIDC client secret is written via the
// write-only oidc_client_secret).
var knownSettings = map[string]settingsKind{
	"ssd_path":        settingsString,
	"ssd_uuid":        settingsString,
	"qsv_device":      settingsString,
	"cuda_device":     settingsString,
	"local_root":      settingsString,
	"ffmpeg_bin":      settingsString,
	"ffprobe_bin":     settingsString,
	"public_url":      settingsString,
	"max_ffmpeg_jobs": settingsInt,
	"workers":         settingsInt,
	"timezone":        settingsString,
	"hw_mode":         settingsString, // "auto" | "nvenc" | "qsv" | "vaapi" | "software"
	// detection_filter is the browser UI's persisted default
	// DetectionFilter (see detection_filter.go) — the same shape POST
	// /api/export accepts as overlay_filter.
	"detection_filter": settingsJSON,
	// dashcam_poll_interval_s: 0 disables dashcam.RunPollLoop's per-tick
	// auto-pull (see main.go's seedDefaultSettings); any positive value is
	// the minimum interval, in seconds, between automatic dashcam_pull
	// enqueues for a given dashcam source.
	"dashcam_poll_interval_s": settingsInt,
	// hash_throttle_mb_s caps the hash_backfill job's (cmd/api/jobs_hash.go)
	// average read throughput in MB/s across all files it hashes; 0 means
	// unthrottled. Defaults to 40 (seedDefaultSettings, main.go) so a
	// backfill over a cifs share doesn't saturate the link out of the box.
	"hash_throttle_mb_s": settingsInt,

	// OIDC. oidc_enabled=true, oidc_issuer, oidc_client_id,
	// oidc_client_secret (write-only) and public_url cannot be changed by a
	// plain PUT while OIDC is on (or being turned on): they go through the
	// test-before-save flow (handlers_oidc.go) so a broken config can never
	// lock the operator out.
	"oidc_enabled":        settingsBool,
	"oidc_issuer":         settingsString,
	"oidc_client_id":      settingsString,
	"oidc_client_secret":  settingsString,
	"oidc_scopes":         settingsStringList,
	"oidc_allowed_emails": settingsStringList,
	"oidc_allowed_groups": settingsStringList,
	"oidc_groups_claim":   settingsString,
}

// settingsError is a validation failure with the HTTP status and optional
// machine-readable code the client branches on.
type settingsError struct {
	status int
	code   string
	msg    string
}

func (e *settingsError) Error() string { return e.msg }

func badSetting(format string, args ...any) error {
	return &settingsError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

func writeSettingsError(w http.ResponseWriter, err error) {
	var se *settingsError
	if errors.As(err, &se) {
		if se.code != "" {
			writeJSONErrorCode(w, se.status, se.code, se.msg)
		} else {
			writeJSONError(w, se.status, se.msg)
		}
		return
	}
	writeJSONError(w, http.StatusBadRequest, err.Error())
}

var hwModes = map[string]bool{"auto": true, "nvenc": true, "qsv": true, "vaapi": true, "software": true}

var bareBinaryName = regexp.MustCompile(`^[a-z0-9._-]+$`)

var claimNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/-]+$`)

// settingsView is GET /api/settings' body: every stored setting except the
// "_enc" secrets, plus oidc_client_secret_set, oidc_secret_error and the
// oidc_* keys filled from the runtime when absent, so a client always sees
// the full OIDC shape.
func settingsView(all map[string]json.RawMessage, r *Runtime) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(all)+8)
	for k, v := range all {
		if !strings.HasSuffix(k, "_enc") {
			out[k] = v
		}
	}
	out["oidc_enabled"] = mustJSON(r.OIDCEnabled)
	out["oidc_client_secret_set"] = mustJSON(r.OIDCSecretSet)
	out["oidc_secret_error"] = mustJSON(r.OIDCSecretErr)
	for k, v := range map[string]json.RawMessage{
		"oidc_issuer": mustJSON(r.OIDC.Issuer), "oidc_client_id": mustJSON(r.OIDC.ClientID),
		"oidc_scopes": mustJSON(nonNil(r.OIDC.Scopes)), "oidc_allowed_emails": mustJSON(nonNil(r.OIDC.AllowedEmails)),
		"oidc_allowed_groups": mustJSON(nonNil(r.OIDC.AllowedGroups)), "oidc_groups_claim": mustJSON(r.OIDC.GroupsClaim),
	} {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// handleSettingsGet returns every settings row as a flat JSON object of
// key -> value (value already being JSON, so it's embedded raw rather than
// double-encoded as a string), minus secrets (see settingsView).
func handleSettingsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all, err := store.SettingsGetAll(r.Context())
		if err != nil {
			writeInternalError(w, "settings get", err)
			return
		}
		writeJSON(w, http.StatusOK, settingsView(all, getRT()))
	}
}

// handleSettingsPut merges the JSON object in the request body into
// settings: every key present is upserted, keys not present are left
// untouched. The body must be a JSON object whose keys are all in
// knownSettings and whose values match the expected type and per-key rules;
// anything else is a 400 (guarded OIDC keys: 400 code "oidc_test_required";
// an allow-list edit that would lock out the caller: 409 code
// "oidc_lockout"). On success the new values are applied live and the
// response is {settings, restart_required, applied, apply_errors}.
func handleSettingsPut(store *db.DB, ap *applier, authn *auth.Authenticator) http.HandlerFunc {
	var mu sync.Mutex // serialize read-modify-apply so old/new snapshots are consistent
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

		mu.Lock()
		defer mu.Unlock()
		ctx := r.Context()
		stored, err := store.SettingsGetAll(ctx)
		if err != nil {
			writeInternalError(w, "settings get", err)
			return
		}
		old := getRT()

		if err := validateSettingsPatch(patch, stored, old); err != nil {
			writeSettingsError(w, err)
			return
		}
		changed := changedKeys(patch, stored)
		if err := guardOIDCPatch(r, patch, changed, old, authn); err != nil {
			writeSettingsError(w, err)
			return
		}

		// The client secret is write-only and stored encrypted under a
		// different key.
		writes := make(map[string]json.RawMessage, len(patch))
		for k, v := range patch {
			if k != "oidc_client_secret" {
				writes[k] = v
			}
		}
		if raw, ok := patch["oidc_client_secret"]; ok {
			var plain, enc string
			_ = json.Unmarshal(raw, &plain)
			if plain != "" {
				if enc, err = encryptSecret(ap.secret, plain); err != nil {
					writeInternalError(w, "encrypt oidc client secret", err)
					return
				}
			}
			writes["oidc_client_secret_enc"] = mustJSON(enc)
		}

		if err := store.SettingsSetMany(ctx, writes); err != nil {
			writeInternalError(w, "settings set", err)
			return
		}
		cur := refreshRuntime(ctx, store, ap.secret)
		res := ap.Apply(ctx, old, cur, changed)
		for k, msg := range res.Errors {
			log.Printf("[api] settings: apply %s: %s", k, msg)
		}

		all, err := store.SettingsGetAll(ctx)
		if err != nil {
			writeInternalError(w, "settings get after put", err)
			return
		}
		sort.Strings(res.Applied)
		writeJSON(w, http.StatusOK, map[string]any{
			"settings":         settingsView(all, cur),
			"restart_required": ap.RestartRequired(),
			"applied":          res.Applied,
			"apply_errors":     res.Errors,
		})
	}
}

// changedKeys returns the patch keys whose canonical value differs from
// what is stored (oidc_client_secret always counts: it is write-only).
func changedKeys(patch, stored map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for k, v := range patch {
		if old, ok := stored[k]; k == "oidc_client_secret" || !ok || !jsonEqual(old, v) {
			out[k] = true
		}
	}
	return out
}

func jsonEqual(a, b json.RawMessage) bool {
	var ba, bb bytes.Buffer
	if json.Compact(&ba, a) != nil || json.Compact(&bb, b) != nil {
		return false
	}
	return bytes.Equal(ba.Bytes(), bb.Bytes())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeJSONErrorCode is writeJSONError plus a stable, machine-readable
// "code" field for a client that needs to branch on the failure reason
// rather than pattern-match the human-readable message (e.g. the frontend
// showing a distinct "Regenerate" hint for code:"key_unreadable").
func writeJSONErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// writeInternalError logs the real error (which may contain sqlite
// internals — file paths, constraint names) server-side and returns a
// generic message to the client, so a 500 never leaks implementation
// details.
func writeInternalError(w http.ResponseWriter, context string, err error) {
	log.Printf("[api] settings: %s: %v", context, err)
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}
