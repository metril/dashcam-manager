package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/share"
)

// maxStoragesBodyBytes caps POST/PATCH /api/storages request bodies — same
// small fixed-shape object as /api/shares (see maxSharesBodyBytes).
const maxStoragesBodyBytes = 16 * 1024

// cacheStorageID is the synthetic id of the export-cache pseudo-storage
// (the exports directory under DASHCAM_CACHE) in GET /api/storages — it
// has no shares row, so it can never be the target of POST/PATCH/DELETE
// /api/storages/{id} or /api/storages/{id}/scan.
const cacheStorageID = "cache"

// storageView is the JSON shape returned for one persistent storage —
// every shares row (kind cifs|local) plus the cache pseudo-storage. The
// recorder's SSD is never included (see internal/db.StoragesList's doc
// comment).
type storageView struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Label        string  `json:"label"`
	Path         string  `json:"path"`
	Status       string  `json:"status"`
	Enabled      bool    `json:"enabled"`
	FreeBytes    *uint64 `json:"free_bytes,omitempty"`
	TotalBytes   *uint64 `json:"total_bytes,omitempty"`
	SegmentCount int64   `json:"segment_count"`
	SegmentBytes int64   `json:"segment_bytes"`
	ExportCount  int64   `json:"export_count"`
	ExportBytes  int64   `json:"export_bytes"`
	// HashPending is always 0 for now — schema v9 task 6 (hash_backfill
	// job) fills this in with the real count of storage_files/segments on
	// this storage still awaiting a sha256.
	HashPending int64 `json:"hash_pending"`
}

// storageFreeSpace mirrors toShareView's (handlers_shares.go) free/total
// logic, generalized to local/cache storages too: a cifs share must
// actually be mounted (checked live against /proc/mounts, never the
// stored status column) before FreeSpace means anything; a local or cache
// directory has no such concept — see share.ProbeLocal/LocalDirReadable's
// doc comments on why local storages never go through IsMounted.
func storageFreeSpace(kind, path string) (*uint64, *uint64) {
	if path == "" {
		return nil, nil
	}
	if kind == db.KindCIFS {
		if mounted, err := share.IsMounted(path); err != nil || !mounted {
			return nil, nil
		}
	} else if err := share.LocalDirReadable(path); err != nil {
		return nil, nil
	}
	free, total, err := share.FreeSpace(path)
	if err != nil {
		return nil, nil
	}
	return &free, &total
}

// toStorageRowView renders a db.StorageRow (usage totals only) into the
// full JSON view, adding the filesystem-derived id/path/free/total fields
// that only cmd/api (not internal/db) can compute.
func toStorageRowView(row db.StorageRow, cacheExportDir string) storageView {
	v := storageView{
		Kind: row.Kind, Label: row.Name, Enabled: row.Enabled, Status: row.Status,
		SegmentCount: row.SegmentCount, SegmentBytes: row.SegmentBytes,
		ExportCount: row.ExportCount, ExportBytes: row.ExportBytes,
	}
	if row.ShareID != nil {
		v.ID = strconv.FormatInt(*row.ShareID, 10)
		v.Path = row.MountPoint
	} else {
		v.ID = cacheStorageID
		v.Path = cacheExportDir
	}
	v.FreeBytes, v.TotalBytes = storageFreeSpace(v.Kind, v.Path)
	return v
}

// shareToStorageRow renders a freshly created/updated db.Share (no usage
// yet worth querying — POST always, PATCH usually, since usage doesn't
// change from a rename/path edit) as a db.StorageRow for toStorageRowView.
func shareToStorageRow(s db.Share) db.StorageRow {
	id := s.ID
	return db.StorageRow{
		ShareID: &id, Kind: s.Kind, Name: s.Name, MountPoint: s.MountPoint,
		Enabled: s.Enabled, Status: s.Status,
	}
}

// handleStoragesGet serves GET /api/storages: every persistent storage
// (shares of both kinds, plus the cache pseudo-storage) with usage and
// live free/total space.
func handleStoragesGet(store *db.DB, cacheDir string) http.HandlerFunc {
	cacheExportDir := filepath.Join(cacheDir, "exports")
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := store.StoragesList(r.Context())
		if err != nil {
			writeInternalError(w, "storages list", err)
			return
		}
		views := make([]storageView, 0, len(rows))
		for _, row := range rows {
			views = append(views, toStorageRowView(row, cacheExportDir))
		}
		writeJSON(w, http.StatusOK, views)
	}
}

// storageRequestBody is the JSON shape for POST/PATCH /api/storages. For
// kind="local" only Path (and optionally Name/Label, Enabled) are used;
// the rest mirror shareRequestBody (handlers_shares.go) and are forwarded
// as-is to the existing /api/shares handlers for kind="cifs" (the default,
// so an omitted kind behaves exactly like /api/shares always has).
type storageRequestBody struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Enabled *bool  `json:"enabled"`
}

// validateLocalStoragePath validates path for a kind="local" storage: it
// must be a non-empty, absolute, already-clean path (no trailing slash,
// ".", "..", and not "/" itself) that lies under localRoot even after
// resolving symlinks (so a symlink inside localRoot can't point the
// storage somewhere else on the host), and must already exist as a
// directory. Returns the validated (unresolved) clean path to store as
// mount_point — deliberately not the symlink-resolved one, so the stored
// path is exactly what the operator configured.
func validateLocalStoragePath(localRoot, path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if strings.Contains(path, "..") {
		return "", errors.New(`path must not contain ".."`)
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean != path || clean == "/" {
		return "", errors.New(`path must be an absolute, cleaned path (no trailing slash, ".", "..", or "/" itself)`)
	}

	root := filepath.Clean(localRoot)
	if rel, err := filepath.Rel(root, clean); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must be under %s", root)
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("path does not exist or is not accessible: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("local root %s is not accessible: %w", root, err)
	}
	if rel, err := filepath.Rel(resolvedRoot, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path resolves outside %s", root)
	}

	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", errors.New("path must be an existing directory")
	}
	return clean, nil
}

// createLocalStorage validates path and creates a kind=local share (plus
// its linked "share"-kind source) atomically — the local-storage
// equivalent of handleSharesPost's cifs path. share.ProbeLocal (a write
// probe, not just LocalDirReadable) is required here since a newly
// registered storage must actually be usable as an ingest/export target.
func createLocalStorage(ctx context.Context, store *db.DB, localRoot, name, path string, enabled bool) (db.Share, error) {
	clean, err := validateLocalStoragePath(localRoot, path)
	if err != nil {
		return db.Share{}, err
	}
	if err := share.ProbeLocal(clean); err != nil {
		return db.Share{}, fmt.Errorf("path is not usable as a storage: %w", err)
	}
	if name == "" {
		name = filepath.Base(clean)
	}
	shareID, _, err := store.ShareCreateWithSource(ctx, db.ShareCreateParams{
		Name: name, Enabled: enabled, Kind: db.KindLocal,
	}, func(int64) string { return clean }, name)
	if err != nil {
		return db.Share{}, fmt.Errorf("could not create local storage: %w", err)
	}
	return store.ShareGet(ctx, shareID)
}

// forwardShareResponseAsStorage translates a response recorded from
// calling handleSharesPost/handleSharesPatch directly (the cifs-kind
// delegation path) into a storageView. A non-success response (validation
// error, conflict, etc.) is forwarded byte-for-byte instead.
func forwardShareResponseAsStorage(w http.ResponseWriter, rec *httptest.ResponseRecorder, successCode int) {
	if rec.Code != successCode {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var sv shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &sv); err != nil {
		writeInternalError(w, "decode share response", err)
		return
	}
	row := db.StorageRow{ShareID: &sv.ID, Kind: db.KindCIFS, Name: sv.Name, MountPoint: sv.MountPoint, Enabled: sv.Enabled, Status: sv.Status}
	writeJSON(w, successCode, toStorageRowView(row, ""))
}

// handleStoragesPost serves POST /api/storages: kind="cifs" (the default,
// matching /api/shares) is a thin delegation to handleSharesPost itself —
// the exact same request body, decoded and validated the exact same way —
// with the resulting shareView translated into a storageView. kind="local"
// is handled directly (createLocalStorage) since /api/shares has no
// equivalent path.
func handleStoragesPost(store *db.DB, secretKey, localRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxStoragesBodyBytes)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}

		var peek struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(bodyBytes, &peek); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		kind := peek.Kind
		if kind == "" {
			kind = db.KindCIFS
		}

		switch kind {
		case db.KindCIFS:
			delegateReq := httptest.NewRequest(http.MethodPost, "/api/shares", bytes.NewReader(bodyBytes)).WithContext(r.Context())
			rec := httptest.NewRecorder()
			handleSharesPost(store, secretKey)(rec, delegateReq)
			forwardShareResponseAsStorage(w, rec, http.StatusCreated)
		case db.KindLocal:
			var body storageRequestBody
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			name := body.Label
			if name == "" {
				name = body.Name
			}
			s, err := createLocalStorage(r.Context(), store, localRoot, name, body.Path, enabled)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusCreated, toStorageRowView(shareToStorageRow(s), ""))
		default:
			writeJSONError(w, http.StatusBadRequest, `kind must be "cifs" or "local"`)
		}
	}
}

// handleStoragesPatch serves PATCH /api/storages/{id}: branches on the
// storage's existing kind (kind never changes via PATCH), delegating to
// handleSharesPatch for cifs and updating mount_point/name/enabled
// directly (db.ShareUpdateLocal) for local.
func handleStoragesPatch(store *db.DB, secretKey, localRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseStorageShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		existing, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "storage not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxStoragesBodyBytes)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}

		if existing.Kind == db.KindLocal {
			var body storageRequestBody
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
			path := body.Path
			if path == "" {
				path = existing.MountPoint
			}
			clean, err := validateLocalStoragePath(localRoot, path)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			name := body.Label
			if name == "" {
				name = body.Name
			}
			if name == "" {
				name = existing.Name
			}
			enabled := existing.Enabled
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			if err := store.ShareUpdateLocal(r.Context(), id, name, clean, enabled); err != nil {
				writeInternalError(w, "share update local", err)
				return
			}
			s, err := store.ShareGet(r.Context(), id)
			if err != nil {
				writeInternalError(w, "share get after update", err)
				return
			}
			writeJSON(w, http.StatusOK, toStorageRowView(shareToStorageRow(s), ""))
			return
		}

		delegateReq := httptest.NewRequest(http.MethodPatch, "/api/shares/"+strconv.FormatInt(id, 10), bytes.NewReader(bodyBytes)).WithContext(r.Context())
		delegateReq.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		handleSharesPatch(store, secretKey)(rec, delegateReq)
		forwardShareResponseAsStorage(w, rec, http.StatusOK)
	}
}

// handleStoragesDelete serves DELETE /api/storages/{id}: reuses the exact
// same purge_index index-orphan guard as handleSharesDelete
// (shareDeleteIndexGuard, handlers_shares.go), but never unmounts or
// os.RemoveAll's a kind=local storage's directory — that's the operator's
// own data, not a mount point this app created and owns (unlike a cifs
// share's mount point, generated by share.MountPointFor).
func handleStoragesDelete(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseStorageShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "storage not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		purgeIndex := r.URL.Query().Get("purge_index") == "1"
		ok, conflictMsg, err := shareDeleteIndexGuard(r.Context(), store, id, purgeIndex)
		if err != nil {
			writeInternalError(w, "share delete index guard", err)
			return
		}
		if !ok {
			writeJSONError(w, http.StatusConflict, conflictMsg)
			return
		}

		if s.Kind == db.KindCIFS && s.MountPoint != "" {
			if err := share.Unmount(r.Context(), s.MountPoint); err != nil {
				log.Printf("[api] storages: unmount %d before delete: %v", id, err)
			}
		}

		if err := store.ShareDelete(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "storage not found")
			return
		} else if err != nil {
			writeInternalError(w, "share delete", err)
			return
		}

		if s.Kind == db.KindCIFS && s.MountPoint != "" {
			if err := os.RemoveAll(s.MountPoint); err != nil {
				log.Printf("[api] storages: remove mount point %d (%s): %v", id, s.MountPoint, err)
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleStoragesScan serves POST /api/storages/{id}/scan: finds (or, for a
// share row somehow missing its linked source, creates) the "share"-kind
// source backed by this storage and enqueues the existing "scan" job for
// it — deliberately the same job type/handler GET /api/sources/scan uses,
// just addressed by storage id instead of source id.
func handleStoragesScan(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseStorageShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		sh, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "storage not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		src, err := store.SourceGetByShareID(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			srcID, createErr := store.SourceCreate(r.Context(), "share", sh.MountPoint, sh.Name)
			if createErr != nil {
				writeInternalError(w, "source create for storage scan", createErr)
				return
			}
			src, err = store.SourceGet(r.Context(), srcID)
			if err != nil {
				writeInternalError(w, "source get after create", err)
				return
			}
		} else if err != nil {
			writeInternalError(w, "source get by share_id", err)
			return
		}

		active, err := activeScanJobsBySource(r.Context(), q)
		if err != nil {
			writeInternalError(w, "active scan jobs", err)
			return
		}
		if existing, ok := active[src.ID]; ok {
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "already_running", "job": existing})
			return
		}

		job, err := q.Enqueue(r.Context(), "scan", scanJobParams{SourceID: src.ID}, 0)
		if err != nil {
			writeInternalError(w, "enqueue scan job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// parseStorageShareID parses the {id} path segment as the underlying
// share id — rejecting the synthetic "cache" id, which has no shares row
// and so can never be POST/PATCH/DELETE'd or scanned via /api/storages.
func parseStorageShareID(r *http.Request) (int64, error) {
	idStr := r.PathValue("id")
	if idStr == cacheStorageID {
		return 0, fmt.Errorf(`the %q storage cannot be modified or scanned`, cacheStorageID)
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, errors.New("invalid storage id")
	}
	return id, nil
}
