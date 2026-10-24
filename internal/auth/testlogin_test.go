package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/share"
)

// disabledEnv is an env whose Authenticator starts with login off, as on a
// fresh install about to run the settings UI's test login.
func disabledEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, nil)
	e.a.Configure(nil)
	return e
}

func (e *env) pending(allowedEmails ...string) PendingConfig {
	enc, err := share.Encrypt(testSecretKey, testSecret)
	if err != nil {
		e.t.Fatal(err)
	}
	return PendingConfig{
		Issuer: e.idp.srv.URL, ClientID: testClientID, ClientSecretEnc: base64.StdEncoding.EncodeToString(enc),
		Scopes: []string{"openid", "email", "profile"}, AllowedEmails: allowedEmails, GroupsClaim: "groups", PublicURL: publicURL,
	}
}

// startTest plays the browser hitting /auth/test-login.
func (e *env) startTest(cookies ...*http.Cookie) (flow, int) {
	e.t.Helper()
	rec := e.do("GET", testLoginPath, cookies...)
	if rec.Code != http.StatusFound {
		return flow{}, rec.Code
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	f := flow{state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	for _, c := range rec.Result().Cookies() {
		if c.Name == loginStateCookie {
			f.cookie = c
		}
	}
	return f, rec.Code
}

func TestTestLoginCommitsSettingsAndEnables(t *testing.T) {
	e := disabledEnv(t)
	var committed atomic.Bool
	e.a.SetOnCommit(func(context.Context) { committed.Store(true) })
	ctx := context.Background()

	if err := e.a.StartTest(ctx, e.pending("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	f, code := e.startTest()
	if code != http.StatusFound {
		t.Fatalf("test-login = %d", code)
	}
	rec := e.finish(f, alice)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/settings?oidc=ok" {
		t.Fatalf("callback = %d -> %s: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	sess := sessionCookieOf(rec)
	if sess == nil {
		t.Fatal("no session cookie after a passing test")
	}
	if !e.a.Enabled() || !committed.Load() {
		t.Errorf("Enabled = %v, committed = %v, want both true", e.a.Enabled(), committed.Load())
	}
	all, err := e.store.SettingsGetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(all["oidc_enabled"]) != "true" || string(all["oidc_issuer"]) != `"`+e.idp.srv.URL+`"` ||
		string(all["oidc_allowed_emails"]) != `["alice@example.com"]` || string(all["public_url"]) != `"`+publicURL+`"` {
		t.Errorf("settings after commit: %v", all)
	}
	if enc := string(all["oidc_client_secret_enc"]); enc == "" || enc == `""` || enc == testSecret {
		t.Errorf("client secret must be stored encrypted, got %s", enc)
	}
	// Gate now enforces: no cookie 401, with the new session 200.
	if got := e.do("GET", "/api/trips"); got.Code != 401 {
		t.Errorf("anonymous after enable = %d, want 401", got.Code)
	}
	if got := e.do("GET", "/api/trips", sess); got.Code != 200 {
		t.Errorf("session after enable = %d, want 200", got.Code)
	}
	// Single use: the pending row is gone.
	if _, code := e.startTest(sess); code != http.StatusBadRequest {
		t.Errorf("second test-login = %d, want 400", code)
	}
}

func TestTestLoginDisallowedUserWritesNothing(t *testing.T) {
	e := disabledEnv(t)
	ctx := context.Background()
	if err := e.a.StartTest(ctx, e.pending("bob@example.com")); err != nil {
		t.Fatal(err)
	}
	f, _ := e.startTest()
	rec := e.finish(f, alice)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback = %d, want 403", rec.Code)
	}
	if sessionCookieOf(rec) != nil {
		t.Error("no session may be issued for a failed test")
	}
	if e.a.Enabled() {
		t.Error("login must stay disabled")
	}
	all, _ := e.store.SettingsGetAll(ctx)
	for k := range all {
		if k != "schema_version" {
			t.Errorf("unexpected setting written on failed test: %s", k)
		}
	}
}

func TestTestLoginRejectsExpiredPending(t *testing.T) {
	e := disabledEnv(t)
	if err := e.a.StartTest(context.Background(), e.pending()); err != nil {
		t.Fatal(err)
	}
	e.a.now = func() time.Time { return time.Now().Add(pendingTTL + time.Minute) }
	if _, code := e.startTest(); code != http.StatusBadRequest {
		t.Errorf("expired test-login = %d, want 400", code)
	}
	if e.a.Enabled() {
		t.Error("login must stay disabled")
	}
}

func TestTestLoginNeedsSessionOnceEnabled(t *testing.T) {
	e := newEnv(t, nil) // login is on
	rec := e.do("GET", testLoginPath)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login") {
		t.Errorf("anonymous test-login = %d -> %s, want redirect to sign-in", rec.Code, rec.Header().Get("Location"))
	}
}

func TestGatePassesWhenAuthDisableSet(t *testing.T) {
	e := newEnv(t, nil)
	e.a.authDisable = true
	if rec := e.do("GET", "/api/trips"); rec.Code != 200 {
		t.Errorf("AuthDisable: anonymous = %d, want 200", rec.Code)
	}
	rec := e.do("GET", "/api/me")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"auth_disabled_override":true`) || !strings.Contains(rec.Body.String(), `"auth_enabled":true`) {
		t.Errorf("Me under AuthDisable = %d %s", rec.Code, rec.Body.String())
	}
}

// TestConfigureSwapIsRaceFree swaps the config while sessions are looked up
// concurrently (meaningful under -race).
func TestConfigureSwapIsRaceFree(t *testing.T) {
	e := newEnv(t, nil)
	c := e.login(alice)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					req, _ := http.NewRequest("GET", "/api/trips", nil)
					req.AddCookie(c)
					_, _ = e.a.lookup(req)
					_ = e.a.Enabled()
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		cfg := e.a.cur()
		cfg.AllowedEmails = []string{"alice@example.com"}
		e.a.Configure(&cfg)
		if i%50 == 0 {
			e.a.Configure((*config.OIDCConfig)(nil))
			e.a.Configure(&cfg)
		}
	}
	close(stop)
	wg.Wait()
}
