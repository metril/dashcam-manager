package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// maxSourcesBodyBytes caps POST /api/sources and /api/sources/scan request
// bodies — both are tiny fixed-shape objects.
const maxSourcesBodyBytes = 16 * 1024

// maxActiveScanJobsScanned bounds how many queued/running jobs
// activeScanJobsBySource inspects when looking for in-flight scans, so a
// server that has accumulated a lot of unrelated queued work doesn't make
// GET /api/sources unboundedly slow.
const maxActiveScanJobsScanned = 200

// sourceView is the JSON shape returned for a source, including whether a
// scan job is currently queued or running for it (M7: this used to be
// tracked by an in-memory scanRunner; now it's derived from the job queue,
// which is the durable source of truth and survives a server restart).
type sourceView struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	RootPath   string  `json:"root_path"`
	Label      string  `json:"label"`
	LastScanAt *string `json:"last_scan_at"`
	Status     string  `json:"status"`
	Scanning   bool    `json:"scanning"`
	ScanJobID  *int64  `json:"scan_job_id,omitempty"`
}

func toSourceView(s db.Source, activeJob *jobs.Job) sourceView {
	v := sourceView{
		ID: s.ID, Kind: s.Kind, RootPath: s.RootPath, Label: s.Label,
		LastScanAt: s.LastScanAt, Status: s.Status,
	}
	if activeJob != nil {
		v.Scanning = true
		id := activeJob.ID
		v.ScanJobID = &id
	}
	return v
}

// activeJobsBySourceOfType returns, for every source with a queued or
// running job whose type is in types, that job (most recently enqueued
// one wins if somehow more than one matching job exists for the same
// source). Every job type this is used with ("scan", "ingest",
// "cleanup") shares a params shape with a top-level "source_id" field,
// so a single anonymous struct can decode all of them.
func activeJobsBySourceOfType(ctx context.Context, q *jobs.Queue, types map[string]bool) (map[int64]*jobs.Job, error) {
	out := map[int64]*jobs.Job{}
	for _, state := range []string{"queued", "running"} {
		list, err := q.List(ctx, state, maxActiveScanJobsScanned)
		if err != nil {
			return nil, err
		}
		for _, j := range list {
			if !types[j.Type] {
				continue
			}
			var params struct {
				SourceID int64 `json:"source_id"`
			}
			if err := json.Unmarshal(j.Params, &params); err != nil {
				continue
			}
			out[params.SourceID] = j
		}
	}
	return out, nil
}

// activeScanJobsBySource returns, for every source with a queued or
// running "scan" job, that job (most recently enqueued one wins if
// somehow more than one exists for the same source).
func activeScanJobsBySource(ctx context.Context, q *jobs.Queue) (map[int64]*jobs.Job, error) {
	return activeJobsBySourceOfType(ctx, q, map[string]bool{"scan": true})
}

// handleSourcesGet lists every configured source with its live scan status.
func handleSourcesGet(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := store.SourcesList(r.Context())
		if err != nil {
			writeInternalError(w, "sources list", err)
			return
		}
		active, err := activeScanJobsBySource(r.Context(), q)
		if err != nil {
			writeInternalError(w, "active scan jobs", err)
			return
		}

		views := make([]sourceView, 0, len(sources))
		for _, s := range sources {
			views = append(views, toSourceView(s, active[s.ID]))
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
		writeJSON(w, http.StatusCreated, toSourceView(s, nil))
	}
}

// enqueueScanMu serializes the active-scan check and the enqueue so
// concurrent callers cannot both enqueue a scan for the same source.
var enqueueScanMu sync.Mutex

// enqueueScan enqueues a "scan" job for sourceID unless one is already
// queued or running, in which case that job is returned with queued=false.
func enqueueScan(ctx context.Context, q *jobs.Queue, sourceID int64, full, force bool) (job *jobs.Job, queued bool, err error) {
	enqueueScanMu.Lock()
	defer enqueueScanMu.Unlock()
	active, err := activeScanJobsBySource(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if existing, ok := active[sourceID]; ok {
		return existing, false, nil
	}
	job, err = q.Enqueue(ctx, "scan", scanJobParams{SourceID: sourceID, Full: full, Force: force}, 0)
	return job, err == nil, err
}

// handleSourcesScan enqueues a "scan" job for a source, per {source_id,
// full, force}, and returns it immediately — the scan itself runs
// asynchronously on the job queue's worker pool (see jobs_runtime.go's
// scanJobHandler). force bypasses scan.Scan's empty-root guard (see its
// doc comment) — the source scan UI should surface it as an explicit
// checkbox with a warning, not a default. If a scan for this source is
// already queued or running, that job is returned instead of enqueueing a
// duplicate.
func handleSourcesScan(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSourcesBodyBytes)

		var body struct {
			SourceID int64 `json:"source_id"`
			Full     bool  `json:"full"`
			Force    bool  `json:"force"`
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

		job, queued, err := enqueueScan(r.Context(), q, src.ID, body.Full, body.Force)
		if err != nil {
			writeInternalError(w, "enqueue scan job", err)
			return
		}
		if !queued {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "already_running", "job": job})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}
