package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/scan"
)

// maxSourcesBodyBytes caps POST /api/sources and /api/sources/scan request
// bodies — both are tiny fixed-shape objects.
const maxSourcesBodyBytes = 16 * 1024

// maxScanHistory bounds scanRunner.last (and thus indirectly how long a
// source can go without a poll before its result is evicted) so a server
// with many sources scanned over a long uptime doesn't grow this map
// forever. running isn't bounded the same way — it only ever holds entries
// for sources actively scanning right now (finish deletes them), which is
// inherently small.
const maxScanHistory = 50

// scanRunner tracks in-memory, best-effort status for the (at most one at a
// time, per source) background goroutine a POST /api/sources/scan starts.
// This is intentionally not persisted: a server restart mid-scan just means
// the next scan starts fresh, and M7's real job queue replaces this.
type scanRunner struct {
	mu      sync.Mutex
	running map[int64]bool
	last    map[int64]scanResult
	order   []int64 // insertion order of `last` keys, oldest first, for eviction
}

type scanResult struct {
	Stats      scan.Stats `json:"stats"`
	Error      string     `json:"error,omitempty"`
	FinishedAt string     `json:"finished_at"`
}

var runner = &scanRunner{running: map[int64]bool{}, last: map[int64]scanResult{}}

func (sr *scanRunner) tryStart(sourceID int64) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.running[sourceID] {
		return false
	}
	sr.running[sourceID] = true
	return true
}

func (sr *scanRunner) finish(sourceID int64, res scanResult) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	delete(sr.running, sourceID)

	if _, exists := sr.last[sourceID]; !exists {
		sr.order = append(sr.order, sourceID)
	}
	sr.last[sourceID] = res
	for len(sr.order) > maxScanHistory {
		oldest := sr.order[0]
		sr.order = sr.order[1:]
		delete(sr.last, oldest)
	}
}

func (sr *scanRunner) status(sourceID int64) (running bool, last scanResult, ok bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	last, ok = sr.last[sourceID]
	return sr.running[sourceID], last, ok
}

// sourceView is the JSON shape returned for a source, including the
// in-memory scan status.
type sourceView struct {
	ID         int64       `json:"id"`
	Kind       string      `json:"kind"`
	RootPath   string      `json:"root_path"`
	Label      string      `json:"label"`
	LastScanAt *string     `json:"last_scan_at"`
	Status     string      `json:"status"`
	Scanning   bool        `json:"scanning"`
	LastScan   *scanResult `json:"last_scan,omitempty"`
}

func toSourceView(s db.Source) sourceView {
	running, last, ok := runner.status(s.ID)
	v := sourceView{
		ID: s.ID, Kind: s.Kind, RootPath: s.RootPath, Label: s.Label,
		LastScanAt: s.LastScanAt, Status: s.Status, Scanning: running,
	}
	if ok {
		v.LastScan = &last
	}
	return v
}

// handleSourcesGet lists every configured source with its live scan status.
func handleSourcesGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := store.SourcesList(r.Context())
		if err != nil {
			writeInternalError(w, "sources list", err)
			return
		}
		views := make([]sourceView, 0, len(sources))
		for _, s := range sources {
			views = append(views, toSourceView(s))
		}
		writeJSON(w, http.StatusOK, views)
	}
}

// handleSourcesPost creates a new source ({kind, root_path, label}).
func handleSourcesPost(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSourcesBodyBytes)

		var body struct {
			Kind     string `json:"kind"`
			RootPath string `json:"root_path"`
			Label    string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.Kind != "ssd" && body.Kind != "share" {
			writeJSONError(w, http.StatusBadRequest, `kind must be "ssd" or "share"`)
			return
		}
		if body.RootPath == "" {
			writeJSONError(w, http.StatusBadRequest, "root_path is required")
			return
		}
		if !filepath.IsAbs(body.RootPath) || filepath.Clean(body.RootPath) != body.RootPath {
			writeJSONError(w, http.StatusBadRequest, "root_path must be an absolute, cleaned path (no trailing slash, \".\", or \"..\")")
			return
		}
		if info, err := os.Stat(body.RootPath); err != nil || !info.IsDir() {
			writeJSONError(w, http.StatusBadRequest, "root_path must be an existing directory")
			return
		}

		id, err := store.SourceCreate(r.Context(), body.Kind, body.RootPath, body.Label)
		if err != nil {
			writeJSONError(w, http.StatusConflict, "could not create source (duplicate root_path?): "+err.Error())
			return
		}
		s, err := store.SourceGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "source get after create", err)
			return
		}
		writeJSON(w, http.StatusCreated, toSourceView(s))
	}
}

// handleSourcesScan starts (or reports already-running for) a scan of one
// source, per {source_id, full}. The scan itself runs in a background
// goroutine — this always returns immediately; poll GET /api/sources for
// status. Runs synchronously would block the request for the ~minutes a
// full 39k-segment scan can take, so this is deliberately fire-and-forget
// with in-memory status (see scanRunner) until M7 adds a real job queue.
// appCtx is the server's lifetime context (cancelled on shutdown) — the
// scan goroutine derives from it, not from the triggering request, so it
// keeps running after the HTTP response is sent but still stops on
// shutdown instead of leaking past the process's intent to stop.
func handleSourcesScan(store *db.DB, appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSourcesBodyBytes)

		var body struct {
			SourceID int64 `json:"source_id"`
			Full     bool  `json:"full"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.SourceID == 0 {
			writeJSONError(w, http.StatusBadRequest, "source_id is required")
			return
		}

		src, err := store.SourceGet(r.Context(), body.SourceID)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "source not found")
			return
		}
		if err != nil {
			writeInternalError(w, "source get", err)
			return
		}

		if !runner.tryStart(src.ID) {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "already_running", "source_id": src.ID})
			return
		}

		if err := store.SourceSetStatus(context.Background(), src.ID, "scanning"); err != nil {
			runner.finish(src.ID, scanResult{Error: err.Error()})
			writeInternalError(w, "source set status", err)
			return
		}

		go runScan(store, src, body.Full, appCtx)

		writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "source_id": src.ID})
	}
}

// runScan runs a full scan pass (Scan then, best-effort, legacy DB
// enrichment) for src and records the outcome in the in-memory scanRunner
// and the source's status/last_scan_at columns. Derives from appCtx (the
// server's lifetime context, not the triggering HTTP request) with a
// generous timeout since a full 39k-segment scan can take minutes; a server
// shutdown cancels appCtx and so cancels any scan still running.
func runScan(store *db.DB, src db.Source, full bool, appCtx context.Context) {
	ctx, cancel := context.WithTimeout(appCtx, 2*time.Hour)
	defer cancel()

	stats, err := scan.Scan(ctx, store, src.ID, src.RootPath, scan.Opts{Full: full})
	if err == nil {
		if _, legacyErr := scan.EnrichFromLegacyDB(ctx, store, src.ID, src.RootPath); legacyErr != nil {
			// Best-effort by design; log via the scan package's own
			// logging and don't fail the scan over it.
			_ = legacyErr
		}
	}

	res := scanResult{Stats: stats, FinishedAt: time.Now().UTC().Format(time.RFC3339)}
	status := "idle"
	if err != nil {
		res.Error = err.Error()
		status = "error"
	}
	runner.finish(src.ID, res)
	_ = store.SourceSetLastScanAt(context.Background(), src.ID, res.FinishedAt, status)
}
