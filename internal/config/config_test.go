package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DASHCAM_DB", "")
	t.Setenv("DASHCAM_CACHE", "")
	t.Setenv("DASHCAM_SSD_PATH", "")
	t.Setenv("DASHCAM_QSV_DEVICE", "")
	t.Setenv("DASHCAM_SECRET_KEY", "")
	t.Setenv("DASHCAM_WORKERS", "")
	t.Setenv("DASHCAM_ADDR", "")

	c := Load()

	want := Config{
		DB:        "/data/dashcam.db",
		Cache:     "/cache",
		SSDPath:   "/mnt/dashcam",
		QSVDevice: "/dev/dri/renderD128",
		SecretKey: "",
		Workers:   4,
		Addr:      ":8080",
	}
	if c != want {
		t.Fatalf("Load() defaults = %+v, want %+v", c, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DASHCAM_DB", "/tmp/x.db")
	t.Setenv("DASHCAM_ADDR", ":9090")
	t.Setenv("DASHCAM_WORKERS", "8")

	c := Load()

	if c.DB != "/tmp/x.db" {
		t.Errorf("DB = %q, want /tmp/x.db", c.DB)
	}
	if c.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", c.Addr)
	}
	if c.Workers != 8 {
		t.Errorf("Workers = %d, want 8", c.Workers)
	}
}

func TestLoadWorkersInvalidFallsBackToDefault(t *testing.T) {
	t.Setenv("DASHCAM_WORKERS", "not-a-number")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := Load()

	if c.Workers != defaultWorkers {
		t.Errorf("Workers = %d, want default %d", c.Workers, defaultWorkers)
	}
	if !strings.Contains(buf.String(), "invalid DASHCAM_WORKERS") {
		t.Errorf("expected a warning log for invalid DASHCAM_WORKERS, got: %q", buf.String())
	}
}

func TestValidateWarnsOnEmptySecretKey(t *testing.T) {
	c := Config{SecretKey: ""}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c.Validate()

	if !strings.Contains(buf.String(), "DASHCAM_SECRET_KEY is empty") {
		t.Errorf("expected a warning log for empty SecretKey, got: %q", buf.String())
	}
}

func TestValidateSilentWhenSecretKeySet(t *testing.T) {
	c := Config{SecretKey: "some-key"}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c.Validate()

	if buf.Len() != 0 {
		t.Errorf("expected no log output when SecretKey is set, got: %q", buf.String())
	}
}
