package config

import "testing"

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
		Workers:   "4",
		Addr:      ":8080",
	}
	if c != want {
		t.Fatalf("Load() defaults = %+v, want %+v", c, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DASHCAM_DB", "/tmp/x.db")
	t.Setenv("DASHCAM_ADDR", ":9090")

	c := Load()

	if c.DB != "/tmp/x.db" {
		t.Errorf("DB = %q, want /tmp/x.db", c.DB)
	}
	if c.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", c.Addr)
	}
}
