// Package config centralizes environment-derived configuration for the
// dashcam-manager API server, with sane defaults for local development.
package config

import (
	"log"
	"os"
	"strconv"
)

// defaultWorkers is used both as the DASHCAM_WORKERS fallback and when the
// value present is not a valid integer.
const defaultWorkers = 4

// Config holds runtime configuration read from the environment.
type Config struct {
	// DB is the path to the SQLite database file.
	DB string
	// Cache is the directory used for thumbnails, sprites, HLS segments, etc.
	Cache string
	// SSDPath is the read-only mount point of the recorder's SSD.
	SSDPath string
	// QSVDevice is the DRI render node used for Intel Quick Sync ffmpeg ops.
	QSVDevice string
	// SecretKey is used to encrypt stored share credentials.
	SecretKey string
	// Workers is the number of concurrent background job workers.
	Workers int
	// Addr is the listen address for the HTTP server.
	Addr string
}

// Load reads configuration from the environment, applying defaults for any
// variable that is unset, empty, or (for Workers) not a valid integer.
func Load() Config {
	return Config{
		DB:        getEnv("DASHCAM_DB", "/data/dashcam.db"),
		Cache:     getEnv("DASHCAM_CACHE", "/cache"),
		SSDPath:   getEnv("DASHCAM_SSD_PATH", "/mnt/dashcam"),
		QSVDevice: getEnv("DASHCAM_QSV_DEVICE", "/dev/dri/renderD128"),
		SecretKey: getEnv("DASHCAM_SECRET_KEY", ""),
		Workers:   getEnvInt("DASHCAM_WORKERS", defaultWorkers),
		Addr:      getEnv("DASHCAM_ADDR", ":8080"),
	}
}

// Validate logs warnings for configuration that is incomplete but not fatal
// at boot. Call once after Load().
func (c Config) Validate() {
	if c.SecretKey == "" {
		// TODO(M9): once share credential storage lands, refuse to
		// create/update a share (POST/PUT /api/shares) when SecretKey is
		// empty instead of only warning here — an empty key would mean
		// stored SMB/CIFS passwords can't be encrypted.
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
