// Package dashcamtest is a reusable fake for resources/ingest_server.py's
// HTTP surface, built on internal/dashcam.Verifier so a test gets the same
// signature/replay/clock-skew enforcement a real recorder would apply.
// It's exported (not a _test.go file) so both internal/dashcam's own
// tests and any later package driving the client (T3) can share one fake
// rather than each hand-rolling httptest handlers.
package dashcamtest

import (
	"crypto/ed25519"
	"crypto/sha256"
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

	"dashcam/internal/dashcam"
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
	// "running" for before flipping to "idle".
	ReprocessTicks int

	verifier *dashcam.Verifier

	mu        sync.Mutex
	nowFn     func() time.Time
	segments  []segMeta
	inFlight  int
	lease     *leaseState
	reprocess *reprocessState
	requests  []RecordedRequest
	killAfter map[string]int64
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

type reprocessState struct {
	camera, date, engine string
	total, processed     int
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
		Dir:            dir,
		MaxConcurrent:  2,
		DefaultTTL:     900 * time.Second,
		MaxTTL:         3600 * time.Second,
		ReprocessTicks: 2,
		nowFn:          time.Now,
	}
	f.verifier = &dashcam.Verifier{
		PublicKeys: map[string]ed25519.PublicKey{},
		MaxSkew:    300 * time.Second,
		Now:        func() time.Time { return f.now() },
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
	f.verifier.PublicKeys[keyID] = pub
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

		status, code := f.verifier.Verify(r, body)
		if status != http.StatusOK {
			f.writeError(w, status, code)
			return
		}
		h(w, r, body)
	}
}

func (f *Fake) writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, dashcam.ErrorBody{Error: code, ServerTime: f.now().Unix()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// -- manifest --------------------------------------------------------------

func (f *Fake) handleManifest(w http.ResponseWriter, r *http.Request, _ []byte) {
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

	segs := make([]dashcam.Segment, 0, len(filtered))
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
		segs = append(segs, dashcam.Segment{
			Camera: s.Camera, Path: s.Path, URL: fileURL(s.Path),
			Size: info.Size(), MTime: info.ModTime().UTC().Format(time.RFC3339),
			StartTime: s.StartTime, EndTime: s.EndTime,
			HasDetections: s.HasDetections, HasGPS: s.HasGPS, HasTracks: s.HasTracks,
			Sidecars: sidecars,
		})
	}

	nextSince := since
	if len(filtered) > 0 {
		nextSince = filtered[len(filtered)-1].StartTime
	}
	writeJSON(w, 200, dashcam.ManifestPage{Segments: segs, Count: len(segs), NextSince: nextSince})
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

	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Accept-Ranges", "bytes")
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
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

func (f *Fake) handleDelete(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Paths  []string          `json:"paths"`
		SHA256 map[string]string `json:"sha256"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.writeError(w, 400, "invalid_json")
		return
	}

	deleted := []string{}
	type skip struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	skipped := []skip{}

	for _, rel := range req.Paths {
		abs, ok := f.resolvePath(rel)
		if !ok {
			skipped = append(skipped, skip{rel, "invalid_path"})
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
			skipped = append(skipped, skip{rel, "open_or_unknown"})
			continue
		}
		if want, has := req.SHA256[rel]; has {
			got, _, err := hashFile(abs)
			if err != nil || got != want {
				skipped = append(skipped, skip{rel, "checksum_mismatch"})
				continue
			}
		}
		_ = os.Remove(abs)
		f.mu.Lock()
		f.segments = append(f.segments[:idx], f.segments[idx+1:]...)
		f.mu.Unlock()
		deleted = append(deleted, rel)
	}

	writeJSON(w, 200, struct {
		Deleted []string `json:"deleted"`
		Skipped []skip   `json:"skipped"`
	}{deleted, skipped})
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
		writeJSON(w, 409, dashcam.ErrorBody{Error: "lease_active", ServerTime: now.Unix()})
		return
	}
	ttlS := int64(0)
	if req.TTLS != nil {
		ttlS = *req.TTLS
	}
	ttl := f.clampTTL(ttlS)
	expires := now.Add(ttl)
	f.lease = &leaseState{LeaseID: newLeaseID(), ExpiresAt: expires, Reason: req.Reason}
	writeJSON(w, 200, dashcam.Lease{
		LeaseID: f.lease.LeaseID, ExpiresAt: expires.UTC().Format(time.RFC3339),
		RemainingS: int64(ttl / time.Second), Reason: req.Reason,
		Recording: false, Inference: false,
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
		writeJSON(w, 404, dashcam.ErrorBody{Error: "no_lease", ServerTime: now.Unix()})
		return
	}
	if f.lease.LeaseID != req.LeaseID {
		writeJSON(w, 404, dashcam.ErrorBody{Error: "unknown_lease", ServerTime: now.Unix()})
		return
	}
	ttlS := int64(0)
	if req.TTLS != nil {
		ttlS = *req.TTLS
	}
	ttl := f.clampTTL(ttlS)
	f.lease.ExpiresAt = now.Add(ttl)
	writeJSON(w, 200, dashcam.Lease{
		LeaseID: f.lease.LeaseID, ExpiresAt: f.lease.ExpiresAt.UTC().Format(time.RFC3339),
		RemainingS: int64(ttl / time.Second), Reason: f.lease.Reason,
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
	if f.lease == nil {
		writeJSON(w, 404, dashcam.ErrorBody{Error: "no_lease", ServerTime: now.Unix()})
		return
	}
	if f.lease.LeaseID != req.LeaseID {
		writeJSON(w, 404, dashcam.ErrorBody{Error: "unknown_lease", ServerTime: now.Unix()})
		return
	}
	f.lease = nil
	writeJSON(w, 200, struct {
		Released bool `json:"released"`
	}{true})
}

func (f *Fake) handleMaintenanceStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.nowFn()
	if f.lease == nil || !f.lease.ExpiresAt.After(now) {
		writeJSON(w, 200, dashcam.LeaseStatus{Active: false})
		return
	}
	writeJSON(w, 200, dashcam.LeaseStatus{
		Active: true, LeaseID: f.lease.LeaseID,
		ExpiresAt:  f.lease.ExpiresAt.UTC().Format(time.RFC3339),
		RemainingS: int64(f.lease.ExpiresAt.Sub(now) / time.Second),
		Reason:     f.lease.Reason,
	})
}

// -- reprocess -------------------------------------------------------------

func (f *Fake) handleReprocess(w http.ResponseWriter, r *http.Request, body []byte) {
	f.mu.Lock()
	now := f.nowFn()
	active := f.lease != nil && f.lease.ExpiresAt.After(now)
	f.mu.Unlock()
	if !active {
		writeJSON(w, 409, dashcam.ErrorBody{Error: "maintenance_required", ServerTime: now.Unix()})
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
	f.reprocess = &reprocessState{camera: camera, date: date, engine: "fake", total: f.ReprocessTicks}
	f.mu.Unlock()

	writeJSON(w, 200, dashcam.ReprocessStarted{Status: "started", Camera: camera, Date: date, Engine: "fake"})
}

func (f *Fake) handleReprocessStatus(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reprocess == nil {
		writeJSON(w, 200, dashcam.ReprocessStatus{State: "idle"})
		return
	}
	rp := f.reprocess
	writeJSON(w, 200, dashcam.ReprocessStatus{
		State: "running", Camera: rp.camera, Date: rp.date, Engine: rp.engine,
		TotalSegments: rp.total, Processed: rp.processed, Tracking: true,
	})
	rp.processed++
	if rp.processed >= rp.total {
		f.reprocess = nil
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
