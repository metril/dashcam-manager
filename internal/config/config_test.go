package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"DASHCAM_DB", "DASHCAM_CACHE", "DASHCAM_SSD_PATH", "DASHCAM_SSD_UUID", "DASHCAM_QSV_DEVICE",
		"DASHCAM_CUDA_DEVICE", "DASHCAM_SECRET_KEY", "DASHCAM_WORKERS", "DASHCAM_ADDR", "DASHCAM_LOCAL_ROOT",
		"DASHCAM_FFMPEG", "DASHCAM_FFPROBE", "DASHCAM_AUTH_DISABLE"} {
		t.Setenv(k, "")
	}
	for _, k := range oidcEnvKeys {
		t.Setenv(k, "")
	}

	c := Load()

	want := Config{
		DB:      "/data/dashcam.db",
		Cache:   "/cache",
		Addr:    ":8080",
		SeedSet: map[string]bool{},
		Seed: SeedValues{
			SSDPath:    "/mnt/dashcam",
			QSVDevice:  "/dev/dri/renderD128",
			CUDADevice: "0",
			Workers:    4,
			LocalRoot:  "/mnt/shares",
			FFmpeg:     "ffmpeg",
			FFprobe:    "ffprobe",
			OIDC: OIDCConfig{
				Scopes:      []string{"openid", "email", "profile"},
				GroupsClaim: "groups",
			},
		},
	}
	if !reflect.DeepEqual(c, want) {
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
	if c.Seed.Workers != 8 {
		t.Errorf("Workers = %d, want 8", c.Seed.Workers)
	}
	if c.Seed.LocalRoot != "/data/local" {
		t.Errorf("LocalRoot = %q, want /data/local", c.Seed.LocalRoot)
	}
	if c.Seed.CUDADevice != "1" {
		t.Errorf("CUDADevice = %q, want 1", c.Seed.CUDADevice)
	}
}

func TestLoadWorkersInvalidFallsBackToDefault(t *testing.T) {
	t.Setenv("DASHCAM_WORKERS", "not-a-number")

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := Load()

	if c.Seed.Workers != defaultWorkers {
		t.Errorf("Workers = %d, want default %d", c.Seed.Workers, defaultWorkers)
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

	if c.Seed.SSDUUID != "" {
		t.Errorf("SSDUUID = %q, want empty (invalid value should be rejected)", c.Seed.SSDUUID)
	}
	if !strings.Contains(buf.String(), "invalid DASHCAM_SSD_UUID") {
		t.Errorf("expected a warning log for invalid DASHCAM_SSD_UUID, got: %q", buf.String())
	}
}

func TestLoadValidSSDUUIDPasses(t *testing.T) {
	t.Setenv("DASHCAM_SSD_UUID", "F9B1-23CB")

	c := Load()

	if c.Seed.SSDUUID != "F9B1-23CB" {
		t.Errorf("SSDUUID = %q, want F9B1-23CB", c.Seed.SSDUUID)
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

func TestValidateWarnsOnAuthDisable(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	Config{AuthDisable: true}.Validate()
	if !strings.Contains(buf.String(), "DASHCAM_AUTH_DISABLE") {
		t.Errorf("expected a loud warning for AuthDisable, got: %q", buf.String())
	}
	buf.Reset()
	Config{}.Validate()
	if strings.Contains(buf.String(), "DASHCAM_AUTH_DISABLE") {
		t.Errorf("unexpected AuthDisable warning: %q", buf.String())
	}
}

func TestLoadAuthDisableAndSeedSet(t *testing.T) {
	t.Setenv("DASHCAM_AUTH_DISABLE", "1")
	t.Setenv("DASHCAM_SSD_PATH", "/mnt/x")
	t.Setenv("DASHCAM_FFMPEG", "")
	c := Load()
	if !c.AuthDisable {
		t.Error("AuthDisable not set")
	}
	if !c.SeedSet["ssd_path"] || c.SeedSet["ffmpeg_bin"] {
		t.Errorf("SeedSet = %v", c.SeedSet)
	}
}

func TestResolveSecretKey(t *testing.T) {
	t.Run("generated then reused with 0600", func(t *testing.T) {
		t.Setenv("DASHCAM_SECRET_KEY", "")
		dir := t.TempDir()
		k1, src, err := ResolveSecretKey(dir)
		if err != nil || src != "generated" || k1 == "" {
			t.Fatalf("first = %q, %q, %v", k1, src, err)
		}
		fi, err := os.Stat(filepath.Join(dir, "secret.key"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("secret.key stat = %v, %v", fi, err)
		}
		k2, src, err := ResolveSecretKey(dir)
		if err != nil || src != "file" || k2 != k1 {
			t.Fatalf("second = %q, %q, %v (want %q from file)", k2, src, err, k1)
		}
	})
	t.Run("env wins and is persisted to the file", func(t *testing.T) {
		t.Setenv("DASHCAM_SECRET_KEY", "from-env")
		dir := t.TempDir()
		k, src, err := ResolveSecretKey(dir)
		if err != nil || src != "env" || k != "from-env" {
			t.Fatalf("got %q, %q, %v", k, src, err)
		}
		fi, err := os.Stat(filepath.Join(dir, "secret.key"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("env key should be persisted to secret.key (0600): %v %v", fi, err)
		}
		// Removing the variable later keeps the same key.
		t.Setenv("DASHCAM_SECRET_KEY", "")
		if k2, src, err := ResolveSecretKey(dir); err != nil || src != "file" || k2 != "from-env" {
			t.Errorf("after unsetting env: %q, %q, %v", k2, src, err)
		}
	})
	t.Run("env vs file mismatch warns, env wins", func(t *testing.T) {
		t.Setenv("DASHCAM_SECRET_KEY", "from-env")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "secret.key"), []byte("other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })
		k, src, err := ResolveSecretKey(dir)
		if err != nil || src != "env" || k != "from-env" {
			t.Fatalf("got %q, %q, %v", k, src, err)
		}
		if !strings.Contains(buf.String(), "differs") {
			t.Errorf("expected mismatch warning, got %q", buf.String())
		}
	})
}

func TestValidators(t *testing.T) {
	for p, want := range map[string]bool{
		"/mnt/dashcam": true, "/mnt": true, "/mnt/a/b": true, "/mnt/": false, "/mnt/../etc": false,
		"mnt/x": false, "/mntx": false, "": false, "/etc/passwd": false,
	} {
		if got := ValidAbsPath(p, "/mnt"); got != want {
			t.Errorf("ValidAbsPath(%q, /mnt) = %v, want %v", p, got, want)
		}
	}
	if !ValidAbsPath("/usr/bin/ffmpeg") || ValidAbsPath("usr/bin") {
		t.Error("ValidAbsPath without roots")
	}
	for p, want := range map[string]bool{"/dev/dri/renderD128": true, "/dev/dri": false, "/dev/sda": false, "/dev/dri/../sda": false} {
		if got := ValidDevicePath(p); got != want {
			t.Errorf("ValidDevicePath(%q) = %v, want %v", p, got, want)
		}
	}
	for s, want := range map[string]bool{"0": true, "12": true, "": false, "a": false, "-1": false, "0 ": false} {
		if got := ValidCUDAIndex(s); got != want {
			t.Errorf("ValidCUDAIndex(%q) = %v, want %v", s, got, want)
		}
	}
	for in, want := range map[string]string{"https://a.example.com/": "https://a.example.com", "http://h:8090": "http://h:8090"} {
		if got, err := ValidPublicURL(in); err != nil || got != want {
			t.Errorf("ValidPublicURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "ftp://x", "https://", "https://x/path", "https://x?q=1", "https://u:p@x", "x.com"} {
		if _, err := ValidPublicURL(in); err == nil {
			t.Errorf("ValidPublicURL(%q) should fail", in)
		}
	}
	if ValidIssuerURL("https://idp.example.com/realms/home") != nil || ValidIssuerURL("idp") == nil || ValidIssuerURL("https://x/?a=1") == nil {
		t.Error("ValidIssuerURL")
	}
}

func TestLocationTimezoneSetting(t *testing.T) {
	t.Setenv("TZ", "UTC")
	t.Cleanup(func() { _ = SetTimezone("") })
	if err := SetTimezone("America/New_York"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	if got := Location().String(); got != "America/New_York" {
		t.Errorf("Location = %s, want America/New_York", got)
	}
	_ = SetTimezone("")
	if got := Location().String(); got != "UTC" {
		t.Errorf("Location = %s, want UTC from TZ", got)
	}
	if SetTimezone("Not/AZone") == nil {
		t.Error("expected error for bad zone")
	}
}

var oidcEnvKeys = []string{
	"DASHCAM_PUBLIC_URL", "DASHCAM_OIDC_ISSUER", "DASHCAM_OIDC_CLIENT_ID", "DASHCAM_OIDC_CLIENT_SECRET",
	"DASHCAM_OIDC_SCOPES", "DASHCAM_OIDC_ALLOWED_EMAILS", "DASHCAM_OIDC_ALLOWED_GROUPS", "DASHCAM_OIDC_GROUPS_CLAIM",
}

func TestLoadOIDCListsAndPublicURL(t *testing.T) {
	for _, k := range oidcEnvKeys {
		t.Setenv(k, "")
	}
	t.Setenv("DASHCAM_PUBLIC_URL", "https://dashcam.example.com/")
	t.Setenv("DASHCAM_OIDC_SCOPES", "openid, email  groups,,")
	t.Setenv("DASHCAM_OIDC_ALLOWED_EMAILS", " Alice@Example.com,bob@example.com ")
	t.Setenv("DASHCAM_OIDC_ALLOWED_GROUPS", "Admins, family")

	c := Load()
	if c.Seed.PublicURL != "https://dashcam.example.com" || c.Seed.OIDC.PublicURL != c.Seed.PublicURL {
		t.Errorf("PublicURL = %q / %q, want trailing slash stripped", c.Seed.PublicURL, c.Seed.OIDC.PublicURL)
	}
	if want := []string{"openid", "email", "groups"}; !reflect.DeepEqual(c.Seed.OIDC.Scopes, want) {
		t.Errorf("Scopes = %v, want %v", c.Seed.OIDC.Scopes, want)
	}
	if want := []string{"alice@example.com", "bob@example.com"}; !reflect.DeepEqual(c.Seed.OIDC.AllowedEmails, want) {
		t.Errorf("AllowedEmails = %v, want %v", c.Seed.OIDC.AllowedEmails, want)
	}
	if want := []string{"Admins", "family"}; !reflect.DeepEqual(c.Seed.OIDC.AllowedGroups, want) {
		t.Errorf("AllowedGroups = %v, want %v (case preserved)", c.Seed.OIDC.AllowedGroups, want)
	}
}

func TestOIDCEnabled(t *testing.T) {
	cases := []struct {
		issuer, client string
		want           bool
	}{{"", "", false}, {"https://idp", "", false}, {"", "id", false}, {"https://idp", "id", true}}
	for _, tc := range cases {
		if got := (OIDCConfig{Issuer: tc.issuer, ClientID: tc.client}).Enabled(); got != tc.want {
			t.Errorf("Enabled(%q,%q) = %v, want %v", tc.issuer, tc.client, got, tc.want)
		}
	}
}

func TestOIDCPartialConfigIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string // "" = ok
	}{
		{"nothing set", nil, ""},
		{"fully configured", map[string]string{"DASHCAM_OIDC_ISSUER": "https://idp", "DASHCAM_OIDC_CLIENT_ID": "c"}, ""},
		{"missing issuer", map[string]string{"DASHCAM_OIDC_CLIENT_ID": "c"}, "DASHCAM_OIDC_ISSUER"},
		{"missing client id", map[string]string{"DASHCAM_OIDC_ISSUER": "https://idp"}, "DASHCAM_OIDC_CLIENT_ID"},
		{"only allow-list", map[string]string{"DASHCAM_OIDC_ALLOWED_EMAILS": "a@x.com"}, "DASHCAM_OIDC_ISSUER"},
		{"only secret", map[string]string{"DASHCAM_OIDC_CLIENT_SECRET": "s"}, "DASHCAM_OIDC_ISSUER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range oidcEnvKeys {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			err := Load().Seed.OIDC.Partial()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Partial() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Partial() = %v, want error naming %s", err, tc.wantErr)
			}
		})
	}
}
