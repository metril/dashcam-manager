package auth

import (
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"dashcam/internal/db"
)

// Login starts the authorization-code + PKCE flow (GET /auth/login?next=).
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
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
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure,
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// Callback completes the flow (GET /auth/callback): validates state, PKCE,
// the ID token and its nonce, applies the allow-lists, then creates the
// session.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// The state cookie is consumed on every outcome.
	sc, _ := r.Cookie(loginStateCookie)
	http.SetCookie(w, &http.Cookie{Name: loginStateCookie, Path: "/auth/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure})

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
	prov, oc, err := a.provider()
	if err != nil {
		a.page(w, http.StatusServiceUnavailable, "Sign-in unavailable",
			"Identity provider unavailable, try again.", "retry")
		return
	}

	tok, err := oc.Exchange(oidc.ClientContext(r.Context(), a.http), q.Get("code"), oauth2.VerifierOption(st.PKCEVerifier))
	if err != nil {
		log.Printf("[auth] token exchange: %v", err)
		a.page(w, http.StatusBadGateway, "Sign-in failed", "Could not complete sign-in with the identity provider.", "retry")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		a.page(w, http.StatusBadGateway, "Sign-in failed", "The identity provider returned no ID token.", "retry")
		return
	}
	idt, err := prov.Verifier(&oidc.Config{ClientID: a.cfg.ClientID}).Verify(oidc.ClientContext(r.Context(), a.http), rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		if err != nil {
			log.Printf("[auth] id token verify: %v", err)
		}
		a.page(w, http.StatusUnauthorized, "Sign-in failed", "The ID token could not be verified.", "retry")
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		a.page(w, http.StatusBadGateway, "Sign-in failed", "The ID token claims could not be read.", "retry")
		return
	}

	email, name, ok := Authorize(a.cfg, claims)
	if !ok {
		a.page(w, http.StatusForbidden, "Access denied",
			"Your account is not allowed to use this app. Sign out of the identity provider or try another account.", "signout")
		return
	}
	verified := true
	if v, present := claims["email_verified"].(bool); present {
		verified = v
	}
	groups, _ := json.Marshal(Groups(a.cfg, claims))

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
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure,
	})
	http.Redirect(w, r, st.NextPath, http.StatusFound)
}

// Logout ends the session (POST /auth/logout, JSON, behind CSRF) and tells
// the SPA where to go: the IdP's end_session_endpoint when advertised, else /.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	redirect := "/"
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		hash := hashToken(c.Value)
		s, found, _ := a.store.AuthSessionGet(r.Context(), hash, a.now())
		if err := a.store.AuthSessionDelete(r.Context(), hash); err != nil {
			log.Printf("[auth] %v", err)
		}
		a.mu.Lock()
		prov := a.prov // cached only: logout never waits on discovery
		a.mu.Unlock()
		if prov != nil {
			var meta struct {
				EndSession string `json:"end_session_endpoint"`
			}
			if prov.Claims(&meta) == nil && meta.EndSession != "" {
				if u, err := url.Parse(meta.EndSession); err == nil {
					q := u.Query()
					if found && s.IDToken != "" {
						q.Set("id_token_hint", s.IDToken)
					}
					q.Set("client_id", a.cfg.ClientID)
					q.Set("post_logout_redirect_uri", a.cfg.PublicURL+"/")
					u.RawQuery = q.Encode()
					redirect = u.String()
				}
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": redirect})
}

// Me serves GET /api/me; nil-safe so it can be registered when auth is off.
func (a *Authenticator) Me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if a == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "user": nil})
		return
	}
	s, ok := a.SessionFrom(r)
	if !ok { // Gate guarantees a session; this is defensive
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": true,
		"user":    map[string]string{"email": s.Email, "name": s.Name},
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
{{if eq .Action "retry"}}<a href="/auth/login">Try again</a>{{end}}
{{if eq .Action "signout"}}<button id="so" type="button">Sign out</button>
<script>document.getElementById('so').onclick=async()=>{let r='/';try{const x=await fetch('/auth/logout',{method:'POST',headers:{'Content-Type':'application/json'}});r=(await x.json()).redirect||'/'}catch(e){}location.assign(r)}</script>{{end}}
</main></body></html>`))

// page renders a small standalone HTML status page; action is "retry"
// (link back to /auth/login) or "signout".
func (a *Authenticator) page(w http.ResponseWriter, status int, title, msg, action string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, map[string]string{"Title": title, "Msg": msg, "Action": action})
}
