// Package config centralizes environment-derived configuration for the
// dashcam-manager API server, with sane defaults for local development.
package config

import (
	"log"
	"os"
	"regexp"
	"strconv"
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
}

// Load reads configuration from the environment, applying defaults for any
// variable that is unset, empty, or (for Workers) not a valid integer.
func Load() Config {
	ssdUUID := getEnv("DASHCAM_SSD_UUID", "")
	if ssdUUID != "" && !ValidSSDUUID(ssdUUID) {
		log.Printf("[config] invalid DASHCAM_SSD_UUID=%q (must match %s); disabling the SSD hotplug watcher", ssdUUID, ssdUUIDPattern)
		ssdUUID = ""
	}

	return Config{
		DB:        getEnv("DASHCAM_DB", "/data/dashcam.db"),
		Cache:     getEnv("DASHCAM_CACHE", "/cache"),
		SSDPath:   getEnv("DASHCAM_SSD_PATH", "/mnt/dashcam"),
		SSDUUID:   ssdUUID,
		QSVDevice: getEnv("DASHCAM_QSV_DEVICE", "/dev/dri/renderD128"),
		SecretKey: getEnv("DASHCAM_SECRET_KEY", ""),
		Workers:   getEnvInt("DASHCAM_WORKERS", defaultWorkers),
		Addr:      getEnv("DASHCAM_ADDR", ":8080"),
		LocalRoot: getEnv("DASHCAM_LOCAL_ROOT", "/mnt/shares"),
	}
}

// Validate logs warnings for configuration that is incomplete but not fatal
// at boot. Call once after Load().
func (c Config) Validate() {
	if c.SecretKey == "" {
		log.Println("[config] DASHCAM_SECRET_KEY is empty — share credentials cannot be encrypted; set it before configuring SMB/CIFS shares")
	}
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
