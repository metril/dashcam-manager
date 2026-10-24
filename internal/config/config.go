// Package config centralizes environment-derived configuration for the
// dashcam-manager API server, with sane defaults for local development.
package config

import (
	"log"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
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

// Config holds runtime configuration read from the environment.
type Config struct {
	// DB is the path to the SQLite database file.
	DB string
	// Cache is the directory used for thumbnails, sprites, HLS segments, etc.
	Cache string
	// SSDPath is the read-only mount point of the recorder's SSD.
	SSDPath string
	// SSDUUID is the filesystem UUID (as it appears under
	// /dev/disk/by-uuid) of the recorder's USB SSD, watched by
	// internal/ssd for in-container hotplug mount/unmount. Empty disables
	// the watcher (legacy behaviour — SSDPath is expected to already be
	// mounted by whatever put it there).
	SSDUUID string
	// QSVDevice is the DRI render node used for Intel Quick Sync ffmpeg ops.
	QSVDevice string
	// CUDADevice is the CUDA device index used for NVIDIA NVENC/NVDEC
	// ffmpeg ops (e.g. "0" for the first GPU).
	CUDADevice string
	// SecretKey is used to encrypt stored share credentials.
	SecretKey string
	// Workers is the number of concurrent background job workers.
	Workers int
	// Addr is the listen address for the HTTP server.
	Addr string
	// LocalRoot is the directory kind=local storages' paths must live
	// under (see POST/PATCH /api/storages' path validation) — bind-mounted
	// from ./shares in compose.yaml, the same tree CIFS shares auto-mount
	// under (share.DefaultMountBase).
	LocalRoot string
	// PublicURL is the externally visible base URL (scheme://host[:port],
	// no trailing slash) the app is reached at — behind a reverse proxy
	// this differs from the request's own Host. Used for the OIDC redirect
	// URI and as an extra accepted CSRF Origin. Only this configured value
	// is trusted; no X-Forwarded-* header is ever read.
	PublicURL string
	// OIDC configures the optional built-in OpenID Connect login; the zero
	// value (see OIDCConfig.Enabled) leaves the app unauthenticated.
	OIDC OIDCConfig
}

// OIDCConfig holds the DASHCAM_OIDC_* settings.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// PublicURL mirrors Config.PublicURL (set by Load) so the auth package
	// only needs this struct.
	PublicURL string
	// Scopes defaults to "openid email profile".
	Scopes []string
	// AllowedEmails (lower-cased) and AllowedGroups restrict who may sign
	// in; with both empty any user the IdP authenticates is allowed.
	AllowedEmails []string
	AllowedGroups []string
	// GroupsClaim is the ID token claim holding the user's groups.
	GroupsClaim string
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

	return Config{
		PublicURL: publicURL,
		OIDC: OIDCConfig{
			Issuer:        getEnv("DASHCAM_OIDC_ISSUER", ""),
			ClientID:      getEnv("DASHCAM_OIDC_CLIENT_ID", ""),
			ClientSecret:  getEnv("DASHCAM_OIDC_CLIENT_SECRET", ""),
			PublicURL:     publicURL,
			Scopes:        scopesOrDefault(splitList(os.Getenv("DASHCAM_OIDC_SCOPES"), false)),
			AllowedEmails: splitList(os.Getenv("DASHCAM_OIDC_ALLOWED_EMAILS"), true),
			AllowedGroups: splitList(os.Getenv("DASHCAM_OIDC_ALLOWED_GROUPS"), false),
			GroupsClaim:   getEnv("DASHCAM_OIDC_GROUPS_CLAIM", "groups"),
		},
		DB:         getEnv("DASHCAM_DB", "/data/dashcam.db"),
		Cache:      getEnv("DASHCAM_CACHE", "/cache"),
		SSDPath:    getEnv("DASHCAM_SSD_PATH", "/mnt/dashcam"),
		SSDUUID:    ssdUUID,
		QSVDevice:  getEnv("DASHCAM_QSV_DEVICE", "/dev/dri/renderD128"),
		CUDADevice: getEnv("DASHCAM_CUDA_DEVICE", "0"),
		SecretKey:  getEnv("DASHCAM_SECRET_KEY", ""),
		Workers:    getEnvInt("DASHCAM_WORKERS", defaultWorkers),
		Addr:       getEnv("DASHCAM_ADDR", ":8080"),
		LocalRoot:  getEnv("DASHCAM_LOCAL_ROOT", "/mnt/shares"),
	}
}

// Validate logs warnings for configuration that is incomplete but not fatal
// at boot. Call once after Load().
func (c Config) Validate() {
	if c.SecretKey == "" {
		log.Println("[config] DASHCAM_SECRET_KEY is empty — share credentials cannot be encrypted; set it before configuring SMB/CIFS shares")
	}
	if !c.OIDC.Enabled() {
		log.Println("[config] OIDC not configured — no authentication; LAN-trusted mode")
		return
	}
	// The redirect URI and the CSRF origin allow-list are both derived from
	// PublicURL, so a missing/malformed one can never work: refuse to start
	// rather than boot into a login loop.
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Fatalf("[config] OIDC is enabled but DASHCAM_PUBLIC_URL=%q is not an absolute http(s) URL", c.PublicURL)
	}
	if c.OIDC.ClientSecret == "" {
		log.Println("[config] DASHCAM_OIDC_CLIENT_SECRET is empty — using a public OIDC client (PKCE only)")
	}
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
