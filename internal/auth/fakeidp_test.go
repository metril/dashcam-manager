package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testClientID = "dashcam"
	testSecret   = "s3cret"
)

// codeRec is what the fake IdP remembers about an issued authorization code.
type codeRec struct {
	challenge string
	nonce     string
	claims    map[string]any // merged over the default ID token claims
	key       *rsa.PrivateKey
}

// fakeIdP is an httptest OIDC provider: discovery, JWKS and a token
// endpoint that enforces PKCE (S256), the client secret and redirect_uri.
type fakeIdP struct {
	t           *testing.T
	srv         *httptest.Server
	key         *rsa.PrivateKey
	redirectURI string
	endSession  bool
	down        atomic.Bool    // discovery returns 500
	userinfo    map[string]any // userinfo response (sub defaults to user-1); nil = 401

	mu    sync.Mutex
	codes map[string]codeRec
}

func newFakeIdP(t *testing.T, redirectURI string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, redirectURI: redirectURI, endSession: true, codes: map[string]codeRec{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("/jwks", f.jwks)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/userinfo", f.userInfo)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) discovery(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	doc := map[string]any{
		"issuer":                                f.srv.URL,
		"authorization_endpoint":                f.srv.URL + "/authorize",
		"token_endpoint":                        f.srv.URL + "/token",
		"userinfo_endpoint":                     f.srv.URL + "/userinfo",
		"jwks_uri":                              f.srv.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	if f.endSession {
		doc["end_session_endpoint"] = f.srv.URL + "/logout"
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeIdP) userInfo(w http.ResponseWriter, r *http.Request) {
	if f.userinfo == nil || r.Header.Get("Authorization") != "Bearer at" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	out := map[string]any{"sub": "user-1"}
	for k, v := range f.userinfo {
		out[k] = v
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeIdP) jwks(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}})
}

// addCode registers an authorization code the way the IdP would have after a
// user authenticated, bound to the challenge/nonce from the /authorize URL.
func (f *fakeIdP) addCode(code, challenge, nonce string, claims map[string]any, signWith *rsa.PrivateKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if signWith == nil {
		signWith = f.key
	}
	f.codes[code] = codeRec{challenge: challenge, nonce: nonce, claims: claims, key: signWith}
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	oauthErr := func(msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	_ = r.ParseForm()
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != testClientID || secret != testSecret {
		oauthErr("invalid_client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != f.redirectURI {
		oauthErr("invalid_request")
		return
	}
	f.mu.Lock()
	rec, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	if !ok {
		oauthErr("invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != rec.challenge {
		oauthErr("invalid_grant") // PKCE verifier mismatch
		return
	}

	now := time.Now()
	claims := map[string]any{
		"iss": f.srv.URL, "aud": testClientID, "sub": "user-1",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": rec.nonce,
	}
	for k, v := range rec.claims {
		claims[k] = v
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: rec.key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		f.t.Fatal(err)
	}
	idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
	})
}
