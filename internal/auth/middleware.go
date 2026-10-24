package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"dashcam/internal/db"
)

type ctxKey struct{}

// Gate refuses requests without a valid, still-authorized session. A nil
// Authenticator (auth disabled) returns next unchanged. Always public:
// /api/health (container healthchecks), /auth/* and any real static asset
// per isPublicAsset (so the sign-in pages can load); index.html is not.
//
// The session cookie is same-origin so it rides along on <video>, hls.js
// XHR, EventSource, <img> and downloads; http.ServeContent is untouched, so
// Range still works. SSE is only checked when it connects.
func (a *Authenticator) Gate(next http.Handler, isPublicAsset func(path string) bool) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api/health" || strings.HasPrefix(p, "/auth/") || isPublicAsset(p) {
			next.ServeHTTP(w, r)
			return
		}
		if s, ok := a.lookup(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
			return
		}

		switch {
		case strings.HasPrefix(p, "/api/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(SafeNext(r.URL.RequestURI())), http.StatusFound)
		default:
			http.Error(w, "authentication required", http.StatusUnauthorized)
		}
	})
}

// lookup resolves the request's session cookie and re-runs Authorize
// against the current allow-lists, deleting a session that no longer
// qualifies (e.g. the operator removed the user from DASHCAM_OIDC_ALLOWED_*).
func (a *Authenticator) lookup(r *http.Request) (db.AuthSession, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return db.AuthSession{}, false
	}
	hash := hashToken(c.Value)
	s, ok, err := a.store.AuthSessionGet(r.Context(), hash, a.now())
	if err != nil || !ok {
		return db.AuthSession{}, false
	}
	var groups []string
	_ = json.Unmarshal([]byte(s.GroupsJSON), &groups)
	claims := map[string]any{"email": s.Email, "email_verified": s.EmailVerified, "name": s.Name}
	gs := make([]any, len(groups))
	for i, g := range groups {
		gs[i] = g
	}
	claims[a.cfg.GroupsClaim] = gs
	if _, _, ok := Authorize(a.cfg, claims); !ok {
		_ = a.store.AuthSessionDelete(r.Context(), hash)
		return db.AuthSession{}, false
	}
	return s, true
}

// SessionFrom returns the session Gate attached to the request.
func (a *Authenticator) SessionFrom(r *http.Request) (db.AuthSession, bool) {
	s, ok := r.Context().Value(ctxKey{}).(db.AuthSession)
	return s, ok
}

// SafeNext reduces a post-login redirect target to a same-origin path+query,
// or "/" if it could redirect anywhere else (open redirect) or loop back
// into /auth/.
func SafeNext(raw string) string {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsRune(raw, '\\') {
		return "/"
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	if u.Path == "/auth" || strings.HasPrefix(u.Path, "/auth/") {
		return "/"
	}
	out := u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}
