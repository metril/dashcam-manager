// Package dashcam is a signed HTTP client for the dashcam recorder's
// pull-ingest API (see resources/ingest.py, ingest_client.py and
// ingest_server.py in the sibling `dashcam` repo). Every request is
// Ed25519-signed the same way the Python reference client signs it — same
// canonical string, same headers — so this client and the recorder's own
// CLI client are interchangeable against the same server. keys.go adds the
// manager-side half: generating and storing the keypair the recorder needs
// configured as one of its public_keys.
package dashcam

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Header names the protocol signs/reads. Must match resources/ingest.py's
// RequestVerifier byte-for-byte.
const (
	headerKeyID    = "X-Ingest-Key-Id"
	headerTS       = "X-Ingest-Timestamp"
	headerNonce    = "X-Ingest-Nonce"
	headerSig      = "X-Ingest-Signature"
	headerContType = "Content-Type"
)

// Client is a signed client for one dashcam recorder's ingest API.
type Client struct {
	BaseURL string
	KeyID   string
	Priv    ed25519.PrivateKey
	HTTP    *http.Client
	// Now returns the client's idea of "now"; overridable in tests. It is
	// never mutated by this package — clock-skew correction lives in the
	// unexported skew field, added on top of Now() when timestamping a
	// request, so tests can fake a wrong local clock without redefining
	// Now on every retry.
	Now func() time.Time

	mu   sync.Mutex
	skew time.Duration
}

// Skew returns the clock-skew correction currently applied to outgoing
// request timestamps: server clock minus this client's own Now(), as last
// learned from a 401 clock_skew response's ServerTime (see do/attempt).
// Zero until the first such correction happens — a caller wanting a fresh
// reading (rather than whatever was last learned, possibly from a much
// earlier request) should make a request first and check Skew() after.
func (c *Client) Skew() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.skew
}

// New builds a Client. The HTTP client has dial and response-header
// timeouts (so a dead recorder is noticed promptly) but no overall request
// timeout — segment downloads can be large and slow-but-alive.
func New(baseURL, keyID string, priv ed25519.PrivateKey) *Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Client{
		BaseURL: baseURL,
		KeyID:   keyID,
		Priv:    priv,
		HTTP:    &http.Client{Transport: transport},
		Now:     time.Now,
	}
}

// APIError is the decoded {"error":code,"server_time":unix} body the
// ingest API returns on any non-2xx/206 response. ServerTime is float64
// (unix seconds with a fractional part) because the Python server sends
// time.time()'s float, not an integer — decoding it as int64 silently
// fails (leaving ServerTime at 0) and breaks the clock-skew retry.
type APIError struct {
	Status     int
	Code       string
	ServerTime float64
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dashcam: %s (http %d)", e.Code, e.Status)
}

// IsCode reports whether err is an *APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == code
}

// canonicalString builds the exact bytes that get Ed25519-signed, matching
// resources/ingest.py's canonical_string byte-for-byte: method, the raw
// request-target (path+query, unescaped further), unix timestamp, nonce,
// and the hex sha256 of the body (sha256 of b” for an empty body).
func canonicalString(method, pathQS string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	s := method + "\n" + pathQS + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
	return []byte(s)
}

// newNonce returns a base64(std)-encoded 16-byte random nonce.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dashcam: nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// signHeaders signs (method, pathQS, body) at timestamp ts with a fresh
// nonce and returns the four X-Ingest-* headers to attach.
func (c *Client) signHeaders(method, pathQS string, ts int64, body []byte) (http.Header, error) {
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	msg := canonicalString(method, pathQS, ts, nonce, body)
	sig := ed25519.Sign(c.Priv, msg)
	h := http.Header{}
	h.Set(headerKeyID, c.KeyID)
	h.Set(headerTS, strconv.FormatInt(ts, 10))
	h.Set(headerNonce, nonce)
	h.Set(headerSig, base64.StdEncoding.EncodeToString(sig))
	return h, nil
}

// buildQuery matches ingest_client.py's `call`: no leading "?" at all when
// there are no params (so an endpoint with no query string signs/sends
// exactly its bare path), otherwise "?" + url.Values.Encode().
func buildQuery(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// do sends one signed request, transparently correcting for clock skew: a
// 401 clock_skew response carries the server's clock, which this client
// then adopts (via the unexported skew field) and retries once. Any other
// non-2xx/206 response becomes an *APIError. On success the caller owns
// the returned *http.Response and must close its Body (this matters most
// for Download, which streams a large body rather than buffering it).
func (c *Client) do(ctx context.Context, method, pathQS string, body []byte) (*http.Response, error) {
	return c.doHeaders(ctx, method, pathQS, body, nil)
}

func (c *Client) doHeaders(ctx context.Context, method, pathQS string, body []byte, extra http.Header) (*http.Response, error) {
	resp, apiErr, err := c.attempt(ctx, method, pathQS, body, extra)
	if err != nil {
		return nil, err
	}
	if apiErr != nil && apiErr.Code == "clock_skew" {
		c.mu.Lock()
		c.skew = time.Duration((apiErr.ServerTime - float64(c.Now().Unix())) * float64(time.Second))
		c.mu.Unlock()
		resp, apiErr, err = c.attempt(ctx, method, pathQS, body, extra)
		if err != nil {
			return nil, err
		}
	}
	if apiErr != nil {
		return nil, apiErr
	}
	return resp, nil
}

// attempt performs exactly one signed HTTP round trip. On a non-2xx/206
// response it reads and closes the body itself, decodes it as an APIError,
// and returns (nil, apiErr, nil) — the caller decides whether to retry.
func (c *Client) attempt(ctx context.Context, method, pathQS string, body []byte, extra http.Header) (*http.Response, *APIError, error) {
	c.mu.Lock()
	ts := c.Now().Add(c.skew).Unix()
	c.mu.Unlock()

	headers, err := c.signHeaders(method, pathQS, ts, body)
	if err != nil {
		return nil, nil, err
	}

	u, err := url.Parse(c.BaseURL + pathQS)
	if err != nil {
		return nil, nil, fmt.Errorf("dashcam: parse url: %w", err)
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, nil, fmt.Errorf("dashcam: new request: %w", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if len(body) > 0 {
		req.Header.Set(headerContType, "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("dashcam: request %s %s: %w", method, pathQS, err)
	}

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		return resp, nil, nil
	}

	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body2 struct {
		Error      string  `json:"error"`
		ServerTime float64 `json:"server_time"`
	}
	_ = json.Unmarshal(raw, &body2)
	return nil, &APIError{Status: resp.StatusCode, Code: body2.Error, ServerTime: body2.ServerTime}, nil
}
