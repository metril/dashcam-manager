package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"dashcam/internal/auth"
	"dashcam/internal/config"
)

// validateSettingsPatch checks every key against knownSettings, its value
// against the expected JSON type and the per-key rules, and rewrites patch
// values to their canonical form (trimmed, URL normalized, lists
// lower-cased/deduplicated). stored is the current settings table; a value
// equal to what is already stored skips the semantic checks, so a UI that
// resends an untouched legacy value (say an SSD path outside /mnt set by
// env) is not rejected. It validates the whole patch before the caller
// writes anything, so a PUT either applies in full or not at all.
func validateSettingsPatch(patch, stored map[string]json.RawMessage, cur *Runtime) error {
	for key, raw := range patch {
		kind, ok := knownSettings[key]
		if !ok {
			return badSetting("unknown setting key %q", key)
		}
		switch kind {
		case settingsString:
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return badSetting("setting %q must be a JSON string", key)
			}
			if key != "oidc_client_secret" {
				s = strings.TrimSpace(s)
			}
			canon, err := canonicalString(key, s, stored, cur)
			if err != nil {
				return err
			}
			patch[key] = mustJSON(canon)
		case settingsInt:
			var f float64
			if err := json.Unmarshal(raw, &f); err != nil {
				return badSetting("setting %q must be a JSON number", key)
			}
			if math.Trunc(f) != f {
				return badSetting("setting %q must be an integer", key)
			}
			switch key {
			case "dashcam_poll_interval_s", "hash_throttle_mb_s":
				if f < 0 {
					return badSetting("setting %q must not be negative", key)
				}
			case "workers":
				if f < 1 || f > 64 {
					return badSetting("setting %q must be between 1 and 64", key)
				}
			case "max_ffmpeg_jobs":
				if f < 1 || f > 32 {
					return badSetting("setting %q must be between 1 and 32", key)
				}
			}
		case settingsBool:
			var b bool
			if err := json.Unmarshal(raw, &b); err != nil {
				return badSetting("setting %q must be true or false", key)
			}
		case settingsStringList:
			var l []string
			if err := json.Unmarshal(raw, &l); err != nil {
				return badSetting("setting %q must be a JSON array of strings", key)
			}
			canon, err := canonicalList(key, l)
			if err != nil {
				return err
			}
			patch[key] = mustJSON(canon)
		case settingsJSON:
			if key == "detection_filter" {
				var f DetectionFilter
				if err := json.Unmarshal(raw, &f); err != nil {
					return badSetting("setting %q must be a JSON object: %v", key, err)
				}
				if err := f.Validate(); err != nil {
					return badSetting("setting %q: %v", key, err)
				}
				// Persist the validated (min_conf-clamped) value rather
				// than whatever the caller sent, so a later GET always
				// reflects a filter that would itself pass Validate.
				canon, err := json.Marshal(f)
				if err != nil {
					return badSetting("setting %q: re-marshal: %v", key, err)
				}
				patch[key] = canon
			}
		default:
			// Defense-in-depth: every settingsKind added to knownSettings
			// must have a case above that actually validates it; without
			// this default, a future kind with no matching case would
			// silently skip validation instead of failing loudly.
			return badSetting("setting %q: unhandled settings kind %d", key, kind)
		}
	}

	// ssd_path and local_root must not nest: CIFS/local storages are
	// scanned under local_root and the SSD source under ssd_path.
	_, ssdIn := patch["ssd_path"]
	_, rootIn := patch["local_root"]
	if ssdIn || rootIn {
		ssd, root := cur.SSDPath, cur.LocalRoot
		if raw, ok := patch["ssd_path"]; ok {
			_ = json.Unmarshal(raw, &ssd)
		}
		if raw, ok := patch["local_root"]; ok {
			_ = json.Unmarshal(raw, &root)
		}
		if pathsOverlap(ssd, root) && !(isUnchanged("ssd_path", patch, stored) && isUnchanged("local_root", patch, stored)) {
			return badSetting("ssd_path and local_root must not overlap")
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// isUnchanged reports whether key is absent from patch or equal to stored.
func isUnchanged(key string, patch, stored map[string]json.RawMessage) bool {
	v, ok := patch[key]
	if !ok {
		return true
	}
	old, ok := stored[key]
	return ok && jsonEqual(old, v)
}

// canonicalString validates a string setting and returns its stored form.
func canonicalString(key, s string, stored map[string]json.RawMessage, cur *Runtime) (string, error) {
	var oldS string
	if raw, ok := stored[key]; ok {
		_ = json.Unmarshal(raw, &oldS)
		if strings.TrimSpace(oldS) == s && key != "public_url" && key != "oidc_client_secret" {
			return s, nil // untouched; don't re-judge a legacy value
		}
	}
	switch key {
	case "ssd_uuid":
		if s != "" && !config.ValidSSDUUID(s) {
			return "", badSetting("setting %q must be a valid filesystem UUID", key)
		}
	case "ssd_path", "local_root":
		if !config.ValidAbsPath(s, "/mnt") {
			return "", badSetting("setting %q must be a clean absolute path under /mnt", key)
		}
	case "qsv_device":
		if !config.ValidDevicePath(s) {
			return "", badSetting("setting %q must be a device node under /dev/dri/", key)
		}
	case "cuda_device":
		if !config.ValidCUDAIndex(s) {
			return "", badSetting("setting %q must be a GPU index such as 0", key)
		}
	case "ffmpeg_bin", "ffprobe_bin":
		if err := validateBinary(s); err != nil {
			return "", badSetting("setting %q %v", key, err)
		}
	case "public_url":
		if s == "" {
			if cur.OIDCEnabled {
				return "", &settingsError{status: http.StatusBadRequest, code: "oidc_test_required",
					msg: "public_url cannot be cleared while sign-in is enabled"}
			}
			return "", nil
		}
		u, err := config.ValidPublicURL(s)
		if err != nil {
			return "", badSetting("setting %q %v", key, err)
		}
		return u, nil
	case "timezone":
		if s != "" {
			if _, err := time.LoadLocation(s); err != nil {
				return "", badSetting("setting %q is not a known time zone", key)
			}
		}
	case "hw_mode":
		if !hwModes[s] {
			return "", badSetting("setting %q must be one of auto, nvenc, qsv, vaapi, software", key)
		}
	case "oidc_issuer":
		if s != "" {
			if err := config.ValidIssuerURL(s); err != nil {
				return "", badSetting("setting %q %v", key, err)
			}
		}
	case "oidc_client_id":
		if len(s) > 256 {
			return "", badSetting("setting %q is too long", key)
		}
	case "oidc_client_secret":
		if len(s) > 1024 {
			return "", badSetting("setting %q is too long", key)
		}
	case "oidc_groups_claim":
		if !claimNamePattern.MatchString(s) {
			return "", badSetting("setting %q must be a claim name such as groups", key)
		}
	}
	return s, nil
}

// validateBinary accepts a bare executable name (resolved via PATH) or the
// absolute path of an existing executable file.
func validateBinary(s string) error {
	if bareBinaryName.MatchString(s) {
		return nil
	}
	if !config.ValidAbsPath(s) {
		return fmt.Errorf("must be a bare name such as ffmpeg or an absolute path")
	}
	fi, err := os.Stat(s)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("must be an existing executable file")
	}
	return nil
}

// canonicalList validates and normalizes an OIDC list setting.
func canonicalList(key string, l []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range l {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > 256 || strings.ContainsAny(v, " \t\r\n,") {
			return nil, badSetting("setting %q entries must be non-empty values without spaces or commas", key)
		}
		if key == "oidc_allowed_emails" {
			v = strings.ToLower(v)
			if !strings.Contains(v, "@") {
				return nil, badSetting("setting %q entries must be email addresses", key)
			}
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if key == "oidc_scopes" {
		if len(out) == 0 {
			return []string{"openid", "email", "profile"}, nil
		}
		if !seen["openid"] {
			return nil, badSetting("setting %q must include openid", key)
		}
	}
	return out, nil
}

// guardOIDCPatch enforces what a plain PUT may do to the sign-in settings:
// turning OIDC on, or changing the issuer, client or public URL while it is
// on, needs the test login (400 oidc_test_required); and an allow-list edit
// may not lock out the caller's own session (409 oidc_lockout), unless
// DASHCAM_AUTH_DISABLE is set.
func guardOIDCPatch(r *http.Request, patch map[string]json.RawMessage, changed map[string]bool, old *Runtime, authn *auth.Authenticator) error {
	testRequired := func(what string) error {
		return &settingsError{status: http.StatusBadRequest, code: "oidc_test_required",
			msg: what + " must go through the sign-in test: use Test and enable in the sign-in settings"}
	}
	disabling := false
	if raw, ok := patch["oidc_enabled"]; ok {
		var on bool
		_ = json.Unmarshal(raw, &on)
		if on && !old.OIDCEnabled {
			return testRequired("Enabling sign-in")
		}
		disabling = !on
	}
	if !old.OIDCEnabled || disabling {
		return nil
	}
	for _, k := range []string{"oidc_issuer", "oidc_client_id", "oidc_client_secret", "public_url"} {
		if changed[k] {
			return testRequired("Changing " + k)
		}
	}

	if !changed["oidc_scopes"] && !changed["oidc_allowed_emails"] && !changed["oidc_allowed_groups"] && !changed["oidc_groups_claim"] {
		return nil
	}
	if authn == nil || authn.AuthDisabled() {
		return nil
	}
	s, ok := authn.SessionFrom(r)
	if !ok {
		return nil
	}
	next := old.OIDC
	if raw, ok := patch["oidc_scopes"]; ok {
		_ = json.Unmarshal(raw, &next.Scopes)
	}
	if raw, ok := patch["oidc_allowed_emails"]; ok {
		_ = json.Unmarshal(raw, &next.AllowedEmails)
	}
	if raw, ok := patch["oidc_allowed_groups"]; ok {
		_ = json.Unmarshal(raw, &next.AllowedGroups)
	}
	if raw, ok := patch["oidc_groups_claim"]; ok {
		_ = json.Unmarshal(raw, &next.GroupsClaim)
	}
	if !authn.SessionAuthorized(s, next) {
		return &settingsError{status: http.StatusConflict, code: "oidc_lockout",
			msg: "this change would lock out your own account; add yourself to the allowed emails or groups first"}
	}
	return nil
}
