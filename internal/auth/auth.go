// Package auth implements dashcam-manager's optional built-in OpenID
// Connect login: an authorization-code + PKCE flow against a configured
// IdP, a server-side session behind an HttpOnly cookie (internal/db's
// auth_sessions), and a Gate middleware that refuses unauthenticated
// requests. OIDC is configured in the web UI (settings oidc_*) and enabled
// only after a successful test login; while disabled the app stays
// LAN-trusted (see README's Security section).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"dashcam/internal/config"
	"dashcam/internal/db"
)

const (
	sessionCookie    = "dcm_session"
	loginStateCookie = "dcm_login_state"
	sessionTTL       = 7 * 24 * time.Hour
	loginStateTTL    = 10 * time.Minute
	discoveryRetry   = 5 * time.Second
)

var errProviderUnavailable = errors.New("auth: identity provider unavailable")

// Authenticator owns the OIDC provider handle and serves the /auth/*
// handlers and the Gate middleware. It is always constructed; whether login
// is on is the live config (Configure): nil means disabled and Gate lets
// every request through. Safe for concurrent use while Configure swaps the
// config under it.
type Authenticator struct {
	store       *db.DB
	secretKey   string // encrypts the pending OIDC client secret
	authDisable bool   // DASHCAM_AUTH_DISABLE: Gate passes everything
	http        *http.Client

	cfg       atomic.Pointer[config.OIDCConfig]
	publicURL atomic.Pointer[string] // cookie Secure flag while OIDC is off
	onCommit  atomic.Pointer[func(context.Context)]
	broken    atomic.Bool // config unusable (e.g. client secret undecryptable): fail closed

	mu         sync.Mutex
	prov       *oidc.Provider
	provIssuer string
	lastTry    time.Time
	retry      time.Duration
	now        func() time.Time
	baseCtx    context.Context // carries http; outlives requests (JWKS refreshes reuse it)
}

// New builds an Authenticator with login disabled; call Configure to
// enable it. It does no network I/O: discovery happens lazily (or via Warm)
// so an unreachable IdP never stops the app from booting.
func New(store *db.DB, secretKey string, authDisable bool) *Authenticator {
	hc := &http.Client{Timeout: 10 * time.Second}
	return &Authenticator{
		store:       store,
		secretKey:   secretKey,
		authDisable: authDisable,
		http:        hc,
		retry:       discoveryRetry,
		now:         time.Now,
		baseCtx:     oidc.ClientContext(context.Background(), hc),
	}
}

// Configure swaps the live OIDC config (nil disables login) and drops the
// cached provider so the next use rediscovers against the new issuer.
func (a *Authenticator) Configure(c *config.OIDCConfig) {
	var cp *config.OIDCConfig
	if c != nil {
		v := *c
		cp = &v
		a.SetPublicURL(v.PublicURL)
	}
	a.mu.Lock()
	a.cfg.Store(cp)
	a.broken.Store(false)
	a.prov, a.provIssuer, a.lastTry = nil, "", time.Time{}
	a.mu.Unlock()
}

// SetUnavailable marks the configured sign-in as unusable (its client secret
// could not be decrypted). The gate keeps enforcing, so requests stay
// refused, while /auth/login answers 503. Configure clears it.
func (a *Authenticator) SetUnavailable(on bool) { a.broken.Store(on) }

// SetPublicURL records the public URL for deriving the cookie Secure flag
// while OIDC is disabled (Configure keeps it current otherwise).
func (a *Authenticator) SetPublicURL(u string) { a.publicURL.Store(&u) }

// SetOnCommit registers fn, called after a test login commits new OIDC
// settings, so cmd/api can reload its runtime snapshot.
func (a *Authenticator) SetOnCommit(fn func(context.Context)) { a.onCommit.Store(&fn) }

// Enabled reports whether login is configured and not bypassed by
// DASHCAM_AUTH_DISABLE.
func (a *Authenticator) Enabled() bool { return a != nil && a.cfg.Load() != nil && !a.authDisable }

// OIDCConfigured reports whether OIDC is configured at all, even if
// DASHCAM_AUTH_DISABLE currently bypasses it.
func (a *Authenticator) OIDCConfigured() bool { return a != nil && a.cfg.Load() != nil }

// AuthDisabled reports whether DASHCAM_AUTH_DISABLE is set.
func (a *Authenticator) AuthDisabled() bool { return a != nil && a.authDisable }

// cur returns a copy of the live config (zero value when disabled).
func (a *Authenticator) cur() config.OIDCConfig {
	if c := a.cfg.Load(); c != nil {
		return *c
	}
	return config.OIDCConfig{}
}

// secure derives the cookie Secure flag from the current public URL.
func (a *Authenticator) secure() bool {
	if c := a.cfg.Load(); c != nil {
		return strings.HasPrefix(c.PublicURL, "https://")
	}
	if u := a.publicURL.Load(); u != nil {
		return strings.HasPrefix(*u, "https://")
	}
	return false
}

// provider returns the discovered provider and OAuth2 config for the live
// config, discovering on first use and caching on success. After a failure
// it refuses to retry for a.retry so a down IdP isn't hammered by every
// request.
func (a *Authenticator) provider() (*oidc.Provider, *oauth2.Config, error) {
	cfg := a.cfg.Load()
	if cfg == nil || a.broken.Load() {
		return nil, nil, errProviderUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prov == nil || a.provIssuer != cfg.Issuer {
		if now := a.now(); !a.lastTry.IsZero() && now.Sub(a.lastTry) < a.retry {
			return nil, nil, errProviderUnavailable
		}
		a.lastTry = a.now()
		p, err := oidc.NewProvider(a.baseCtx, cfg.Issuer)
		if err != nil {
			log.Printf("[auth] OIDC discovery failed: %v", err)
			return nil, nil, errProviderUnavailable
		}
		a.prov, a.provIssuer = p, cfg.Issuer
	}
	return a.prov, oauthConfig(*cfg, a.prov), nil
}

func oauthConfig(cfg config.OIDCConfig, p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  cfg.PublicURL + "/auth/callback",
		Scopes:       cfg.Scopes,
	}
}

// Warm retries discovery in the background (5s doubling to 5m) until it
// succeeds, so the first login doesn't pay for it. Returns when ctx is done.
func (a *Authenticator) Warm(ctx context.Context) {
	delay := discoveryRetry
	for {
		if a.cfg.Load() == nil {
			return // disabled; discovery happens lazily if it is enabled later
		}
		if _, _, err := a.provider(); err == nil {
			log.Printf("[auth] OIDC provider ready (%s)", a.cur().Issuer)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
	}
}

// RunCleanup purges expired sessions and login states every interval until
// ctx is done.
func (a *Authenticator) RunCleanup(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.store.AuthPurgeExpired(ctx, a.now()); err != nil {
				log.Printf("[auth] purge expired: %v", err)
			} else if n > 0 {
				log.Printf("[auth] purged %d expired session/login rows", n)
			}
		}
	}
}

// randomToken returns 32 random bytes, base64url-encoded.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is the hex sha256 stored in place of a cookie token / state.
func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
