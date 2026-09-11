// Package dashcamtest is a reusable fake for resources/ingest_server.py's
// HTTP surface. It deliberately does NOT import internal/dashcam or reuse
// any of its types: verification is its own from-scratch canonical-string
// + Ed25519 check (mirroring resources/ingest.py's RequestVerifier
// independently rather than calling dashcam.Verifier), and every response
// is hand-built JSON shaped the way the Python server actually emits it
// (server_time/remaining_s as floats where Python's time.time() arithmetic
// makes them floats, tracking as the string label the dashboard reports,
// next_since capable of coming back JSON null). Sharing the client's Go
// structs on both ends of the fake would let a decode bug on the client
// side and an encode "bug" on the fake side cancel each other out
// silently — the whole point of a fake independent of the code under test.
package dashcamtest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is an in-process stand-in for the recorder's ingest API.
type Fake struct {
	Server *httptest.Server
	Dir    string // segment tree root

	// MaxConcurrent is the download concurrency limit (429 beyond it),
	// mirroring config.max_concurrent_downloads.
	MaxConcurrent int
	DefaultTTL    time.Duration
	MaxTTL        time.Duration
	// ReprocessTicks is how many ReprocessStatus polls a started run stays
	// "running" for before landing on its terminal state ("done", or
	// "error" when ReprocessError is set).
	ReprocessTicks int
	// ReprocessTracking is the tracking-mode label a running reprocess
	// reports (dashboard.py sends one of "off"/"single_segment"/
	// "per_recording", never a bool).
	ReprocessTracking string
	// ReprocessError, when non-empty, makes the next started run land on
	// state="error" (with this string as ReprocessStatus.Error) instead of
	// "done" once ReprocessTicks polls have elapsed — for exercising the
	// dashcam_reprocess job's failure path against a fake that fails the
	// same way the real recorder's background reprocess thread does (an
	// exception caught in resources/dashboard.py's _run_reprocess).
	ReprocessError string

	mu                 sync.Mutex
	nowFn              func() time.Time
	pubKeys            map[string]ed25519.PublicKey
	maxSkew            time.Duration
	nonces             map[string]int64
	segments           []segMeta
	inFlight           int
	lease              *leaseState
	reprocess          *reprocessState
	requests           []RecordedRequest
	killAfter          map[string]int64
	rangeMismatch      map[string]int64
	forceNextSinceNull bool
	stickyManifest     *stickyManifestPage
	forceDeleteError   map[string]forcedError
	// forceStatusErrorRemaining/forceStatusErrorSpec: see
	// ForceReprocessStatusError.
	forceStatusErrorRemaining int
	forceStatusErrorSpec      forcedError
	// forceReleaseErrorRemaining/forceReleaseErrorSpec: see
	// ForceReleaseError.
	forceReleaseErrorRemaining int
	forceReleaseErrorSpec      forcedError
}

// forcedError is a one-shot HTTP status/code to answer with instead of
// processing a request normally — see ForceDeleteErrorForPath.
type forcedError struct {
	status int
	code   string
}

type segMeta struct {
	Camera                                         string
	Path                                           string
	StartTime, EndTime                             string
	HasTimestamp, HasDetections, HasGPS, HasTracks bool
}

type leaseState struct {
	LeaseID   string
	ExpiresAt time.Time
	Reason    string
}

// reprocessState mirrors resources/dashboard.py's ReprocessHandler state
// machine: a POST /api/ingest/reprocess sets state="starting" synchronously
// (before returning 200), a background thread flips it to "running" and
// counts processed up to total, then lands on a *terminal* "done" (or
// "error", carrying err) that persists — unlike this fake's earlier
// idle/running-only model — until the next POST overwrites it. There is no
// path back to "idle" once a run has ever started.
type reprocessState struct {
	state                string // "starting" | "running" | "done" | "error"
	camera, date, engine string
	total, processed     int
	err                  string
}

// stickyManifestPage, when set, makes handleManifest return this exact
// page (verbatim, ignoring since/camera/limit) every time — used to
// exercise EachSegment's sticky-cursor guard without needing real
// same-start_time segment collisions to reproduce it.
type stickyManifestPage struct {
	segments []map[string]any
	next     any // string or nil
}

// RecordedRequest is one request the fake received, kept for tests that
// want to assert on nonce/timestamp uniqueness or call counts.
type RecordedRequest struct {
	Method   string
	Path     string
	RawQuery string
	Nonce    string
	Ts       string
}

// New starts a fake server rooted at dir (typically t.TempDir()). Register
// signing keys with AddKey before making requests — with none configured
// every request 503s (matching a real recorder with no public_keys set).
func New(dir string) *Fake {
	f := &Fake{
		Dir:               dir,
		MaxConcurrent:     2,
		DefaultTTL:        900 * time.Second,
		MaxTTL:            3600 * time.Second,
		ReprocessTicks:    2,
		ReprocessTracking: "per_recording",
		nowFn:             time.Now,
		pubKeys:           map[string]ed25519.PublicKey{},
		maxSkew:           300 * time.Second,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/ingest/manifest", f.wrap(f.handleManifest))
	mux.HandleFunc("/api/ingest/file", f.wrap(f.handleFile))
	mux.HandleFunc("/api/ingest/delete", f.wrap(f.handleDelete))
	mux.HandleFunc("/api/ingest/maintenance", f.wrap(f.handleMaintenanceRoot))
	mux.HandleFunc("/api/ingest/maintenance/renew", f.wrap(f.handleRenew))
	mux.HandleFunc("/api/ingest/maintenance/release", f.wrap(f.handleRelease))
	mux.HandleFunc("/api/ingest/reprocess", f.wrap(f.handleReprocess))
	mux.HandleFunc("/api/ingest/reprocess/status", f.wrap(f.handleReprocessStatus))
	f.Server = httptest.NewServer(mux)
	return f
}

// Close shuts the fake server down.
func (f *Fake) Close() { f.Server.Close() }

// URL is the fake's base URL, suitable for dashcam.New's baseURL arg.
func (f *Fake) URL() string { return f.Server.URL }

// AddKey registers a signing key the fake will accept.
func (f *Fake) AddKey(keyID string, pub ed25519.PublicKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubKeys[keyID] = pub
}

// SetMaxConcurrent changes the download concurrency limit while the fake
// is serving requests (safe to call concurrently with in-flight
// downloads, unlike writing the MaxConcurrent field directly — a test
// simulating "429 until the caller backs off, then it fits" needs this).
func (f *Fake) SetMaxConcurrent(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MaxConcurrent = n
}

// SetNow overrides the fake's clock (used for server_time in error bodies
// and clock-skew checks).
func (f *Fake) SetNow(fn func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nowFn = fn
}

func (f *Fake) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nowFn()
}

// Requests returns every request the fake has received so far, in order.
func (f *Fake) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedRequest(nil), f.requests...)
}

// KillConnectionAfter arranges for the *next* GET of rel's file to abort
// the connection after writing n bytes of the response body — simulating
// a dropped connection mid-transfer so a test can exercise Download's
// Range-resume path. One-shot: consumed by the next matching request.
func (f *Fake) KillConnectionAfter(rel string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.killAfter == nil {
		f.killAfter = map[string]int64{}
	}
	f.killAfter[rel] = n
}

// SetForceNextSinceNull makes every manifest response's next_since field
// JSON null instead of a string, for exercising the client's "null means
// end of stream" handling.
func (f *Fake) SetForceNextSinceNull(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceNextSinceNull = v
}

// SetStickyManifestPage makes every manifest request (regardless of
// since/camera/limit) receive exactly this page, with the given next
// cursor (string, or nil for JSON null) — for tests that want to force
// EachSegment's sticky-cursor guard deterministically rather than relying
// on real segments happening to collide on start_time. Each entry of
// paths becomes a minimal-but-valid segment JSON object.
func (f *Fake) SetStickyManifestPage(paths []string, next any) {
	segs := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		segs = append(segs, map[string]any{
			"camera": "front", "path": p, "url": fileURL(p),
			"size": int64(0), "mtime": "2026-01-01T00:00:00Z",
			"start_time": "2026-01-01T00:00:00", "end_time": "2026-01-01T00:01:00",
			"has_detections": false, "has_gps": false, "has_tracks": false,
			"sidecars": map[string]string{},
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stickyManifest = &stickyManifestPage{segments: segs, next: next}
}

// ForceNextRangeMismatch arranges for the *next* Range GET of rel's file
// to answer 206 with a Content-Range that does NOT start at the byte the
// client asked to resume from (it claims bogusStart instead), while still
// serving the file's full content from byte 0 — simulating a server whose
// Range support lies about (or otherwise gets wrong) where the partial
// content actually starts. A correct client must not trust the 206 status
// alone; it has to notice the mismatch and restart the download from 0
// rather than appending this response after what it already has on disk.
// One-shot: consumed by the next matching Range request.
func (f *Fake) ForceNextRangeMismatch(rel string, bogusStart int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rangeMismatch == nil {
		f.rangeMismatch = map[string]int64{}
	}
	f.rangeMismatch[rel] = bogusStart
}

// AddSegment registers a segment: writes content at dir/relPath (creating
// parent dirs) plus whichever sidecar files the has* flags request, and
// makes it show up in the manifest.
func (f *Fake) AddSegment(camera, relPath string, content []byte, startTime, endTime string, hasTimestamp, hasDetections, hasGPS, hasTracks bool) error {
	abs := filepath.Join(f.Dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		return err
	}
	base := strings.TrimSuffix(abs, ".mp4")
	write := func(want bool, suffix string) error {
		if !want {
			return nil
		}
		return os.WriteFile(base+suffix, []byte("{}"), 0o644)
	}
	if err := write(hasTimestamp, ".timestamp.json"); err != nil {
		return err
	}
	if err := write(hasDetections, ".detections.json"); err != nil {
		return err
	}
	if err := write(hasGPS, ".gps.json"); err != nil {
		return err
	}

	f.mu.Lock()
	f.segments = append(f.segments, segMeta{
		Camera: camera, Path: filepath.ToSlash(relPath),
		StartTime: startTime, EndTime: endTime,
		HasTimestamp: hasTimestamp, HasDetections: hasDetections,
		HasGPS: hasGPS, HasTracks: hasTracks,
	})
	f.mu.Unlock()
	return nil
}

// -- signature verification (independent of internal/dashcam) -----------

// localCanonicalString duplicates (deliberately, not by import) the
// canonical-string construction resources/ingest.py's canonical_string
// and internal/dashcam's client both implement, so this fake's
// verification is a genuinely separate re-derivation to check the
// client's signing against — not the same code trusting itself.
func localCanonicalString(method, requestTarget string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	s := method + "\n" + requestTarget + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
	return []byte(s)
}

// verify re-implements RequestVerifier.verify from scratch: header
// presence, key lookup, clock skew, nonce replay, then the Ed25519 check
// itself. It reads the request-target from r.RequestURI — the exact bytes
// the client put on the wire, as net/http's server captures them before
// any URL parsing/normalization — matching aiohttp's request.raw_path,
// which is what the real server signs against.
func (f *Fake) verify(r *http.Request, body []byte) (status int, code string) {
	f.mu.Lock()
	noKeys := len(f.pubKeys) == 0
	f.mu.Unlock()
	if noKeys {
		return 503, "ingest_disabled"
	}

	keyID := r.Header.Get("X-Ingest-Key-Id")
	tsRaw := r.Header.Get("X-Ingest-Timestamp")
	nonce := r.Header.Get("X-Ingest-Nonce")
	sigB64 := r.Header.Get("X-Ingest-Signature")
	if keyID == "" || tsRaw == "" || nonce == "" || sigB64 == "" {
		return 401, "missing_headers"
	}

	f.mu.Lock()
	pub, known := f.pubKeys[keyID]
	f.mu.Unlock()
	if !known {
		return 401, "unknown_key"
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return 401, "missing_headers"
	}

	now := f.now()
	skewS := int64(f.maxSkew / time.Second)
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

	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return 401, "bad_signature"
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneNoncesLocked(now.Unix(), skewS)
	if _, seen := f.nonces[nonce]; seen {
		return 401, "replay"
	}

	msg := localCanonicalString(r.Method, r.RequestURI, ts, nonce, body)
	if !ed25519.Verify(pub, msg, sig) {
		return 401, "bad_signature"
	}

	if f.nonces == nil {
		f.nonces = map[string]int64{}
	}
	f.nonces[nonce] = now.Unix()
	return 200, ""
}

func (f *Fake) pruneNoncesLocked(nowUnix, skewS int64) {
	if f.nonces == nil {
		return
	}
	cutoff := nowUnix - 2*skewS
	for n, t := range f.nonces {
		if t < cutoff {
			delete(f.nonces, n)
		}
	}
}

// -- middleware ----------------------------------------------------------

func (f *Fake) wrap(h func(w http.ResponseWriter, r *http.Request, body []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, RecordedRequest{
			Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
			Nonce: r.Header.Get("X-Ingest-Nonce"), Ts: r.Header.Get("X-Ingest-Timestamp"),
		})
		f.mu.Unlock()

		status, code := f.verify(r, body)
		if status != http.StatusOK {
			f.writeError(w, status, code)
			return
		}
		h(w, r, body)
	}
}

// serverTimeSeconds is server_time the way Python's time.time() produces
// it: unix seconds with a fractional part, not a bare integer.
func (f *Fake) serverTimeSeconds() float64 {
	return float64(f.now().UnixNano()) / 1e9
}

func (f *Fake) writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{
		"error":       code,
		"server_time": f.serverTimeSeconds(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// -- manifest --------------------------------------------------------------

func (f *Fake) handleManifest(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	sticky := f.stickyManifest
	forceNullSticky := f.forceNextSinceNull
	f.mu.Unlock()
	if sticky != nil {
		var next any = sticky.next
		if forceNullSticky {
			next = nil
		}
		writeJSON(w, 200, map[string]any{
			"segments": sticky.segments, "count": len(sticky.segments), "next_since": next,
		})
		return
	}

	q := r.URL.Query()
	since := q.Get("since")
	camera := q.Get("camera")
	limit := 500
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			f.writeError(w, 400, "invalid_limit")
			return
		}
		limit = n
	}

	f.mu.Lock()
	all := append([]segMeta(nil), f.segments...)
	f.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].StartTime < all[j].StartTime })

	var filtered []segMeta
	for _, s := range all {
		if camera != "" && s.Camera != camera {
			continue
		}
		if since != "" && s.StartTime <= since {
			continue
		}
		filtered = append(filtered, s)
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	segs := make([]map[string]any, 0, len(filtered))
	for _, s := range filtered {
		abs := filepath.Join(f.Dir, filepath.FromSlash(s.Path))
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(s.Path, ".mp4")
		sidecars := map[string]string{}
		for _, kv := range []struct{ kind, suffix string }{
			{"timestamp", ".timestamp.json"},
			{"detections", ".detections.json"},
			{"gps", ".gps.json"},
		} {
			sideRel := base + kv.suffix
			if _, err := os.Stat(filepath.Join(f.Dir, filepath.FromSlash(sideRel))); err == nil {
				sidecars[kv.kind] = fileURL(sideRel)
			}
		}
		// has_detections/has_gps mirror the *live* presence of their
		// sidecar file (sidecars["detections"]/["gps"] above), not
		// AddSegment's original hasDetections/hasGPS flags — matching the
		// real recorder, whose has_detections column flips 0->1 only once
		// analysis actually finishes and writes the sidecar (see
		// resources/metadata.py), independent of when the segment itself
		// was first recorded/listed. A test simulating a "late sidecar"
		// (final-fix-wave item 1) writes the .detections.json/.gps.json
		// file directly rather than re-calling AddSegment, so this must
		// recompute from disk on every manifest request rather than
		// trusting the segMeta snapshot taken at AddSegment time.
		_, hasDet := sidecars["detections"]
		_, hasGPS := sidecars["gps"]
		segs = append(segs, map[string]any{
			"camera": s.Camera, "path": s.Path, "url": fileURL(s.Path),
			"size": info.Size(), "mtime": info.ModTime().UTC().Format(time.RFC3339),
			"start_time": s.StartTime, "end_time": s.EndTime,
			"has_detections": hasDet, "has_gps": hasGPS, "has_tracks": s.HasTracks,
			"sidecars": sidecars,
		})
	}

	var nextSince any
	f.mu.Lock()
	forceNull := f.forceNextSinceNull
	f.mu.Unlock()
	switch {
	case forceNull:
		nextSince = nil
	case len(filtered) > 0:
		nextSince = filtered[len(filtered)-1].StartTime
	default:
		if since == "" {
			nextSince = nil
		} else {
			nextSince = since
		}
	}
	writeJSON(w, 200, map[string]any{
		"segments": segs, "count": len(segs), "next_since": nextSince,
	})
}

func fileURL(rel string) string {
	v := url.Values{}
	v.Set("path", rel)
	return "/api/ingest/file?" + v.Encode()
}

// -- file / download -------------------------------------------------------

func (f *Fake) resolvePath(rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	base, err := filepath.Abs(f.Dir)
	if err != nil {
		return "", false
	}
	abs := filepath.Join(base, clean)
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

func (f *Fake) handleFile(w http.ResponseWriter, r *http.Request, _ []byte) {
	rel := r.URL.Query().Get("path")
	abs, ok := f.resolvePath(rel)
	if !ok {
		f.writeError(w, 400, "invalid_path")
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		f.writeError(w, 404, "not_found")
		return
	}

	f.mu.Lock()
	if f.inFlight >= f.MaxConcurrent {
		f.mu.Unlock()
		f.writeError(w, 429, "too_many_downloads")
		return
	}
	f.inFlight++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	file, err := os.Open(abs)
	if err != nil {
		f.writeError(w, 404, "not_found")
		return
	}
	defer file.Close()

	size := info.Size()
	start, end := int64(0), size-1
	status := http.StatusOK
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		s, e, ok := parseRange(rangeHeader, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, status = s, e, http.StatusPartialContent
	}
	length := end - start + 1

	f.mu.Lock()
	bogusStart, mismatch := int64(-1), false
	if n, ok := f.rangeMismatch[rel]; ok && status == http.StatusPartialContent {
		bogusStart, mismatch = n, true
		delete(f.rangeMismatch, rel)
	}
	f.mu.Unlock()
	if mismatch {
		// Lie about where this 206 actually starts, but serve the whole
		// file body from 0 — see ForceNextRangeMismatch's doc comment.
		start, end = 0, size-1
		length = size
	}

	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Accept-Ranges", "bytes")
	if status == http.StatusPartialContent {
		rangeStart := start
		if mismatch {
			rangeStart = bogusStart
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, end, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)

	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return
	}

	f.mu.Lock()
	killAfter, hasKill := int64(-1), false
	if n, ok := f.killAfter[rel]; ok {
		killAfter, hasKill = n, true
		delete(f.killAfter, rel)
	}
	f.mu.Unlock()

	buf := make([]byte, 32*1024)
	var written int64
	for written < length {
		toRead := int64(len(buf))
		if remain := length - written; remain < toRead {
			toRead = remain
		}
		n, rerr := file.Read(buf[:toRead])
		if n > 0 {
			if hasKill && written+int64(n) > killAfter {
				n = int(killAfter - written)
				if n > 0 {
					w.Write(buf[:n])
					if fl, ok := w.(http.Flusher); ok {
						fl.Flush()
					}
				}
				panic(http.ErrAbortHandler)
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return
		}
	}
}

func parseRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, prefix)
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	startS, endS := spec[:dash], spec[dash+1:]
	if startS == "" {
		n, err := strconv.ParseInt(endS, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start = size - n
		if start < 0 {
			start = 0
		}
		end = size - 1
	} else {
		s, err := strconv.ParseInt(startS, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start = s
		if endS == "" {
			end = size - 1
		} else {
			e, err := strconv.ParseInt(endS, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			end = e
		}
	}
	if start > end || start >= size || end >= size || start < 0 {
		return 0, 0, false
	}
	return start, end, true
}

// -- delete ------------------------------------------------------------

// ForceDeleteErrorForPath arranges for the *next* POST /api/ingest/delete
// call whose paths include rel to fail outright (the whole call, not just
// rel's entry) with the given HTTP status/code, instead of being processed
// normally — for exercising a caller's handling of a batched delete call
// that errors partway through a multi-batch run (e.g. dashcam_cleanup job
// must not lose its record of earlier, already-succeeded batches just
// because a later one failed). One-shot: consumed by the next matching
// call.
func (f *Fake) ForceDeleteErrorForPath(rel string, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceDeleteError == nil {
		f.forceDeleteError = map[string]forcedError{}
	}
	f.forceDeleteError[rel] = forcedError{status: status, code: code}
}

func (f *Fake) handleDelete(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Paths  []string          `json:"paths"`
		SHA256 map[string]string `json:"sha256"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeError(w, 400, "invalid_json")
		return
	}

	f.mu.Lock()
	for _, rel := range req.Paths {
		if fe, ok := f.forceDeleteError[rel]; ok {
			delete(f.forceDeleteError, rel)
			f.mu.Unlock()
			f.writeError(w, fe.status, fe.code)
			return
		}
	}
	f.mu.Unlock()

	deleted := []string{}
	skipped := []map[string]any{}

	for _, rel := range req.Paths {
		abs, ok := f.resolvePath(rel)
		if !ok {
			skipped = append(skipped, map[string]any{"path": rel, "reason": "invalid_path"})
			continue
		}
		f.mu.Lock()
		idx := -1
		for i, s := range f.segments {
			if s.Path == rel {
				idx = i
				break
			}
		}
		f.mu.Unlock()
		if idx < 0 {
			skipped = append(skipped, map[string]any{"path": rel, "reason": "open_or_unknown"})
			continue
		}
		if want, has := req.SHA256[rel]; has {
			got, _, err := hashFile(abs)
			if err != nil || got != want {
				skipped = append(skipped, map[string]any{"path": rel, "reason": "checksum_mismatch"})
				continue
			}
		}
		_ = os.Remove(abs)
		f.mu.Lock()
		f.segments = append(f.segments[:idx], f.segments[idx+1:]...)
		f.mu.Unlock()
		deleted = append(deleted, rel)
	}

	writeJSON(w, 200, map[string]any{"deleted": deleted, "skipped": skipped})
}

// ForceReleaseError arranges for the next n POST
// /api/ingest/maintenance/release calls to answer with the given HTTP
// status/code instead of actually releasing the lease — for exercising a
// caller's retry-with-backoff handling of a 409 reprocess_running the
// recorder can answer release with while its own background thread still
// briefly holds the lease busy.
func (f *Fake) ForceReleaseError(n int, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceReleaseErrorRemaining = n
	f.forceReleaseErrorSpec = forcedError{status: status, code: code}
}

// -- maintenance ---------------------------------------------------------

func (f *Fake) handleMaintenanceRoot(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method == http.MethodGet {
		f.handleMaintenanceStatus(w, r, body)
		return
	}
	f.handleMaintenanceAcquire(w, r, body)
}

func (f *Fake) clampTTL(ttlS int64) time.Duration {
	ttl := f.DefaultTTL
	if ttlS > 0 {
		ttl = time.Duration(ttlS) * time.Second
	}
	if ttl > f.MaxTTL {
		ttl = f.MaxTTL
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	return ttl
}

func (f *Fake) handleMaintenanceAcquire(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		TTLS   *int64 `json:"ttl_s"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.nowFn()
	if f.lease != nil && f.lease.ExpiresAt.After(now) {
		writeJSON(w, 409, map[string]any{"error": "lease_active", "server_time": float64(now.UnixNano()) / 1e9})
		return
	}
	ttlS := int64(0)
	if req.TTLS != nil {
		ttlS = *req.TTLS
	}
	ttl := f.clampTTL(ttlS)
	expires := now.Add(ttl)
	f.lease = &leaseState{LeaseID: newLeaseID(), ExpiresAt: expires, Reason: req.Reason}
	// acquire_lease's remaining_s is the clamped ttl (an int), unlike
	// lease_status's remaining_s (a float — see handleMaintenanceStatus).
	writeJSON(w, 200, map[string]any{
		"lease_id": f.lease.LeaseID, "expires_at": expires.UTC().Format(time.RFC3339),
		"remaining_s": int64(ttl / time.Second), "reason": req.Reason,
		"recording": false, "inference": false,
	})
}

func (f *Fake) handleRenew(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		LeaseID string `json:"lease_id"`
		TTLS    *int64 `json:"ttl_s"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.nowFn()
	if f.lease == nil {
		writeJSON(w, 404, map[string]any{"error": "no_lease", "server_time": float64(now.UnixNano()) / 1e9})
		return
	}
	if f.lease.LeaseID != req.LeaseID {
		writeJSON(w, 404, map[string]any{"error": "unknown_lease", "server_time": float64(now.UnixNano()) / 1e9})
		return
	}
	ttlS := int64(0)
	if req.TTLS != nil {
		ttlS = *req.TTLS
	}
	ttl := f.clampTTL(ttlS)
	f.lease.ExpiresAt = now.Add(ttl)
	writeJSON(w, 200, map[string]any{
		"lease_id": f.lease.LeaseID, "expires_at": f.lease.ExpiresAt.UTC().Format(time.RFC3339),
		"remaining_s": int64(ttl / time.Second), "reason": f.lease.Reason,
	})
}

func (f *Fake) handleRelease(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		LeaseID string `json:"lease_id"`
		Force   bool   `json:"force"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.nowFn()
	if f.forceReleaseErrorRemaining > 0 {
		f.forceReleaseErrorRemaining--
		spec := f.forceReleaseErrorSpec
		writeJSON(w, spec.status, map[string]any{"error": spec.code, "server_time": float64(now.UnixNano()) / 1e9})
		return
	}
	if f.lease == nil {
		writeJSON(w, 404, map[string]any{"error": "no_lease", "server_time": float64(now.UnixNano()) / 1e9})
		return
	}
	if f.lease.LeaseID != req.LeaseID {
		writeJSON(w, 404, map[string]any{"error": "unknown_lease", "server_time": float64(now.UnixNano()) / 1e9})
		return
	}
	f.lease = nil
	writeJSON(w, 200, map[string]any{"released": true})
}

func (f *Fake) handleMaintenanceStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.nowFn()
	if f.lease == nil || !f.lease.ExpiresAt.After(now) {
		writeJSON(w, 200, map[string]any{
			"active": false, "lease_id": nil, "expires_at": nil,
			"remaining_s": float64(0), "reason": nil,
		})
		return
	}
	// lease_status() computes remaining_s as expires_at - time.time(): a
	// Python float, not an int — kept that way here on purpose.
	writeJSON(w, 200, map[string]any{
		"active": true, "lease_id": f.lease.LeaseID,
		"expires_at":  f.lease.ExpiresAt.UTC().Format(time.RFC3339),
		"remaining_s": f.lease.ExpiresAt.Sub(now).Seconds(),
		"reason":      f.lease.Reason,
	})
}

// -- reprocess -------------------------------------------------------------

// ForceReprocessStatusError arranges for the next n GET
// /api/ingest/reprocess/status calls to answer with the given HTTP
// status/code instead of the normal reprocess-status response — for
// exercising a caller's tolerance of a transient status-poll failure
// (e.g. dashcam_reprocess's consecutive-failure retry) without tearing
// the run's actual progress down.
func (f *Fake) ForceReprocessStatusError(n int, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceStatusErrorRemaining = n
	f.forceStatusErrorSpec = forcedError{status: status, code: code}
}

func (f *Fake) handleReprocess(w http.ResponseWriter, r *http.Request, body []byte) {
	f.mu.Lock()
	now := f.nowFn()
	active := f.lease != nil && f.lease.ExpiresAt.After(now)
	f.mu.Unlock()
	if !active {
		writeJSON(w, 409, map[string]any{"error": "maintenance_required", "server_time": float64(now.UnixNano()) / 1e9})
		return
	}

	var req struct {
		Camera string `json:"camera"`
		Date   string `json:"date"`
	}
	_ = json.Unmarshal(body, &req)
	camera, date := req.Camera, req.Date
	if camera == "" {
		camera = "all"
	}
	if date == "" {
		date = "today"
	}

	f.mu.Lock()
	// state="starting" synchronously, matching resources/dashboard.py's
	// ReprocessHandler: it sets self._reprocess_status (state='starting')
	// before this POST returns, then a background thread walks it through
	// 'running' to a terminal 'done'/'error' — see reprocessState's doc
	// comment. A fresh POST always overwrites whatever terminal state a
	// previous run left behind, same as the real recorder.
	f.reprocess = &reprocessState{state: "starting", camera: camera, date: date, engine: "fake", total: f.ReprocessTicks, err: f.ReprocessError}
	f.mu.Unlock()

	writeJSON(w, 200, map[string]any{"status": "started", "camera": camera, "date": date, "engine": "fake"})
}

// reprocessStatusJSON builds one GET /api/ingest/reprocess/status response
// for state/rp, with errMsg carried only in the terminal "error" case —
// factored out since every one of reprocessState's states below shares the
// same field shape (tracking is a string label, never a bool — see
// dashboard.py).
func (f *Fake) reprocessStatusJSON(state string, rp *reprocessState, processed int, errMsg string) map[string]any {
	return map[string]any{
		"state": state, "camera": rp.camera, "date": rp.date, "engine": rp.engine,
		"total_segments": rp.total, "processed": processed, "tracking": f.ReprocessTracking,
		"current_segment": "", "error": errMsg, "started_at": "",
		"total_objects": 0, "total_plates": 0, "total_vehicles": 0, "total_track_events": 0,
	}
}

func (f *Fake) handleReprocessStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceStatusErrorRemaining > 0 {
		f.forceStatusErrorRemaining--
		spec := f.forceStatusErrorSpec
		writeJSON(w, spec.status, map[string]any{"error": spec.code, "server_time": float64(f.nowFn().UnixNano()) / 1e9})
		return
	}
	if f.reprocess == nil {
		writeJSON(w, 200, map[string]any{"state": "idle"})
		return
	}
	rp := f.reprocess

	switch rp.state {
	case "starting":
		writeJSON(w, 200, f.reprocessStatusJSON("starting", rp, 0, ""))
		// The real recorder's background thread would already be under
		// way by the time a caller's *next* status poll lands; a single
		// "starting" snapshot is enough to exercise that this fake (like
		// the real recorder) never reports "idle" again once a run has
		// started.
		rp.state = "running"
	case "running":
		writeJSON(w, 200, f.reprocessStatusJSON("running", rp, rp.processed, ""))
		rp.processed++
		if rp.processed >= rp.total {
			if rp.err != "" {
				rp.state = "error"
			} else {
				rp.state = "done"
			}
		}
	case "done":
		// Terminal: stays "done" (see reprocessState's doc comment) until
		// the next POST /api/ingest/reprocess overwrites f.reprocess.
		writeJSON(w, 200, f.reprocessStatusJSON("done", rp, rp.total, ""))
	case "error":
		writeJSON(w, 200, f.reprocessStatusJSON("error", rp, rp.processed, rp.err))
	}
}

// -- misc --------------------------------------------------------------

var leaseCounter int64

func newLeaseID() string {
	leaseCounter++
	return fmt.Sprintf("lease-%d-%d", time.Now().UnixNano(), leaseCounter)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
