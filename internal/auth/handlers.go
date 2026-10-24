package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"dashcam/internal/config"
	"dashcam/internal/db"
)

// Login starts the authorization-code + PKCE flow (GET /auth/login?next=).
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if !a.OIDCConfigured() {
		a.page(w, http.StatusNotFound, "Sign-in not enabled", "Sign-in is not configured.", "")
		return
	}
	_, oc, err := a.provider()
	if err != nil {
		a.page(w, http.StatusServiceUnavailable, "Sign-in unavailable",
			"Identity provider unavailable, try again.", "retry")
		return
	}
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	if err1 != nil || err2 != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	st := db.AuthLoginState{
		StateHash:    hashToken(state),
		Nonce:        nonce,
		PKCEVerifier: verifier,
		NextPath:     SafeNext(r.URL.Query().Get("next")),
		ExpiresAt:    a.now().Add(loginStateTTL),
	}
	if err := a.store.AuthLoginStatePut(r.Context(), st); err != nil {
		log.Printf("[auth] %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginStateCookie, Value: state, Path: "/auth/", MaxAge: int(loginStateTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure(),
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// Callback completes the flow (GET /auth/callback): validates state, PKCE,
// the ID token and its nonce, applies the allow-lists, then creates the
// session. A state recorded by /auth/test-login (purpose oidc_test) is
// verified against the pending candidate config instead, and only if that
// succeeds are the settings written and OIDC enabled.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// The state cookie is consumed on every outcome.
	sc, _ := r.Cookie(loginStateCookie)
	http.SetCookie(w, &http.Cookie{Name: loginStateCookie, Path: "/auth/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure()})
	if sc == nil && !a.OIDCConfigured() {
		a.page(w, http.StatusNotFound, "Not found", "There is no sign-in to complete.", "")
		return
	}

	if e := q.Get("error"); e != "" {
		msg := "The identity provider returned an error: " + e
		if d := q.Get("error_description"); d != "" {
			msg += " (" + d + ")"
		}
		a.page(w, http.StatusBadRequest, "Sign-in failed", msg, "retry")
		return
	}
	state := q.Get("state")
	// Login-CSRF guard: the state must match the one this browser was given.
	if sc == nil || state == "" || subtle.ConstantTimeCompare([]byte(sc.Value), []byte(state)) != 1 {
		a.page(w, http.StatusBadRequest, "Sign-in failed", "Login state mismatch. Start again.", "retry")
		return
	}
	st, ok, err := a.store.AuthLoginStateTake(r.Context(), hashToken(state), a.now())
	if err != nil {
		log.Printf("[auth] %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		a.page(w, http.StatusBadRequest, "Sign-in failed", "Login expired or already used. Start again.", "retry")
		return
	}

	test := st.Purpose == purposeTest
	action, failTitle := "retry", "Sign-in failed"
	var (
		cfg  config.OIDCConfig
		pend PendingConfig
		prov *oidc.Provider
		oc   *oauth2.Config
	)
	if test {
		action, failTitle = "settings", "Test failed"
		cfgJSON, ok, err := a.store.OIDCPendingTake(r.Context(), hashToken(state), a.now())
		if err == nil && ok {
			err = json.Unmarshal([]byte(cfgJSON), &pend)
		}
		if err != nil || !ok {
			a.page(w, http.StatusBadRequest, failTitle, "The test expired or was already used. Start the test again from settings.", action)
			return
		}
		if cfg, err = pend.toConfig(a.secretKey); err != nil {
			log.Printf("[auth] pending config: %v", err)
			a.page(w, http.StatusInternalServerError, failTitle, "The saved test configuration could not be read.", action)
			return
		}
		prov, oc, err = a.discover(cfg)
	} else {
		if !a.OIDCConfigured() {
			a.page(w, http.StatusNotFound, "Not found", "Sign-in is not enabled.", "")
			return
		}
		cfg = a.cur()
		prov, oc, err = a.provider()
	}
	if err != nil {
		a.page(w, http.StatusServiceUnavailable, "Sign-in unavailable",
			"Identity provider unavailable, try again.", action)
		return
	}

	tok, err := oc.Exchange(oidc.ClientContext(r.Context(), a.http), q.Get("code"), oauth2.VerifierOption(st.PKCEVerifier))
	if err != nil {
		log.Printf("[auth] token exchange: %v", err)
		a.page(w, http.StatusBadGateway, failTitle, "Could not complete sign-in with the identity provider.", action)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		a.page(w, http.StatusBadGateway, failTitle, "The identity provider returned no ID token.", action)
		return
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: cfg.ClientID}).Verify(oidc.ClientContext(r.Context(), a.http), rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		msg := "The ID token could not be verified."
		if err != nil {
			msg = verifyFailMessage(err)
			log.Printf("[auth] id token verify: %v: %s", err, msg)
		}
		a.page(w, http.StatusUnauthorized, failTitle, msg, action)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		a.page(w, http.StatusBadGateway, failTitle, "The ID token claims could not be read.", action)
		return
	}

	a.mergeUserInfo(r.Context(), prov, tok, cfg, idt.Subject, claims)

	email, name, ok := Authorize(cfg, claims)
	if !ok {
		detail := denyDetail(cfg, idt.Subject, claims)
		if test {
			a.pageDetail(w, http.StatusForbidden, failTitle,
				"Your account is not allowed by the allowed emails and groups you entered, so nothing was changed. Add your account, or clear both lists, then test again.", action, detail)
			return
		}
		a.pageDetail(w, http.StatusForbidden, "Access denied",
			"Your account is not allowed to use this app. Sign out of the identity provider or try another account.", "signout", detail)
		return
	}
	if test {
		if err := a.commitPending(r.Context(), pend); err != nil {
			log.Printf("[auth] commit oidc settings: %v", err)
			a.page(w, http.StatusInternalServerError, failTitle, "The settings could not be saved.", action)
			return
		}
		a.Configure(&cfg)
		if fn := a.onCommit.Load(); fn != nil {
			(*fn)(r.Context())
		}
	}
	verified := true
	if v, present := claims["email_verified"].(bool); present {
		verified = v
	}
	groups, _ := json.Marshal(Groups(cfg, claims))

	token, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := a.now()
	err = a.store.AuthSessionCreate(r.Context(), db.AuthSession{
		TokenHash: hashToken(token), Subject: idt.Subject, Email: email, Name: name,
		EmailVerified: verified, GroupsJSON: string(groups), IDToken: rawID,
		CreatedAt: now, ExpiresAt: now.Add(sessionTTL), // absolute, never extended
	})
	if err != nil {
		log.Printf("[auth] %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: now.Add(sessionTTL),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(cfg.PublicURL, "https://"),
	})
	next := st.NextPath
	if test {
		next = "/settings?oidc=ok"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// Logout ends the session (POST /auth/logout, JSON, behind CSRF) and tells
// the SPA where to go: the IdP's end_session_endpoint when advertised, else /.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	redirect := "/"
	var idToken string
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		hash := hashToken(c.Value)
		if s, found, _ := a.store.AuthSessionGet(r.Context(), hash, a.now()); found {
			idToken = s.IDToken
		}
		if err := a.store.AuthSessionDelete(r.Context(), hash); err != nil {
			log.Printf("[auth] %v", err)
		}
	}
	// Built even without a session: a user denied by the allow-lists has no
	// dcm_session, but still needs the IdP session ended or signing in again
	// would silently re-deny them.
	cfg := a.cur()
	a.mu.Lock()
	prov := a.prov // cached only: logout never waits on discovery
	a.mu.Unlock()
	if prov != nil && a.OIDCConfigured() {
		var meta struct {
			EndSession string `json:"end_session_endpoint"`
		}
		if prov.Claims(&meta) == nil && meta.EndSession != "" {
			if u, err := url.Parse(meta.EndSession); err == nil {
				q := u.Query()
				if idToken != "" {
					q.Set("id_token_hint", idToken)
				}
				q.Set("client_id", cfg.ClientID)
				q.Set("post_logout_redirect_uri", cfg.PublicURL+"/")
				u.RawQuery = q.Encode()
				redirect = u.String()
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure()})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": redirect})
}

// Me serves GET /api/me. enabled is true when sign-in is enforced (OIDC on
// and not bypassed); auth_enabled is true when OIDC is configured at all,
// and auth_disabled_override when DASHCAM_AUTH_DISABLE bypasses it.
func (a *Authenticator) Me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !a.Enabled() {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enabled": false, "user": nil,
			"auth_enabled": a.OIDCConfigured(), "auth_disabled_override": a.AuthDisabled(),
		})
		return
	}
	s, ok := a.SessionFrom(r)
	if !ok { // Gate guarantees a session; this is defensive
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": true, "auth_enabled": true, "auth_disabled_override": false,
		"user": map[string]string{"email": s.Email, "name": s.Name},
	})
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light dark}
body{font:16px system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;padding:16px;box-sizing:border-box}
main{max-width:26rem}
h1{font-size:1.25rem;margin:0 0 .5rem}
a,button{font:inherit;color:inherit;border:1px solid currentColor;border-radius:.375rem;background:none;padding:.4rem .8rem;text-decoration:none;cursor:pointer;display:inline-block;margin-top:.75rem}
</style></head><body><main>
<h1>{{.Title}}</h1><p>{{.Msg}}</p>
{{if .Detail}}<p><small>{{.Detail}}</small></p>{{end}}
{{if eq .Action "retry"}}<a href="/auth/login">Try again</a>{{end}}
{{if eq .Action "settings"}}<a href="/settings">Back to settings</a>{{end}}
{{if eq .Action "signout"}}<button id="so" type="button">Sign out</button>
<script>document.getElementById('so').onclick=async()=>{let r='/';try{const x=await fetch('/auth/logout',{method:'POST',headers:{'Content-Type':'application/json'}});r=(await x.json()).redirect||'/'}catch(e){}location.assign(r)}</script>{{end}}
</main></body></html>`))

// page renders a small standalone HTML status page; action is "retry"
// (link back to /auth/login), "settings" (link back to /settings), "signout"
// or "" (no action).
func (a *Authenticator) page(w http.ResponseWriter, status int, title, msg, action string) {
	a.pageDetail(w, status, title, msg, action, "")
}

// pageDetail is page plus a small extra line (auto-escaped by the template).
func (a *Authenticator) pageDetail(w http.ResponseWriter, status int, title, msg, action, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, map[string]string{"Title": title, "Msg": msg, "Action": action, "Detail": detail})
}

// mergeUserInfo fills claims keys missing from the ID token (email, and the
// groups claim) from the userinfo endpoint. Many IdPs, Authentik included,
// leave them out of the ID token. The userinfo subject must match the ID
// token's, and failures are non-fatal: the ID token claims stand.
func (a *Authenticator) mergeUserInfo(ctx context.Context, prov *oidc.Provider, tok *oauth2.Token, cfg config.OIDCConfig, sub string, claims map[string]any) {
	_, hasEmail := claims["email"]
	_, hasGroups := claims[cfg.GroupsClaim]
	if hasEmail && hasGroups {
		return
	}
	var meta struct {
		UserInfoURL string `json:"userinfo_endpoint"`
	}
	if prov.Claims(&meta) != nil || meta.UserInfoURL == "" {
		return
	}
	ui, err := prov.UserInfo(oidc.ClientContext(ctx, a.http), oauth2.StaticTokenSource(tok))
	if err != nil {
		log.Printf("[auth] userinfo: %v", err)
		return
	}
	if ui.Subject != sub {
		log.Printf("[auth] userinfo subject does not match the ID token, ignoring userinfo")
		return
	}
	var extra map[string]any
	if err := ui.Claims(&extra); err != nil {
		log.Printf("[auth] userinfo claims: %v", err)
		return
	}
	if !hasEmail {
		// email and email_verified travel together: never pair an ID token's
		// verified flag with a userinfo email.
		delete(claims, "email_verified")
		for _, k := range []string{"email", "email_verified"} {
			if v, ok := extra[k]; ok {
				claims[k] = v
			}
		}
	}
	for _, k := range []string{"name", "preferred_username", cfg.GroupsClaim} {
		if v, ok := extra[k]; ok {
			if _, present := claims[k]; !present {
				claims[k] = v
			}
		}
	}
}

// denyDetail logs why Authorize refused and returns a short line for the
// error page. It never includes tokens.
func denyDetail(cfg config.OIDCConfig, sub string, claims map[string]any) string {
	email, _ := claims["email"].(string)
	verified := "absent"
	shown := "not sent"
	if v, present := claims["email_verified"].(bool); present {
		verified = strconv.FormatBool(v)
		shown = "verified"
		if !v {
			shown = "not verified"
		}
	}
	groups := Groups(cfg, claims)
	log.Printf("[auth] access denied: sub=%s email=%q email_verified=%s groups=%v (allowed: %d emails, %d groups)",
		sub, email, verified, groups, len(cfg.AllowedEmails), len(cfg.AllowedGroups))
	if email == "" {
		email = "none"
	}
	if verified == "absent" {
		shown = "verification not reported"
	}
	return fmt.Sprintf("The provider sent email: %s (%s); groups: %s.", email, shown, strings.Join(groups, ", "))
}

// verifyFailMessage explains an ID token verification failure; an
// unexpected signing algorithm (typically HS256 from an IdP provider with no
// signing key) gets an actionable hint.
func verifyFailMessage(err error) string {
	if strings.Contains(err.Error(), "signed with unsupported algorithm") {
		return "The ID token could not be verified: it was not signed with an expected algorithm; set a signing key (RS256) on the identity provider's OIDC provider settings."
	}
	return "The ID token could not be verified."
}
