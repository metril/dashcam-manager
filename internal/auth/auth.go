// Package auth implements dashcam-manager's optional built-in OpenID
// Connect login: an authorization-code + PKCE flow against a configured
// IdP, a server-side session behind an HttpOnly cookie (internal/db's
// auth_sessions), and a Gate middleware that refuses unauthenticated
// requests. When DASHCAM_OIDC_* is unset none of this is wired in and the
// app stays LAN-trusted (see README's Security section).
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
// handlers and the Gate middleware. A nil *Authenticator means auth is
// disabled (Gate and Me are nil-safe).
type Authenticator struct {
	cfg    config.OIDCConfig
	store  *db.DB
	secure bool // PublicURL is https: sets the cookie Secure flag
	http   *http.Client

	mu      sync.Mutex
	prov    *oidc.Provider
	lastTry time.Time
	retry   time.Duration
	now     func() time.Time
	baseCtx context.Context // carries http; outlives requests (JWKS refreshes reuse it)
}

// New builds an Authenticator for an enabled cfg. It does no network I/O:
// discovery happens lazily (or via Warm) so an unreachable IdP never stops
// the app from booting.
func New(cfg config.OIDCConfig, store *db.DB) *Authenticator {
	hc := &http.Client{Timeout: 10 * time.Second}
	return &Authenticator{
		cfg:     cfg,
		store:   store,
		secure:  strings.HasPrefix(cfg.PublicURL, "https://"),
		http:    hc,
		retry:   discoveryRetry,
		now:     time.Now,
		baseCtx: oidc.ClientContext(context.Background(), hc),
	}
}

// provider returns the discovered provider and OAuth2 config, discovering on
// first use and caching on success. After a failure it refuses to retry for
// a.retry so a down IdP isn't hammered by every request.
func (a *Authenticator) provider() (*oidc.Provider, *oauth2.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prov == nil {
		if now := a.now(); !a.lastTry.IsZero() && now.Sub(a.lastTry) < a.retry {
			return nil, nil, errProviderUnavailable
		}
		a.lastTry = a.now()
		p, err := oidc.NewProvider(a.baseCtx, a.cfg.Issuer)
		if err != nil {
			log.Printf("[auth] OIDC discovery failed: %v", err)
			return nil, nil, errProviderUnavailable
		}
		a.prov = p
	}
	return a.prov, &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		Endpoint:     a.prov.Endpoint(),
		RedirectURL:  a.cfg.PublicURL + "/auth/callback",
		Scopes:       a.cfg.Scopes,
	}, nil
}

// Warm retries discovery in the background (5s doubling to 5m) until it
// succeeds, so the first login doesn't pay for it. Returns when ctx is done.
func (a *Authenticator) Warm(ctx context.Context) {
	delay := discoveryRetry
	for {
		if _, _, err := a.provider(); err == nil {
			log.Printf("[auth] OIDC provider ready (%s)", a.cfg.Issuer)
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
