package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
)

const (
	publicURL     = "http://dashcam.test"
	testSecretKey = "test-secret-key"
)

// env wires a fake IdP, a real SQLite store, an Authenticator and a mux
// behind Gate, and plays the browser.
type env struct {
	t     *testing.T
	idp   *fakeIdP
	store *db.DB
	a     *Authenticator
	h     http.Handler
	seq   int
}

func newEnv(t *testing.T, mutate func(*config.OIDCConfig)) *env {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "dashcam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	idp := newFakeIdP(t, publicURL+"/auth/callback")
	cfg := config.OIDCConfig{
		Issuer: idp.srv.URL, ClientID: testClientID, ClientSecret: testSecret, PublicURL: publicURL,
		Scopes: []string{"openid", "email", "profile"}, GroupsClaim: "groups",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a := New(store, testSecretKey, false)
	a.Configure(&cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", a.Login)
	mux.HandleFunc("GET /auth/test-login", a.TestLogin)
	mux.HandleFunc("GET /auth/callback", a.Callback)
	mux.HandleFunc("POST /auth/logout", a.Logout)
	mux.HandleFunc("GET /api/me", a.Me)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/trips", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[]")) })
	mux.HandleFunc("GET /assets/x.js", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("js")) })
	data := bytes.Repeat([]byte("0123456789"), 10)
	mux.HandleFunc("GET /api/media", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "m.mp4", time.Time{}, bytes.NewReader(data))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("shell")) })
	h := a.Gate(mux, func(p string) bool { return p == "/assets/x.js" })
	return &env{t: t, idp: idp, store: store, a: a, h: h}
}

func (e *env) do(method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "dashcam.test"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// flow is one in-progress login: what /auth/login handed the browser.
type flow struct {
	state, nonce, challenge string
	cookie                  *http.Cookie
}

func (e *env) start(next string) flow {
	e.t.Helper()
	rec := e.do("GET", "/auth/login?next="+url.QueryEscape(next))
	if rec.Code != http.StatusFound {
		e.t.Fatalf("login status = %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), e.idp.srv.URL+"/authorize") || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != e.a.cur().PublicURL+"/auth/callback" || q.Get("client_id") != testClientID {
		e.t.Fatalf("unexpected authorize URL %s", loc)
	}
	f := flow{state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	for _, c := range rec.Result().Cookies() {
		if c.Name == loginStateCookie {
			f.cookie = c
			if !c.HttpOnly || c.Path != "/auth/" || c.SameSite != http.SameSiteLaxMode {
				e.t.Errorf("login state cookie flags wrong: %+v", c)
			}
		}
	}
	if f.cookie == nil || f.cookie.Value != f.state {
		e.t.Fatal("missing login state cookie")
	}
	return f
}

// finish registers a code with the fake IdP and hits the callback.
func (e *env) finish(f flow, claims map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	e.seq++
	code := "code-" + string(rune('a'+e.seq))
	e.idp.addCode(code, f.challenge, f.nonce, claims, nil)
	return e.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(f.state), f.cookie)
}

func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

// login runs the full happy path and returns the session cookie.
func (e *env) login(claims map[string]any) *http.Cookie {
	e.t.Helper()
	rec := e.finish(e.start("/"), claims)
	c := sessionCookieOf(rec)
	if rec.Code != http.StatusFound || c == nil {
		e.t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	return c
}

var alice = map[string]any{"email": "Alice@Example.com", "email_verified": true, "name": "Alice"}

func TestLoginHappyPath(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.finish(e.start("/trips/5?x=1"), alice)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/trips/5?x=1" {
		t.Fatalf("callback = %d -> %q, want 302 -> /trips/5?x=1", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionCookieOf(rec)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Fatalf("session cookie flags wrong for http PublicURL: %+v", c)
	}
	if time.Until(c.Expires) < 6*24*time.Hour {
		t.Errorf("session cookie expires %v, want ~7 days", c.Expires)
	}

	var stored string
	if err := e.store.Read().QueryRow(`SELECT token_hash FROM auth_sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == c.Value || stored != hashToken(c.Value) {
		t.Errorf("DB must store sha256(token), got %q for cookie %q", stored, c.Value)
	}

	me := e.do("GET", "/api/me", c)
	if me.Code != 200 || !strings.Contains(me.Body.String(), `"email":"Alice@Example.com"`) ||
		!strings.Contains(me.Body.String(), `"name":"Alice"`) || !strings.Contains(me.Body.String(), `"enabled":true`) {
		t.Errorf("/api/me = %d %s", me.Code, me.Body.String())
	}
}

func TestSessionCookieSecureWithHTTPSPublicURL(t *testing.T) {
	e := newEnv(t, func(c *config.OIDCConfig) { c.PublicURL = "https://dashcam.test" })
	e.idp.redirectURI = "https://dashcam.test/auth/callback"
	c := e.login(alice)
	if !c.Secure {
		t.Errorf("session cookie must be Secure with an https PublicURL: %+v", c)
	}
}

func TestCallbackRejects(t *testing.T) {
	other, _ := rsa.GenerateKey(rand.Reader, 2048)

	t.Run("state mismatch", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		f.cookie = &http.Cookie{Name: loginStateCookie, Value: "different"}
		if rec := e.finish(f, alice); rec.Code != 400 || sessionCookieOf(rec) != nil {
			t.Errorf("status = %d, want 400 with no session", rec.Code)
		}
	})
	t.Run("missing cookie", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		f.cookie = nil
		rec := e.do("GET", "/auth/callback?code=x&state="+url.QueryEscape(f.state))
		if rec.Code != 400 {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("replayed state", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		if rec := e.finish(f, alice); rec.Code != 302 {
			t.Fatalf("first callback = %d", rec.Code)
		}
		if rec := e.finish(f, alice); rec.Code != 400 {
			t.Errorf("replay = %d, want 400", rec.Code)
		}
	})
	t.Run("bad nonce", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		f.nonce = "not-the-nonce"
		if rec := e.finish(f, alice); rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		e := newEnv(t, nil)
		if rec := e.finish(e.start("/"), map[string]any{"aud": "someone-else"}); rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("expired token", func(t *testing.T) {
		e := newEnv(t, nil)
		if rec := e.finish(e.start("/"), map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}); rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("wrong signing key", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		e.idp.addCode("bad-key", f.challenge, f.nonce, alice, other)
		rec := e.do("GET", "/auth/callback?code=bad-key&state="+url.QueryEscape(f.state), f.cookie)
		if rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("idp error", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		rec := e.do("GET", "/auth/callback?error=access_denied&state="+url.QueryEscape(f.state), f.cookie)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "access_denied") {
			t.Errorf("status = %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("pkce verifier mismatch", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.start("/")
		f.challenge = "challenge-for-a-different-verifier"
		if rec := e.finish(f, alice); rec.Code == 302 || sessionCookieOf(rec) != nil {
			t.Errorf("status = %d, want failure with no session", rec.Code)
		}
	})
	t.Run("not allowed", func(t *testing.T) {
		e := newEnv(t, func(c *config.OIDCConfig) { c.AllowedEmails = []string{"bob@example.com"} })
		rec := e.finish(e.start("/"), alice)
		if rec.Code != 403 || sessionCookieOf(rec) != nil || !strings.Contains(rec.Body.String(), "Sign out") {
			t.Errorf("status = %d body %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAuthorize(t *testing.T) {
	cfg := func(emails, groups []string, claim string) config.OIDCConfig {
		if claim == "" {
			claim = "groups"
		}
		return config.OIDCConfig{AllowedEmails: emails, AllowedGroups: groups, GroupsClaim: claim}
	}
	cases := []struct {
		name   string
		cfg    config.OIDCConfig
		claims map[string]any
		want   bool
	}{
		{"empty lists allow anyone", cfg(nil, nil, ""), map[string]any{"email": "x@y.z"}, true},
		{"email hit case-insensitive", cfg([]string{"a@x.com"}, nil, ""), map[string]any{"email": "A@X.com"}, true},
		{"email miss", cfg([]string{"a@x.com"}, nil, ""), map[string]any{"email": "b@x.com"}, false},
		{"email unverified", cfg([]string{"a@x.com"}, nil, ""), map[string]any{"email": "a@x.com", "email_verified": false}, false},
		{"email verified claim absent", cfg([]string{"a@x.com"}, nil, ""), map[string]any{"email": "a@x.com"}, true},
		{"group array hit", cfg(nil, []string{"fam"}, ""), map[string]any{"groups": []any{"x", "fam"}}, true},
		{"group string hit", cfg(nil, []string{"fam"}, ""), map[string]any{"groups": "fam"}, true},
		{"group miss", cfg(nil, []string{"fam"}, ""), map[string]any{"groups": []any{"x"}}, false},
		{"custom claim", cfg(nil, []string{"fam"}, "roles"), map[string]any{"roles": []any{"fam"}}, true},
		{"custom claim ignores default", cfg(nil, []string{"fam"}, "roles"), map[string]any{"groups": []any{"fam"}}, false},
		{"email miss but group hit", cfg([]string{"a@x.com"}, []string{"fam"}, ""), map[string]any{"email": "b@x.com", "groups": []any{"fam"}}, true},
		{"unverified email but group hit", cfg([]string{"a@x.com"}, []string{"fam"}, ""), map[string]any{"email": "a@x.com", "email_verified": false, "groups": []any{"fam"}}, true},
	}
	for _, tc := range cases {
		if _, _, ok := Authorize(tc.cfg, tc.claims); ok != tc.want {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.want)
		}
	}

	_, name, _ := Authorize(cfg(nil, nil, ""), map[string]any{"preferred_username": "pu", "email": "e@x.com"})
	if name != "pu" {
		t.Errorf("name = %q, want preferred_username fallback", name)
	}
	_, name, _ = Authorize(cfg(nil, nil, ""), map[string]any{"email": "e@x.com"})
	if name != "e@x.com" {
		t.Errorf("name = %q, want email fallback", name)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                    "/",
		"//evil.com":          "/",
		"/\\evil":             "/",
		"https://x":           "/",
		"javascript:alert(1)": "/",
		"/auth/login":         "/",
		"/auth/callback?x=1":  "/",
		"/a\r\nb":             "/",
		"/a\x00b":             "/",
		"relative":            "/",
		"/trips/5":            "/trips/5",
		"/trips/5?x=1&y=2":    "/trips/5?x=1&y=2",
		"/":                   "/",
	}
	for in, want := range cases {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGate(t *testing.T) {
	e := newEnv(t, nil)

	for _, p := range []string{"/api/health", "/assets/x.js", "/auth/login"} {
		if rec := e.do("GET", p); rec.Code == 401 || (rec.Code == 302 && strings.HasPrefix(rec.Header().Get("Location"), "/auth/login?next")) {
			t.Errorf("%s should be public, got %d -> %s", p, rec.Code, rec.Header().Get("Location"))
		}
	}

	rec := e.do("GET", "/api/trips")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"error":"authentication required"`) ||
		!strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("/api/trips unauthenticated = %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", "/trips/5?x=1")
	if rec.Code != 302 || rec.Header().Get("Location") != "/auth/login?next=%2Ftrips%2F5%3Fx%3D1" {
		t.Errorf("SPA route = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.do("POST", "/trips/5"); rec.Code != 401 {
		t.Errorf("non-GET SPA route = %d, want 401", rec.Code)
	}

	c := e.login(alice)
	if rec := e.do("GET", "/api/trips", c); rec.Code != 200 {
		t.Errorf("authenticated /api/trips = %d", rec.Code)
	}
	if rec := e.do("GET", "/api/trips", &http.Cookie{Name: sessionCookie, Value: "forged"}); rec.Code != 401 {
		t.Errorf("forged cookie = %d, want 401", rec.Code)
	}

	// Media with Range keeps working for a signed-in browser.
	req := httptest.NewRequest("GET", "/api/media", nil)
	req.AddCookie(c)
	req.Header.Set("Range", "bytes=10-19")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent || rr.Body.String() != "0123456789" {
		t.Errorf("Range GET = %d %q, want 206", rr.Code, rr.Body.String())
	}
	if rec := e.do("GET", "/api/media"); rec.Code != 401 {
		t.Errorf("unauthenticated media = %d, want 401", rec.Code)
	}

	// Absolute 7-day expiry.
	e.a.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if rec := e.do("GET", "/api/trips", c); rec.Code != 401 {
		t.Errorf("expired session = %d, want 401", rec.Code)
	}
}

func TestGateReauthorizesEveryRequest(t *testing.T) {
	e := newEnv(t, func(c *config.OIDCConfig) { c.AllowedEmails = []string{"alice@example.com"} })
	c := e.login(alice)
	if rec := e.do("GET", "/api/trips", c); rec.Code != 200 {
		t.Fatalf("allowed user = %d", rec.Code)
	}
	tight := e.a.cur()
	tight.AllowedEmails = []string{"bob@example.com"} // operator tightens the allow-list
	e.a.Configure(&tight)
	if rec := e.do("GET", "/api/trips", c); rec.Code != 401 {
		t.Errorf("removed user = %d, want 401", rec.Code)
	}
	if _, ok, _ := e.store.AuthSessionGet(context.Background(), hashToken(c.Value), time.Now()); ok {
		t.Error("session should be deleted once no longer authorized")
	}
}

func TestLogout(t *testing.T) {
	for _, withEnd := range []bool{true, false} {
		e := newEnv(t, nil)
		e.idp.endSession = withEnd
		c := e.login(alice)
		rec := e.do("POST", "/auth/logout", c)
		if rec.Code != 200 {
			t.Fatalf("logout = %d", rec.Code)
		}
		var out struct{ Redirect string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if withEnd {
			u, _ := url.Parse(out.Redirect)
			q := u.Query()
			if !strings.HasPrefix(out.Redirect, e.idp.srv.URL+"/logout") || q.Get("id_token_hint") == "" ||
				q.Get("post_logout_redirect_uri") != publicURL+"/" || q.Get("client_id") != testClientID {
				t.Errorf("redirect = %s", out.Redirect)
			}
		} else if out.Redirect != "/" {
			t.Errorf("redirect = %q, want /", out.Redirect)
		}
		if _, ok, _ := e.store.AuthSessionGet(context.Background(), hashToken(c.Value), time.Now()); ok {
			t.Error("session row survived logout")
		}
		cleared := false
		for _, ck := range rec.Result().Cookies() {
			if ck.Name == sessionCookie && ck.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Error("session cookie not expired")
		}
		if rec := e.do("GET", "/api/trips", c); rec.Code != 401 {
			t.Errorf("after logout = %d, want 401", rec.Code)
		}
	}
}

func TestDiscoveryDownThenRecovers(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.down.Store(true)
	clock := time.Now()
	e.a.now = func() time.Time { return clock }

	if rec := e.do("GET", "/auth/login?next=/"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "unavailable") {
		t.Fatalf("login with IdP down = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/health"); rec.Code != 200 {
		t.Errorf("health with IdP down = %d, want 200", rec.Code)
	}

	// IdP recovers, but the retry backoff hasn't elapsed.
	e.idp.down.Store(false)
	if rec := e.do("GET", "/auth/login?next=/"); rec.Code != 503 {
		t.Errorf("login inside backoff = %d, want 503", rec.Code)
	}
	clock = clock.Add(6 * time.Second)
	if rec := e.do("GET", "/auth/login?next=/"); rec.Code != 302 {
		t.Errorf("login after backoff = %d, want 302", rec.Code)
	}
}

func TestMeDisabled(t *testing.T) {
	a := New(nil, "", false)
	rec := httptest.NewRecorder()
	a.Me(rec, httptest.NewRequest("GET", "/api/me", nil))
	if strings.TrimSpace(rec.Body.String()) != `{"auth_disabled_override":false,"auth_enabled":false,"enabled":false,"user":null}` {
		t.Errorf("body = %s", rec.Body.String())
	}
	next := http.NewServeMux()
	next.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("open")) })
	rec = httptest.NewRecorder()
	a.Gate(next, func(string) bool { return false }).ServeHTTP(rec, httptest.NewRequest("GET", "/api/trips", nil))
	if rec.Code != 200 || rec.Body.String() != "open" {
		t.Errorf("Gate with login disabled must pass everything: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLogoutWithoutSessionStillEndsIdPSession(t *testing.T) {
	e := newEnv(t, func(c *config.OIDCConfig) { c.AllowedEmails = []string{"bob@example.com"} })
	// A denied login: discovery is cached, but no dcm_session is issued.
	if rec := e.finish(e.start("/"), alice); rec.Code != 403 {
		t.Fatalf("denied login = %d, want 403", rec.Code)
	}
	rec := e.do("POST", "/auth/logout")
	var out struct{ Redirect string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(out.Redirect)
	q := u.Query()
	if !strings.HasPrefix(out.Redirect, e.idp.srv.URL+"/logout") || q.Get("client_id") != testClientID ||
		q.Get("post_logout_redirect_uri") != publicURL+"/" || q.Has("id_token_hint") {
		t.Errorf("redirect = %s, want end-session URL without id_token_hint", out.Redirect)
	}
}

func TestUserInfoFallback(t *testing.T) {
	allow := func(c *config.OIDCConfig) {
		c.AllowedEmails = []string{"alice@example.com"}
		c.AllowedGroups = []string{"dash"}
	}
	run := func(e *env, claims map[string]any) *httptest.ResponseRecorder {
		return e.finish(e.start("/"), claims)
	}
	t.Run("email only in userinfo", func(t *testing.T) {
		e := newEnv(t, allow)
		e.idp.userinfo = map[string]any{"email": "alice@example.com", "email_verified": true}
		if rec := run(e, map[string]any{}); sessionCookieOf(rec) == nil {
			t.Fatalf("want allowed, got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("sub mismatch ignored", func(t *testing.T) {
		e := newEnv(t, allow)
		e.idp.userinfo = map[string]any{"sub": "other", "email": "alice@example.com"}
		if rec := run(e, map[string]any{}); rec.Code != http.StatusForbidden || sessionCookieOf(rec) != nil {
			t.Fatalf("want 403, got %d", rec.Code)
		}
	})
	t.Run("id token wins", func(t *testing.T) {
		e := newEnv(t, allow)
		e.idp.userinfo = map[string]any{"email": "alice@example.com", "groups": []string{"dash"}}
		rec := run(e, map[string]any{"email": "mallory@example.com", "groups": []string{"x"}})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "mallory@example.com") {
			t.Fatalf("want 403 for id token email, got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("email_verified false denied", func(t *testing.T) {
		e := newEnv(t, allow)
		rec := run(e, map[string]any{"email": "alice@example.com", "email_verified": false})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not verified") {
			t.Fatalf("want 403 with not verified, got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("verified flag not paired with userinfo email", func(t *testing.T) {
		for _, ui := range []map[string]any{{"email": "alice@example.com"}, {"email": "alice@example.com", "email_verified": false}} {
			e := newEnv(t, allow)
			e.idp.userinfo = ui
			if rec := run(e, map[string]any{"email_verified": true}); rec.Code == http.StatusFound && sessionCookieOf(rec) != nil && ui["email_verified"] == false {
				t.Fatalf("unverified userinfo email accepted")
			}
		}
	})
	t.Run("groups only in userinfo", func(t *testing.T) {
		e := newEnv(t, allow)
		e.idp.userinfo = map[string]any{"groups": []string{"dash"}}
		if rec := run(e, map[string]any{"email": "bob@example.com"}); sessionCookieOf(rec) == nil {
			t.Fatalf("want allowed, got %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestVerifyFailMessageHS256Hint(t *testing.T) {
	err := errors.New(`oidc: id token signed with unsupported algorithm, expected ["RS256"] got "HS256"`)
	if got := verifyFailMessage(err); !strings.Contains(got, "set a signing key (RS256)") && !strings.Contains(got, "Set a signing key (RS256)") {
		t.Errorf("hint missing: %q", got)
	}
	if got := verifyFailMessage(errors.New("oidc: token expired")); got != "The ID token could not be verified." {
		t.Errorf("generic message changed: %q", got)
	}
}
