package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/media"
	"dashcam/internal/share"
)

// Runtime is the immutable snapshot of every DB-backed setting the server
// acts on. PUT /api/settings (and a finished OIDC test login) builds a new
// one from the settings table and swaps it into rt; readers call getRT()
// per use instead of capturing values at startup, which is what makes
// settings apply without a restart.
type Runtime struct {
	SSDPath    string
	SSDUUID    string
	QSVDevice  string
	CUDADevice string
	LocalRoot  string
	FFmpeg     string
	FFprobe    string
	PublicURL  string
	Timezone   string
	Workers    int
	MaxFFmpeg  int
	HWMode     string

	// OIDC is the effective login config; meaningful only when OIDCEnabled.
	OIDC config.OIDCConfig
	// OIDCEnabled is the stored oidc_enabled flag, forced false when the
	// stored client secret could not be decrypted (OIDCSecretErr).
	OIDCEnabled bool
	// OIDCSecretSet reports whether a client secret is stored.
	OIDCSecretSet bool
	// OIDCSecretErr is non-empty when the stored client secret failed to
	// decrypt (e.g. the secret key changed); OIDC then stays disabled.
	OIDCSecretErr string
}

// OIDCConfig returns the config to hand the authenticator, nil if login is
// disabled.
func (r *Runtime) OIDCConfig() *config.OIDCConfig {
	if !r.OIDCEnabled {
		return nil
	}
	c := r.OIDC
	return &c
}

var rt atomic.Pointer[Runtime]

// getRT returns the current runtime, falling back to the env-derived
// defaults when none has been loaded yet (tests that skip startup).
func getRT() *Runtime {
	if r := rt.Load(); r != nil {
		return r
	}
	return runtimeFromSettings(nil, config.Load().Seed, "")
}

// currentDevices is the media.Devices view of getRT, evaluated per call so
// QSV/CUDA device changes apply to the next ffmpeg invocation.
func currentDevices() media.Devices {
	r := getRT()
	return media.Devices{DRI: r.QSVDevice, CUDA: r.CUDADevice}
}

// devicesFn is how handlers and job handlers obtain the live devices.
type devicesFn func() media.Devices

// loadRuntime builds a Runtime from the settings table, falling back to the
// env seed values for any key that is missing.
func loadRuntime(ctx context.Context, store *db.DB, secretKey string) (*Runtime, error) {
	all, err := store.SettingsGetAll(ctx)
	if err != nil {
		return nil, err
	}
	return runtimeFromSettings(all, config.Load().Seed, secretKey), nil
}

// refreshRuntime reloads rt from the DB, logging (and keeping the previous
// snapshot) on failure.
func refreshRuntime(ctx context.Context, store *db.DB, secretKey string) *Runtime {
	r, err := loadRuntime(ctx, store, secretKey)
	if err != nil {
		log.Printf("[api] load runtime settings: %v", err)
		return getRT()
	}
	rt.Store(r)
	return r
}

func runtimeFromSettings(all map[string]json.RawMessage, seed config.SeedValues, secretKey string) *Runtime {
	str := func(key, def string) string {
		var s string
		if raw, ok := all[key]; ok && json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s)
		}
		return def
	}
	num := func(key string, def int) int {
		var n int
		if raw, ok := all[key]; ok && json.Unmarshal(raw, &n) == nil && n > 0 {
			return n
		}
		return def
	}
	list := func(key string, def []string) []string {
		var l []string
		if raw, ok := all[key]; ok && json.Unmarshal(raw, &l) == nil {
			return l
		}
		return def
	}

	r := &Runtime{
		SSDPath:    str("ssd_path", seed.SSDPath),
		SSDUUID:    str("ssd_uuid", seed.SSDUUID),
		QSVDevice:  str("qsv_device", seed.QSVDevice),
		CUDADevice: str("cuda_device", seed.CUDADevice),
		LocalRoot:  str("local_root", seed.LocalRoot),
		FFmpeg:     str("ffmpeg_bin", seed.FFmpeg),
		FFprobe:    str("ffprobe_bin", seed.FFprobe),
		PublicURL:  strings.TrimRight(str("public_url", seed.PublicURL), "/"),
		Timezone:   str("timezone", ""),
		Workers:    num("workers", seed.Workers),
		MaxFFmpeg:  num("max_ffmpeg_jobs", defaultFFmpegConcurrency),
		HWMode:     str("hw_mode", "auto"),
	}
	if r.SSDUUID != "" && !config.ValidSSDUUID(r.SSDUUID) {
		log.Printf("[api] settings: ignoring invalid stored ssd_uuid %q", r.SSDUUID)
		r.SSDUUID = ""
	}

	r.OIDC = config.OIDCConfig{
		Issuer:        str("oidc_issuer", ""),
		ClientID:      str("oidc_client_id", ""),
		PublicURL:     r.PublicURL,
		Scopes:        list("oidc_scopes", []string{"openid", "email", "profile"}),
		AllowedEmails: list("oidc_allowed_emails", nil),
		AllowedGroups: list("oidc_allowed_groups", nil),
		GroupsClaim:   str("oidc_groups_claim", "groups"),
	}
	if len(r.OIDC.Scopes) == 0 {
		r.OIDC.Scopes = []string{"openid", "email", "profile"}
	}
	if r.OIDC.GroupsClaim == "" {
		r.OIDC.GroupsClaim = "groups"
	}
	enabled := false
	if raw, ok := all["oidc_enabled"]; ok {
		_ = json.Unmarshal(raw, &enabled)
	}
	if enc := str("oidc_client_secret_enc", ""); enc != "" {
		r.OIDCSecretSet = true
		secret, err := decryptSecret(secretKey, enc)
		if err != nil {
			r.OIDCSecretErr = err.Error()
			enabled = false
		} else {
			r.OIDC.ClientSecret = secret
		}
	}
	r.OIDCEnabled = enabled && r.OIDC.Enabled()
	return r
}

// encryptSecret/decryptSecret store a secret as base64 of share.Encrypt, so
// it lives in a JSON string setting ("..._enc"); these keys are never
// returned by the API.
func encryptSecret(secretKey, plain string) (string, error) {
	b, err := share.Encrypt(secretKey, plain)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func decryptSecret(secretKey, enc string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("stored OIDC client secret is not valid base64: %w", err)
	}
	s, err := share.Decrypt(secretKey, b)
	if err != nil {
		return "", fmt.Errorf("stored OIDC client secret cannot be decrypted with the current secret key: %w", err)
	}
	return s, nil
}

// applyResult reports what a settings change did.
type applyResult struct {
	Applied []string
	Restart []string
	Errors  map[string]string
}

// applier pushes a changed Runtime into the live subsystems. Every
// dependency is optional so tests can build one with only what they check.
type applier struct {
	store  *db.DB
	queue  *jobs.Queue
	authn  authReconfigurer
	ssd    *ssdController
	secret string

	mu      sync.Mutex
	restart map[string]bool // sticky: keys whose change needs a process restart
}

// authReconfigurer is the slice of *auth.Authenticator the applier drives.
type authReconfigurer interface {
	Configure(c *config.OIDCConfig)
	SetPublicURL(u string)
}

func newApplier(store *db.DB, queue *jobs.Queue, authn authReconfigurer, ssd *ssdController, secret string) *applier {
	return &applier{store: store, queue: queue, authn: authn, ssd: ssd, secret: secret, restart: map[string]bool{}}
}

// RestartRequired lists keys whose saved value only takes effect after a
// restart (an apply that could not complete live).
func (a *applier) RestartRequired() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for k := range a.restart {
		out = append(out, k)
	}
	return out
}

// Apply makes the changed keys take effect in the running process.
func (a *applier) Apply(ctx context.Context, old, cur *Runtime, changed map[string]bool) applyResult {
	res := applyResult{Applied: []string{}, Restart: []string{}, Errors: map[string]string{}}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if changed[k] {
				return true
			}
		}
		return false
	}
	ok := func(keys ...string) {
		for _, k := range keys {
			if changed[k] {
				res.Applied = append(res.Applied, k)
			}
		}
	}
	fail := func(err error, keys ...string) {
		for _, k := range keys {
			if !changed[k] {
				continue
			}
			res.Errors[k] = err.Error()
			if errorsIsRestart(err) {
				res.Restart = append(res.Restart, k)
				a.mu.Lock()
				a.restart[k] = true
				a.mu.Unlock()
			}
		}
	}

	if has("ssd_uuid", "ssd_path") && a.ssd != nil {
		if err := a.ssd.Apply(ctx, cur.SSDUUID, cur.SSDPath); err != nil {
			fail(err, "ssd_uuid", "ssd_path")
		} else {
			ok("ssd_uuid", "ssd_path")
			if changed["ssd_path"] && a.store != nil && a.queue != nil {
				a.rescanSSD(ctx, cur.SSDPath)
			}
		}
	} else {
		ok("ssd_uuid", "ssd_path")
	}
	if has("workers") {
		if a.queue != nil {
			a.queue.Resize(cur.Workers)
		}
		ok("workers")
	}
	if has("max_ffmpeg_jobs") {
		ffmpegLimiter.SetMax(cur.MaxFFmpeg)
		ok("max_ffmpeg_jobs")
	}
	if has("ffmpeg_bin", "ffprobe_bin") {
		media.SetBinaries(cur.FFmpeg, cur.FFprobe)
	}
	if has("ffmpeg_bin", "ffprobe_bin", "qsv_device", "cuda_device", "hw_mode") {
		if a.store != nil {
			scheduleReprobeHW(a.store, 500*time.Millisecond)
		}
		ok("ffmpeg_bin", "ffprobe_bin", "qsv_device", "cuda_device", "hw_mode")
	}
	if has("timezone") {
		if err := config.SetTimezone(cur.Timezone); err != nil {
			fail(err, "timezone")
		} else {
			ok("timezone")
		}
	}
	if has("public_url") && a.authn != nil {
		a.authn.SetPublicURL(cur.PublicURL)
	}
	if has("public_url", "oidc_enabled", "oidc_issuer", "oidc_client_id", "oidc_client_secret", "oidc_scopes",
		"oidc_allowed_emails", "oidc_allowed_groups", "oidc_groups_claim") {
		if a.authn != nil {
			a.authn.Configure(cur.OIDCConfig())
		}
		ok("public_url", "oidc_enabled", "oidc_issuer", "oidc_client_id", "oidc_client_secret", "oidc_scopes",
			"oidc_allowed_emails", "oidc_allowed_groups", "oidc_groups_claim")
	}
	// Everything else (local_root, dashcam_poll_interval_s, ...) is read per
	// use and needs no push.
	for k := range changed {
		if _, handled := livePushed[k]; !handled {
			res.Applied = append(res.Applied, k)
		}
	}
	return res
}

// livePushed are the keys Apply handles explicitly above; every other
// changed key is read per use and so counts as applied.
var livePushed = map[string]struct{}{
	"ssd_uuid": {}, "ssd_path": {}, "workers": {}, "max_ffmpeg_jobs": {}, "ffmpeg_bin": {}, "ffprobe_bin": {},
	"qsv_device": {}, "cuda_device": {}, "hw_mode": {}, "timezone": {}, "public_url": {}, "oidc_enabled": {},
	"oidc_issuer": {}, "oidc_client_id": {}, "oidc_client_secret": {}, "oidc_scopes": {},
	"oidc_allowed_emails": {}, "oidc_allowed_groups": {}, "oidc_groups_claim": {},
}

// rescanSSD enqueues a scan of the repointed SSD source if the new path is
// already mounted (legacy bind-mount setups). With a UUID the watcher
// mounts asynchronously, so the first scan is left to the operator.
func (a *applier) rescanSSD(ctx context.Context, path string) {
	if mounted, err := ssdMountedFn(path); err != nil || !mounted {
		return
	}
	src, err := findSSDSource(ctx, a.store)
	if err != nil || src == nil {
		return
	}
	if _, err := a.queue.Enqueue(ctx, "scan", scanJobParams{SourceID: src.ID}, 0); err != nil {
		log.Printf("[api] settings: enqueue rescan after ssd_path change: %v", err)
	}
}
