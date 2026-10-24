// Package config centralizes environment-derived configuration for the
// dashcam-manager API server, with sane defaults for local development.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// defaultWorkers is used both as the DASHCAM_WORKERS fallback and when the
// value present is not a valid integer.
const defaultWorkers = 4

// ssdUUIDPattern is the allowed shape of an SSD_UUID value: filesystem
// UUIDs under /dev/disk/by-uuid are hex digits and hyphens, but this is
// intentionally a bit more permissive (letters+digits+hyphens) to also
// accept e.g. FAT/exFAT's shorter XXXX-XXXX volume-serial style UUIDs.
// The real reason this is enforced at all is security, not cosmetics: the
// value is joined onto /dev/disk/by-uuid/<value> and then mounted, so
// without validation a value containing "/" or ".." could walk the
// symlink resolution outside that directory and mount an arbitrary host
// device.
var ssdUUIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// ValidSSDUUID reports whether uuid is safe to join onto
// /dev/disk/by-uuid (see ssdUUIDPattern) — used both here (to sanitize
// DASHCAM_SSD_UUID at boot) and by cmd/api (to sanitize the same value
// when it's overridden from the "ssd_uuid" setting) and internal/ssd (as
// a defense-in-depth check right before it's used).
func ValidSSDUUID(uuid string) bool {
	return ssdUUIDPattern.MatchString(uuid)
}

// Config holds the bootstrap configuration read from the environment:
// only what is needed before the database can be opened. Everything else
// lives in the settings table (see cmd/api's Runtime) and is edited in the
// web UI; the DASHCAM_* variables for those values only seed the table on
// first start (Seed).
type Config struct {
	// DB is the path to the SQLite database file.
	DB string
	// Cache is the directory used for thumbnails, sprites, HLS segments, etc.
	Cache string
	// Addr is the listen address for the HTTP server.
	Addr string
	// SecretKey encrypts stored share credentials and the OIDC client
	// secret. Load reads DASHCAM_SECRET_KEY only; main replaces it with the
	// ResolveSecretKey result (env, else <data dir>/secret.key).
	SecretKey string
	// SecretKeySource is "env", "file" or "generated" (set by main).
	SecretKeySource string
	// AuthDisable (DASHCAM_AUTH_DISABLE=1) bypasses the sign-in gate even
	// when OIDC is enabled — the recovery hatch for a broken IdP config.
	AuthDisable bool
	// Seed holds the env values for settings that live in the DB; they are
	// inserted only if the key is absent.
	Seed SeedValues
	// SeedSet records which settings keys had a non-empty env variable.
	SeedSet map[string]bool
}

// SeedValues is the env-provided first-start value of every DB-backed
// setting.
type SeedValues struct {
	// SSDPath is the read-only mount point of the recorder's SSD.
	SSDPath string
	// SSDUUID is the filesystem UUID (as it appears under
	// /dev/disk/by-uuid) of the recorder's USB SSD, watched by
	// internal/ssd for in-container hotplug mount/unmount. Empty disables
	// the watcher.
	SSDUUID string
	// QSVDevice is the DRI render node used for Intel Quick Sync ffmpeg ops.
	QSVDevice string
	// CUDADevice is the CUDA device index used for NVIDIA NVENC/NVDEC
	// ffmpeg ops (e.g. "0" for the first GPU).
	CUDADevice string
	// Workers is the number of concurrent background job workers.
	Workers int
	// LocalRoot is the directory kind=local storages' paths must live
	// under — bind-mounted from ./shares in compose.yaml, the same tree
	// CIFS shares auto-mount under (share.DefaultMountBase).
	LocalRoot string
	// FFmpeg/FFprobe name the binaries to exec (jellyfin-ffmpeg is on PATH
	// in the container image).
	FFmpeg  string
	FFprobe string
	// PublicURL is the externally visible base URL (scheme://host[:port],
	// no trailing slash) the app is reached at — behind a reverse proxy
	// this differs from the request's own Host. Used for the OIDC redirect
	// URI and as an extra accepted CSRF Origin. Only this configured value
	// is trusted; no X-Forwarded-* header is ever read.
	PublicURL string
	// OIDC configures the optional built-in OpenID Connect login.
	OIDC OIDCConfig
}

// OIDCConfig holds the OIDC login settings.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// PublicURL mirrors the public_url setting so the auth package only
	// needs this struct.
	PublicURL string
	// Scopes defaults to "openid email profile".
	Scopes []string
	// AllowedEmails (lower-cased) and AllowedGroups restrict who may sign
	// in; with both empty any user the IdP authenticates is allowed.
	AllowedEmails []string
	AllowedGroups []string
	// GroupsClaim is the ID token claim holding the user's groups.
	GroupsClaim string

	// anySet records that at least one DASHCAM_OIDC_* variable was non-empty
	// (set by Load), for partial's check.
	anySet bool
}

// Partial returns an error if some DASHCAM_OIDC_* setting is present but
// ISSUER or CLIENT_ID is missing; the caller skips seeding OIDC then.
func (o OIDCConfig) Partial() error {
	if o.Enabled() {
		return nil
	}
	if o.Issuer == "" && o.ClientID == "" && o.ClientSecret == "" && !o.anySet {
		return nil
	}
	missing := "DASHCAM_OIDC_ISSUER"
	if o.Issuer != "" {
		missing = "DASHCAM_OIDC_CLIENT_ID"
	}
	return fmt.Errorf("OIDC is partially configured but %s is empty; set both DASHCAM_OIDC_ISSUER and DASHCAM_OIDC_CLIENT_ID, or unset every DASHCAM_OIDC_* variable", missing)
}

// Enabled reports whether OIDC login is configured.
func (o OIDCConfig) Enabled() bool {
	return o.Issuer != "" && o.ClientID != ""
}

// Load reads configuration from the environment, applying defaults for any
// variable that is unset, empty, or (for Workers) not a valid integer.
func Load() Config {
	ssdUUID := getEnv("DASHCAM_SSD_UUID", "")
	if ssdUUID != "" && !ValidSSDUUID(ssdUUID) {
		log.Printf("[config] invalid DASHCAM_SSD_UUID=%q (must match %s); disabling the SSD hotplug watcher", ssdUUID, ssdUUIDPattern)
		ssdUUID = ""
	}

	publicURL := strings.TrimRight(getEnv("DASHCAM_PUBLIC_URL", ""), "/")

	set := map[string]bool{}
	for key, env := range map[string]string{
		"ssd_path": "DASHCAM_SSD_PATH", "ssd_uuid": "DASHCAM_SSD_UUID", "qsv_device": "DASHCAM_QSV_DEVICE",
		"cuda_device": "DASHCAM_CUDA_DEVICE", "workers": "DASHCAM_WORKERS", "local_root": "DASHCAM_LOCAL_ROOT",
		"ffmpeg_bin": "DASHCAM_FFMPEG", "ffprobe_bin": "DASHCAM_FFPROBE", "public_url": "DASHCAM_PUBLIC_URL",
		"oidc_issuer": "DASHCAM_OIDC_ISSUER", "oidc_client_id": "DASHCAM_OIDC_CLIENT_ID",
		"oidc_client_secret": "DASHCAM_OIDC_CLIENT_SECRET", "oidc_scopes": "DASHCAM_OIDC_SCOPES",
		"oidc_allowed_emails": "DASHCAM_OIDC_ALLOWED_EMAILS", "oidc_allowed_groups": "DASHCAM_OIDC_ALLOWED_GROUPS",
		"oidc_groups_claim": "DASHCAM_OIDC_GROUPS_CLAIM",
	} {
		if os.Getenv(env) != "" {
			set[key] = true
		}
	}

	return Config{
		DB:          getEnv("DASHCAM_DB", "/data/dashcam.db"),
		Cache:       getEnv("DASHCAM_CACHE", "/cache"),
		Addr:        getEnv("DASHCAM_ADDR", ":8080"),
		SecretKey:   getEnv("DASHCAM_SECRET_KEY", ""),
		AuthDisable: os.Getenv("DASHCAM_AUTH_DISABLE") == "1",
		SeedSet:     set,
		Seed: SeedValues{
			SSDPath:    getEnv("DASHCAM_SSD_PATH", "/mnt/dashcam"),
			SSDUUID:    ssdUUID,
			QSVDevice:  getEnv("DASHCAM_QSV_DEVICE", "/dev/dri/renderD128"),
			CUDADevice: getEnv("DASHCAM_CUDA_DEVICE", "0"),
			Workers:    getEnvInt("DASHCAM_WORKERS", defaultWorkers),
			LocalRoot:  getEnv("DASHCAM_LOCAL_ROOT", "/mnt/shares"),
			FFmpeg:     getEnv("DASHCAM_FFMPEG", "ffmpeg"),
			FFprobe:    getEnv("DASHCAM_FFPROBE", "ffprobe"),
			PublicURL:  publicURL,
			OIDC: OIDCConfig{
				Issuer:        getEnv("DASHCAM_OIDC_ISSUER", ""),
				ClientID:      getEnv("DASHCAM_OIDC_CLIENT_ID", ""),
				ClientSecret:  getEnv("DASHCAM_OIDC_CLIENT_SECRET", ""),
				PublicURL:     publicURL,
				Scopes:        scopesOrDefault(splitList(os.Getenv("DASHCAM_OIDC_SCOPES"), false)),
				AllowedEmails: splitList(os.Getenv("DASHCAM_OIDC_ALLOWED_EMAILS"), true),
				AllowedGroups: splitList(os.Getenv("DASHCAM_OIDC_ALLOWED_GROUPS"), false),
				GroupsClaim:   getEnv("DASHCAM_OIDC_GROUPS_CLAIM", "groups"),
				anySet:        anyEnvSet(oidcEnvVars),
			},
		},
	}
}

// Validate logs warnings for configuration that deserves operator
// attention. Call once after Load(). Nothing here is fatal: OIDC and the
// public URL are validated when saved through the settings API.
func (c Config) Validate() {
	if c.AuthDisable {
		log.Println("[config] WARNING: DASHCAM_AUTH_DISABLE=1 — sign-in is bypassed for every request, even if OIDC is enabled")
	}
	if err := c.Seed.OIDC.Partial(); err != nil {
		log.Printf("[config] %v — skipping the OIDC env seed", err)
	}
}

// ResolveSecretKey returns the key encrypting stored credentials and where
// it came from ("env", "file" or "generated"). DASHCAM_SECRET_KEY wins so
// credentials encrypted by an existing install keep decrypting; otherwise
// <dataDir>/secret.key is read, or generated (32 random bytes, base64,
// mode 0600) on first start.
func ResolveSecretKey(dataDir string) (key, source string, err error) {
	path := filepath.Join(dataDir, "secret.key")
	if env := os.Getenv("DASHCAM_SECRET_KEY"); env != "" {
		if b, rerr := os.ReadFile(path); rerr == nil && strings.TrimSpace(string(b)) != strings.TrimSpace(env) {
			log.Printf("[config] DASHCAM_SECRET_KEY differs from %s; using the env value so existing encrypted credentials keep working", path)
		}
		return env, "env", nil
	}
	if b, rerr := os.ReadFile(path); rerr == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, "file", nil
		}
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return "", "", fmt.Errorf("config: read %s: %w", path, rerr)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("config: generate secret key: %w", err)
	}
	key = base64.StdEncoding.EncodeToString(raw)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) { // lost a race with another starter
			return ResolveSecretKey(dataDir)
		}
		return "", "", fmt.Errorf("config: create %s: %w", path, err)
	}
	if _, err := f.WriteString(key + "\n"); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("config: write %s: %w", path, err)
	}
	log.Printf("[config] generated a new secret key at %s", path)
	return key, "generated", nil
}

// ValidAbsPath reports whether p is a clean absolute path (no "..", no
// trailing slash) and, when roots are given, equal to or under one of them.
func ValidAbsPath(p string, roots ...string) bool {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	if len(roots) == 0 {
		return true
	}
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimRight(r, "/")+"/") {
			return true
		}
	}
	return false
}

// ValidPublicURL checks that s is an http(s) origin (host required, no
// path/query/fragment) and returns it with any trailing slash trimmed.
func ValidPublicURL(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("must be an absolute http(s) URL such as https://dashcam.example.com")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("must not contain a path, query or credentials")
	}
	return s, nil
}

// ValidIssuerURL checks that s is an absolute http(s) URL without query or
// fragment (a path is normal, e.g. /realms/home).
func ValidIssuerURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("must not contain a query, fragment or credentials")
	}
	return nil
}

// ValidDevicePath reports whether p is a clean device node under /dev/dri/.
func ValidDevicePath(p string) bool {
	return ValidAbsPath(p, "/dev/dri") && p != "/dev/dri"
}

var cudaIndexPattern = regexp.MustCompile(`^\d+$`)

// ValidCUDAIndex reports whether s is a plain GPU index such as "0".
func ValidCUDAIndex(s string) bool { return cudaIndexPattern.MatchString(s) }

var timezoneOverride atomic.Pointer[time.Location]

// SetTimezone applies the "timezone" setting ("" clears it); returns an
// error if name is not a known zone.
func SetTimezone(name string) error {
	if name == "" {
		timezoneOverride.Store(nil)
		return nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return err
	}
	timezoneOverride.Store(l)
	return nil
}

// Location is the zone for interpreting the recorder's naive local
// timestamps: the timezone setting if set, else the TZ environment variable
// if valid, else time.Local. Resolved on every call (tests set TZ
// mid-suite).
func Location() *time.Location {
	if l := timezoneOverride.Load(); l != nil {
		return l
	}
	if tz := os.Getenv("TZ"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.Local
}

// splitList splits a comma/space separated list, trimming and dropping empty
// entries, optionally lower-casing (emails are matched case-insensitively).
func splitList(s string, lower bool) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if lower {
			f = strings.ToLower(f)
		}
		out = append(out, f)
	}
	return out
}

func scopesOrDefault(s []string) []string {
	if len(s) == 0 {
		return []string{"openid", "email", "profile"}
	}
	return s
}

var oidcEnvVars = []string{
	"DASHCAM_OIDC_ISSUER", "DASHCAM_OIDC_CLIENT_ID", "DASHCAM_OIDC_CLIENT_SECRET", "DASHCAM_OIDC_SCOPES",
	"DASHCAM_OIDC_ALLOWED_EMAILS", "DASHCAM_OIDC_ALLOWED_GROUPS", "DASHCAM_OIDC_GROUPS_CLAIM",
}

func anyEnvSet(keys []string) bool {
	for _, k := range keys {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[config] invalid %s=%q, using default %d: %v", key, v, def, err)
		return def
	}
	return n
}
