package dashcam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DownloadResult reports what Download actually wrote.
type DownloadResult struct {
	Bytes  int64
	SHA256 string
}

// maxDownloadAttempts bounds Download's retry loop (1 initial try + 4
// retries, matching the brief's "1s,2s,4s… max 5 attempts").
const maxDownloadAttempts = 5

// downloadBackoff is attempt (0-indexed retry number) -> sleep duration.
func downloadBackoff(retry int) time.Duration {
	d := time.Second
	for i := 0; i < retry; i++ {
		d *= 2
	}
	return d
}

// Download fetches remoteRel (a manifest segment or sidecar rel path) to
// destPath, streaming through sha256 as it writes so the caller never has
// to re-read the file to verify it. It writes "<dest>.part" in dest's
// directory and only renames it into place once the transfer completes and
// is fsync'd, so a crash mid-download never leaves a file at destPath a
// later run would mistake for a finished one. A retryable failure (429,
// 5xx, or a network error) is retried with backoff; if the server answers
// a retry with 206 Partial Content, the existing ".part" bytes are kept
// and hashed incrementally rather than re-downloaded — otherwise (no Range
// support, or the server no longer has a partial view) the ".part" file is
// truncated and the download restarts from 0.
func (c *Client) Download(ctx context.Context, remoteRel, destPath string) (DownloadResult, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return DownloadResult{}, fmt.Errorf("dashcam: mkdir for %s: %w", destPath, err)
	}

	v := url.Values{}
	v.Set("path", remoteRel)
	pathQS := "/api/ingest/file" + buildQuery(v)

	partPath := destPath + ".part"
	var hasher hash.Hash = sha256.New()
	var written int64

	var lastErr error
	for attempt := 0; attempt < maxDownloadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return DownloadResult{}, ctx.Err()
			case <-time.After(downloadBackoff(attempt - 1)):
			}
		}

		n, resumed, err := c.downloadAttempt(ctx, pathQS, partPath, hasher, written)
		// n reflects what's actually on disk (and hashed) now, whether or
		// not this attempt ultimately errored — a mid-stream failure still
		// flushed bytes to partPath before it died, and the next attempt's
		// Range header must be based on those, not on the pre-attempt count.
		written = n
		if err == nil {
			break
		}
		lastErr = err
		if !resumed {
			// Server didn't (or couldn't) resume from `written` — the
			// hasher and on-disk bytes are no longer trustworthy as a
			// prefix, so start clean on the next attempt.
			hasher = sha256.New()
			written = 0
			_ = os.Remove(partPath)
		}
		if !isRetryable(err) {
			return DownloadResult{}, err
		}
	}
	if lastErr != nil && written == 0 {
		return DownloadResult{}, fmt.Errorf("dashcam: download %s: %w", remoteRel, lastErr)
	}

	f, err := os.Open(partPath)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("dashcam: reopen %s: %w", partPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return DownloadResult{}, fmt.Errorf("dashcam: fsync %s: %w", partPath, err)
	}
	f.Close()

	if err := os.Rename(partPath, destPath); err != nil {
		return DownloadResult{}, fmt.Errorf("dashcam: rename %s -> %s: %w", partPath, destPath, err)
	}
	if d, err := os.Open(filepath.Dir(destPath)); err == nil {
		_ = d.Sync()
		d.Close()
	}

	return DownloadResult{Bytes: written, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// retryableError wraps a transport/HTTP error Download's caller should
// back off and retry, distinct from a hard failure (e.g. 401/404) that
// should surface immediately.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	_, ok := err.(*retryableError)
	return ok
}

// downloadAttempt performs one GET (possibly with Range: bytes=have-) and
// streams the response into partPath, updating hasher as it goes. It
// returns the total bytes now in partPath and whether the server honored
// (or at least didn't invalidate) the existing prefix — false means the
// caller must discard partPath and hasher and restart from 0 on the next
// attempt.
func (c *Client) downloadAttempt(ctx context.Context, pathQS, partPath string, hasher hash.Hash, have int64) (int64, bool, error) {
	var extra http.Header
	if have > 0 {
		extra = http.Header{"Range": []string{"bytes=" + strconv.FormatInt(have, 10) + "-"}}
	}

	resp, err := c.doHeaders(ctx, "GET", pathQS, nil, extra)
	if err != nil {
		var ae *APIError
		if asAPIError(err, &ae) {
			if ae.Status == http.StatusTooManyRequests || ae.Status >= 500 {
				// Resumable — keep what we already have on disk.
				return have, true, &retryableError{err}
			}
			return have, true, err
		}
		// Network-level error: connection may have died mid-body, so the
		// bytes already flushed to partPath are still good; keep them.
		return have, true, &retryableError{err}
	}
	defer resp.Body.Close()

	resumed := resp.StatusCode == http.StatusPartialContent
	flags := os.O_CREATE | os.O_WRONLY
	offset := int64(0)
	if resumed {
		flags |= os.O_APPEND
		offset = have
	} else {
		// Server ignored (or we didn't send) Range: start over, both on
		// disk and in the running hash — any bytes hashed for a previous
		// attempt's partial prefix are no longer part of what's coming.
		flags |= os.O_TRUNC
		if have > 0 {
			hasher.Reset()
		}
	}
	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return 0, false, fmt.Errorf("dashcam: open %s: %w", partPath, err)
	}
	defer f.Close()

	dst := io.MultiWriter(f, hasher)
	n, copyErr := io.Copy(dst, resp.Body)
	total := offset + n
	if copyErr != nil {
		return total, true, &retryableError{fmt.Errorf("dashcam: stream %s: %w", partPath, copyErr)}
	}
	return total, true, nil
}

func asAPIError(err error, target **APIError) bool {
	if ae, ok := err.(*APIError); ok {
		*target = ae
		return true
	}
	return false
}
