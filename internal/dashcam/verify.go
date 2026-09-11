package dashcam

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// VerifyRequest is the Go-side re-implementation of resources/ingest.py's
// RequestVerifier signature check for a single already-resolved public
// key: it does not know about key ids, unknown keys, or nonce replay
// (that's Verifier, below) — just "does this request's signature match
// this key, within maxSkew of now". code is one of "missing_headers",
// "clock_skew", "bad_signature", or "" (ok=true).
func VerifyRequest(r *http.Request, body []byte, pub ed25519.PublicKey, maxSkew time.Duration) (code string, ok bool) {
	keyID := r.Header.Get(headerKeyID)
	tsRaw := r.Header.Get(headerTS)
	nonce := r.Header.Get(headerNonce)
	sigB64 := r.Header.Get(headerSig)
	if keyID == "" || tsRaw == "" || nonce == "" || sigB64 == "" {
		return "missing_headers", false
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return "missing_headers", false
	}
	skewS := int64(maxSkew / time.Second)
	if skewS < 1 {
		skewS = 1
	}
	now := time.Now().Unix()
	diff := now - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > skewS {
		return "clock_skew", false
	}

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return "bad_signature", false
	}
	msg := canonicalString(r.Method, r.URL.RequestURI(), ts, nonce, body)
	if !ed25519.Verify(pub, msg, sig) {
		return "bad_signature", false
	}
	return "", true
}

// Verifier is a stateful, multi-key re-implementation of
// resources/ingest.py's RequestVerifier, including nonce-replay tracking.
// dashcamtest's fake server uses it; tests that want to drive verification
// directly (without a full fake server) can use it too.
type Verifier struct {
	PublicKeys map[string]ed25519.PublicKey
	MaxSkew    time.Duration
	Now        func() time.Time

	mu     sync.Mutex
	nonces map[string]time.Time
}

// Verify mirrors RequestVerifier.verify: 503 ingest_disabled if no keys
// are configured, else the usual 401 codes, else ("", 200)-equivalent
// (empty code, ok=true).
func (v *Verifier) Verify(r *http.Request, body []byte) (status int, code string) {
	if len(v.PublicKeys) == 0 {
		return 503, "ingest_disabled"
	}

	keyID := r.Header.Get(headerKeyID)
	tsRaw := r.Header.Get(headerTS)
	nonce := r.Header.Get(headerNonce)
	sigB64 := r.Header.Get(headerSig)
	if keyID == "" || tsRaw == "" || nonce == "" || sigB64 == "" {
		return 401, "missing_headers"
	}

	pub, known := v.PublicKeys[keyID]
	if !known {
		return 401, "unknown_key"
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return 401, "missing_headers"
	}

	now := v.now()
	skew := v.MaxSkew
	if skew <= 0 {
		skew = 300 * time.Second
	}
	skewS := int64(skew / time.Second)
	if skewS < 1 {
		skewS = 1
	}
	diff := now.Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > skewS {
		return 401, "clock_skew"
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneNoncesLocked(now, skewS)
	if _, seen := v.nonces[nonce]; seen {
		return 401, "replay"
	}

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return 401, "bad_signature"
	}
	msg := canonicalString(r.Method, r.URL.RequestURI(), ts, nonce, body)
	if !ed25519.Verify(pub, msg, sig) {
		return 401, "bad_signature"
	}

	if v.nonces == nil {
		v.nonces = map[string]time.Time{}
	}
	v.nonces[nonce] = now
	return 200, ""
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// pruneNoncesLocked drops nonces older than 2*maxSkew, matching
// RequestVerifier._prune_nonces. Caller holds v.mu.
func (v *Verifier) pruneNoncesLocked(now time.Time, skewS int64) {
	if v.nonces == nil {
		return
	}
	cutoff := now.Unix() - 2*skewS
	for n, t := range v.nonces {
		if t.Unix() < cutoff {
			delete(v.nonces, n)
		}
	}
}

// ErrorBody is the {"error":code,"server_time":unix} shape every non-2xx
// ingest response carries.
type ErrorBody struct {
	Error      string `json:"error"`
	ServerTime int64  `json:"server_time"`
}
