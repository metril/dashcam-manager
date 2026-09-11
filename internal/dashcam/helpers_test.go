package dashcam_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func base64Std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// testCanonicalString duplicates dashcam's unexported canonicalString
// (method\npathQS\nts\nnonce\nsha256hex(body)) so external tests can
// hand-build a validly-signed request without needing package-internal
// access; see client_test.go's replay test.
func testCanonicalString(method, pathQS string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	s := method + "\n" + pathQS + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
	return []byte(s)
}
