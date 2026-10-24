// csrf.go: lightweight CSRF hardening for dashcam-manager's mutating API
// routes.
//
// dashcam-manager has no authentication by default (see README's "LAN-trusted"
// note), and with the optional OIDC login (internal/auth) the session cookie
// is SameSite=Lax — either way the only real defense against a malicious
// page on another origin driving a browser to make requests against this
// server is the browser's own same-origin machinery. Two checks give that
// some teeth without adding a token dance:
//
//  1. Require Content-Type: application/json on every mutating request.
//     A plain HTML <form> POST (the classic drive-by CSRF vector) can only
//     ever send application/x-www-form-urlencoded, multipart/form-data, or
//     text/plain — never application/json — so this alone rules out the
//     simplest cross-site attacks. None of dashcam-manager's mutating
//     endpoints accept file uploads or any other non-JSON body, so there's
//     no legitimate exception to carve out.
//  2. When an Origin header is present (browsers attach it to
//     cross-origin fetch/XHR and to most same-origin ones too), it must
//     match the request's own Host — refusing a cross-origin fetch even if
//     it somehow got the Content-Type right (e.g. via a
//     CORS-preflight-exempt content type, which application/json is not,
//     but this is cheap insurance against future changes weakening check
//     1). A request with no Origin header (same-origin navigations,
//     curl, server-to-server) is allowed through — Origin's absence isn't
//     itself suspicious for a non-CORS same-origin app. Behind a reverse
//     proxy Host is the internal upstream (e.g. app:8080) while the browser
//     sends the public origin, so when the public URL setting is set its
//     scheme://host is accepted too. Only that configured value is trusted;
//     no X-Forwarded-* header is read.
package main

import (
	"net/http"
	"net/url"
	"strings"
)

// csrfProtect wraps next, rejecting mutating (non-GET/HEAD/OPTIONS)
// requests that don't look same-origin, per the package doc above.
// publicURL returns the current public_url setting ("" if unset), an
// additional accepted origin; it is evaluated per request so editing the
// setting takes effect immediately.
func csrfProtect(publicURL func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return csrfHandler(next, publicURL)
	}
}

func csrfHandler(next http.Handler, publicURL func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		ct := r.Header.Get("Content-Type")
		if mediaType(ct) != "application/json" {
			writeJSONError(w, http.StatusUnsupportedMediaType,
				"Content-Type must be application/json for "+r.Method+" requests")
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) && !matchesPublicOrigin(origin, publicURL()) {
			writeJSONError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// matchesPublicOrigin reports whether origin is the configured public URL's
// origin (false when none is configured).
func matchesPublicOrigin(origin, publicURL string) bool {
	po := normalizeOrigin(publicURL)
	return po != "" && normalizeOrigin(origin) == po
}

// mediaType strips any "; charset=..." (or other) parameters and
// lower-cases the result, so "application/json; charset=utf-8" still
// matches.
func mediaType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// sameOrigin reports whether origin's host (scheme stripped) matches host
// (an http.Request.Host value, i.e. no scheme, may include :port).
func sameOrigin(origin, host string) bool {
	// Origin is "scheme://host[:port]" (RFC 6454) — strip the scheme.
	_, rest, ok := strings.Cut(origin, "://")
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(rest), host)
}

// normalizeOrigin lower-cases raw's "scheme://host[:port]" and drops a
// default port (80/http, 443/https), or returns "" if raw isn't an
// absolute http(s) URL.
func normalizeOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port == "" || (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		return u.Scheme + "://" + host
	}
	return u.Scheme + "://" + host + ":" + port
}
