package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/share"
)

const (
	purposeTest   = "oidc_test"
	testLoginPath = "/auth/test-login"
	pendingTTL    = 10 * time.Minute
	discoverTTL   = 10 * time.Second
)

// PendingConfig is the candidate OIDC configuration held (client secret
// encrypted) between "Test and enable" in the settings UI and the test
// login finishing. Nothing in it is live until that login succeeds.
type PendingConfig struct {
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	ClientSecretEnc string   `json:"client_secret_enc"` // base64 share.Encrypt; "" = public client
	Scopes          []string `json:"scopes"`
	AllowedEmails   []string `json:"allowed_emails"`
	AllowedGroups   []string `json:"allowed_groups"`
	GroupsClaim     string   `json:"groups_claim"`
	PublicURL       string   `json:"public_url"`
}

// toConfig decrypts the client secret and returns the candidate as a live
// config value.
func (p PendingConfig) toConfig(secretKey string) (config.OIDCConfig, error) {
	c := config.OIDCConfig{
		Issuer: p.Issuer, ClientID: p.ClientID, PublicURL: p.PublicURL, Scopes: p.Scopes,
		AllowedEmails: p.AllowedEmails, AllowedGroups: p.AllowedGroups, GroupsClaim: p.GroupsClaim,
	}
	if p.ClientSecretEnc != "" {
		raw, err := base64.StdEncoding.DecodeString(p.ClientSecretEnc)
		if err != nil {
			return c, err
		}
		if c.ClientSecret, err = share.Decrypt(secretKey, raw); err != nil {
			return c, err
		}
	}
	return c, nil
}

// StartTest stores p as the pending candidate (replacing any earlier one)
// for ten minutes; the browser then visits /auth/test-login.
func (a *Authenticator) StartTest(ctx context.Context, p PendingConfig) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	now := a.now()
	return a.store.OIDCPendingPut(ctx, string(b), now, now.Add(pendingTTL))
}

// commitPending writes the verified candidate to the settings table in one
// transaction and enables OIDC.
func (a *Authenticator) commitPending(ctx context.Context, p PendingConfig) error {
	vals := map[string]any{
		"oidc_issuer": p.Issuer, "oidc_client_id": p.ClientID, "oidc_scopes": p.Scopes,
		"oidc_allowed_emails": p.AllowedEmails, "oidc_allowed_groups": p.AllowedGroups,
		"oidc_groups_claim": p.GroupsClaim, "oidc_client_secret_enc": p.ClientSecretEnc,
		"public_url": p.PublicURL, "oidc_enabled": true,
	}
	raws := make(map[string]json.RawMessage, len(vals))
	for k, v := range vals {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		raws[k] = b
	}
	return a.store.SettingsSetMany(ctx, raws)
}

// discover runs provider discovery for cfg without touching the cache.
func (a *Authenticator) discover(cfg config.OIDCConfig) (*oidc.Provider, *oauth2.Config, error) {
	ctx, cancel := context.WithTimeout(a.baseCtx, discoverTTL)
	defer cancel()
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		log.Printf("[auth] OIDC discovery failed: %v", err)
		return nil, nil, errProviderUnavailable
	}
	return p, oauthConfig(cfg, p), nil
}

// TestLogin starts the test login (GET /auth/test-login): like Login but
// against the pending candidate config, with state purpose oidc_test.
func (a *Authenticator) TestLogin(w http.ResponseWriter, r *http.Request) {
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	if err1 != nil || err2 != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cfgJSON, ok, err := a.store.OIDCPendingBind(r.Context(), hashToken(state), a.now())
	if err != nil {
		log.Printf("[auth] %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var pend PendingConfig
	if ok {
		err = json.Unmarshal([]byte(cfgJSON), &pend)
	}
	if !ok || err != nil {
		a.page(w, http.StatusBadRequest, "Test expired", "There is no sign-in test in progress, or it timed out. Start it again from settings.", "settings")
		return
	}
	cfg, err := pend.toConfig(a.secretKey)
	if err != nil {
		log.Printf("[auth] pending config: %v", err)
		a.page(w, http.StatusInternalServerError, "Test failed", "The saved test configuration could not be read.", "settings")
		return
	}
	_, oc, err := a.discover(cfg)
	if err != nil {
		a.page(w, http.StatusServiceUnavailable, "Test failed", "Identity provider unavailable. Check the issuer URL and try again.", "settings")
		return
	}
	verifier := oauth2.GenerateVerifier()
	st := db.AuthLoginState{
		StateHash: hashToken(state), Nonce: nonce, PKCEVerifier: verifier, NextPath: "/settings",
		ExpiresAt: a.now().Add(loginStateTTL), Purpose: purposeTest,
	}
	if err := a.store.AuthLoginStatePut(r.Context(), st); err != nil {
		log.Printf("[auth] %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginStateCookie, Value: state, Path: "/auth/", MaxAge: int(loginStateTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(cfg.PublicURL, "https://"),
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// DiscoverResult is what POST /api/settings/oidc/discover reports.
type DiscoverResult struct {
	Issuer                string
	AuthorizationEndpoint string
	EndSession            bool
}

// Discover runs OIDC discovery against issuer with a 10 second timeout.
func Discover(ctx context.Context, issuer string) (DiscoverResult, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTTL)
	defer cancel()
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return DiscoverResult{}, err
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err != nil {
		return DiscoverResult{}, fmt.Errorf("read discovery document: %w", err)
	}
	return DiscoverResult{Issuer: issuer, AuthorizationEndpoint: p.Endpoint().AuthURL, EndSession: meta.EndSession != ""}, nil
}
