// Package config centralizes environment-derived configuration for the
// dashcam-manager API server, with sane defaults for local development.
package config

import "os"

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
	Workers string
	// Addr is the listen address for the HTTP server.
	Addr string
}

// Load reads configuration from the environment, applying defaults for any
// variable that is unset or empty.
func Load() Config {
	return Config{
		DB:        getEnv("DASHCAM_DB", "/data/dashcam.db"),
		Cache:     getEnv("DASHCAM_CACHE", "/cache"),
		SSDPath:   getEnv("DASHCAM_SSD_PATH", "/mnt/dashcam"),
		QSVDevice: getEnv("DASHCAM_QSV_DEVICE", "/dev/dri/renderD128"),
		SecretKey: getEnv("DASHCAM_SECRET_KEY", ""),
		Workers:   getEnv("DASHCAM_WORKERS", "4"),
		Addr:      getEnv("DASHCAM_ADDR", ":8080"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
