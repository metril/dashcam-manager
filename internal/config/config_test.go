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
	t.Setenv("DASHCAM_SSD_UUID", "")
	t.Setenv("DASHCAM_QSV_DEVICE", "")
	t.Setenv("DASHCAM_CUDA_DEVICE", "")
	t.Setenv("DASHCAM_SECRET_KEY", "")
	t.Setenv("DASHCAM_WORKERS", "")
	t.Setenv("DASHCAM_ADDR", "")
	t.Setenv("DASHCAM_LOCAL_ROOT", "")

	c := Load()

	want := Config{
		DB:         "/data/dashcam.db",
		Cache:      "/cache",
		SSDPath:    "/mnt/dashcam",
		SSDUUID:    "",
		QSVDevice:  "/dev/dri/renderD128",
		CUDADevice: "0",
		SecretKey:  "",
		Workers:    4,
		Addr:       ":8080",
		LocalRoot:  "/mnt/shares",
	}
	if c != want {
		t.Fatalf("Load() defaults = %+v, want %+v", c, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DASHCAM_DB", "/tmp/x.db")
	t.Setenv("DASHCAM_ADDR", ":9090")
	t.Setenv("DASHCAM_WORKERS", "8")
	t.Setenv("DASHCAM_LOCAL_ROOT", "/data/local")
	t.Setenv("DASHCAM_CUDA_DEVICE", "1")

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
	if c.LocalRoot != "/data/local" {
		t.Errorf("LocalRoot = %q, want /data/local", c.LocalRoot)
	}
	if c.CUDADevice != "1" {
		t.Errorf("CUDADevice = %q, want 1", c.CUDADevice)
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

func TestLoadInvalidSSDUUIDDisablesWatcher(t *testing.T) {
	t.Setenv("DASHCAM_SSD_UUID", "../../../etc/passwd")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := Load()

	if c.SSDUUID != "" {
		t.Errorf("SSDUUID = %q, want empty (invalid value should be rejected)", c.SSDUUID)
	}
	if !strings.Contains(buf.String(), "invalid DASHCAM_SSD_UUID") {
		t.Errorf("expected a warning log for invalid DASHCAM_SSD_UUID, got: %q", buf.String())
	}
}

func TestLoadValidSSDUUIDPasses(t *testing.T) {
	t.Setenv("DASHCAM_SSD_UUID", "F9B1-23CB")

	c := Load()

	if c.SSDUUID != "F9B1-23CB" {
		t.Errorf("SSDUUID = %q, want F9B1-23CB", c.SSDUUID)
	}
}

func TestValidSSDUUID(t *testing.T) {
	cases := map[string]bool{
		"F9B1-23CB":           true,
		"abcdef12-3456-7890":  true,
		"":                    false, // callers gate on non-empty separately before calling this at all
		"../../../etc/passwd": false,
		"foo/bar":             false,
		"foo bar":             false,
		"foo;rm -rf /":        false,
	}
	for in, want := range cases {
		if got := ValidSSDUUID(in); got != want {
			t.Errorf("ValidSSDUUID(%q) = %v, want %v", in, got, want)
		}
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
