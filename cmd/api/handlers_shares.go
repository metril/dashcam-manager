package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/share"
)

// maxSharesBodyBytes caps POST/PATCH /api/shares request bodies — a share
// config is a small fixed-shape object, never a bulk upload.
const maxSharesBodyBytes = 16 * 1024

// shareView is the JSON shape returned for a share. Password is
// deliberately never included, even encrypted.
type shareView struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Server        string  `json:"server"`
	ShareName     string  `json:"share"`
	Subpath       string  `json:"subpath"`
	Username      string  `json:"username"`
	Domain        string  `json:"domain"`
	MountPoint    string  `json:"mount_point"`
	Options       string  `json:"options"`
	Enabled       bool    `json:"enabled"`
	Status        string  `json:"status"`
	LastMountedAt *string `json:"last_mounted_at"`
	HasPassword   bool    `json:"has_password"`
	SourceID      *int64  `json:"source_id,omitempty"`
	FreeBytes     *uint64 `json:"free_bytes,omitempty"`
	TotalBytes    *uint64 `json:"total_bytes,omitempty"`
}

func toShareView(s db.Share) shareView {
	v := shareView{
		ID: s.ID, Name: s.Name, Server: s.Server, ShareName: s.ShareName, Subpath: s.Subpath,
		Username: s.Username, Domain: s.Domain, MountPoint: s.MountPoint, Options: s.Options,
		Enabled: s.Enabled, Status: s.Status, LastMountedAt: s.LastMountedAt,
		HasPassword: len(s.PasswordEnc) > 0,
	}
	// Free/total space (used by the ingest UI to show available room on a
	// target share) is only meaningful once the share is actually mounted;
	// leave both nil otherwise rather than reporting stats for whatever
	// happens to be at an unmounted directory.
	if s.MountPoint != "" {
		if free, total, err := share.FreeSpace(s.MountPoint); err == nil {
			v.FreeBytes = &free
			v.TotalBytes = &total
		}
	}
	return v
}

// handleSharesGet lists every configured share (password never included),
// with each one's linked source id (for the "scan now" button) and free
// space (for the ingest UI).
func handleSharesGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shares, err := store.SharesList(r.Context())
		if err != nil {
			writeInternalError(w, "shares list", err)
			return
		}
		views := make([]shareView, 0, len(shares))
		for _, s := range shares {
			v := toShareView(s)
			if src, err := store.SourceGetByShareID(r.Context(), s.ID); err == nil {
				id := src.ID
				v.SourceID = &id
			}
			views = append(views, v)
		}
		writeJSON(w, http.StatusOK, views)
	}
}

// shareRequestBody is the shared JSON shape for POST and PATCH /api/shares.
type shareRequestBody struct {
	Name     string  `json:"name"`
	Server   string  `json:"server"`
	Share    string  `json:"share"`
	Subpath  string  `json:"subpath"`
	Username string  `json:"username"`
	Password *string `json:"password"` // nil = leave unchanged (PATCH); "" = explicitly clear
	Domain   string  `json:"domain"`
	Options  string  `json:"options"`
	Enabled  *bool   `json:"enabled"`
}

// handleSharesPost creates a new share (and a "share"-kind source rooted at
// its mount point, so it can be scanned/browsed like the SSD once
// mounted). Does not mount it — call POST /api/shares/{id}/mount (or
// /test) for that.
func handleSharesPost(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxSharesBodyBytes)

		var body shareRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.Server == "" || body.Share == "" {
			writeJSONError(w, http.StatusBadRequest, "server and share are required")
			return
		}
		if body.Name == "" {
			body.Name = body.Server + "/" + body.Share
		}

		var passwordEnc []byte
		if body.Password != nil && *body.Password != "" {
			if secretKey == "" {
				writeJSONError(w, http.StatusBadRequest, share.ErrEmptyKey.Error())
				return
			}
			enc, err := share.Encrypt(secretKey, *body.Password)
			if err != nil {
				writeInternalError(w, "encrypt share password", err)
				return
			}
			passwordEnc = enc
		}

		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}

		mountPoint := share.MountPointFor(share.DefaultMountBase, share.Slug(body.Name))

		id, err := store.ShareCreate(r.Context(), db.ShareCreateParams{
			Name: body.Name, Server: body.Server, ShareName: body.Share, Subpath: body.Subpath,
			Username: body.Username, PasswordEnc: passwordEnc, Domain: body.Domain,
			MountPoint: mountPoint, Options: body.Options, Enabled: enabled,
		})
		if err != nil {
			writeJSONError(w, http.StatusConflict, "could not create share: "+err.Error())
			return
		}

		if _, err := store.SourceCreateForShare(r.Context(), id, mountPoint, body.Name); err != nil {
			writeInternalError(w, "create source for share", err)
			return
		}

		s, err := store.ShareGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "share get after create", err)
			return
		}
		writeJSON(w, http.StatusCreated, toShareView(s))
	}
}

// handleSharesPatch updates the editable fields of a share. A missing
// "password" field leaves the stored credential untouched; an explicit
// empty string clears it.
func handleSharesPatch(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		existing, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxSharesBodyBytes)
		var body shareRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.Server == "" || body.Share == "" {
			writeJSONError(w, http.StatusBadRequest, "server and share are required")
			return
		}
		if body.Name == "" {
			body.Name = existing.Name
		}

		params := db.ShareUpdateParams{
			Name: body.Name, Server: body.Server, ShareName: body.Share, Subpath: body.Subpath,
			Username: body.Username, Domain: body.Domain, Options: body.Options,
			Enabled: existing.Enabled,
		}
		if body.Enabled != nil {
			params.Enabled = *body.Enabled
		}
		if body.Password != nil {
			if *body.Password == "" {
				params.SetPassword = true
				params.PasswordEnc = nil
			} else {
				if secretKey == "" {
					writeJSONError(w, http.StatusBadRequest, share.ErrEmptyKey.Error())
					return
				}
				enc, err := share.Encrypt(secretKey, *body.Password)
				if err != nil {
					writeInternalError(w, "encrypt share password", err)
					return
				}
				params.SetPassword = true
				params.PasswordEnc = enc
			}
		}

		if err := store.ShareUpdate(r.Context(), id, params); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		} else if err != nil {
			writeInternalError(w, "share update", err)
			return
		}

		s, err := store.ShareGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "share get after update", err)
			return
		}
		writeJSON(w, http.StatusOK, toShareView(s))
	}
}

// handleSharesDelete unmounts (best-effort) and removes a share. The
// linked source (and, transitively, its segments/trips) cascade per the
// schema's ON DELETE CASCADE.
func handleSharesDelete(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		s, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		if s.MountPoint != "" {
			if err := share.Unmount(r.Context(), s.MountPoint); err != nil {
				// Best-effort: still proceed with deleting the share row.
				log.Printf("[api] shares: unmount %d before delete: %v", id, err)
			}
		}

		if err := store.ShareDelete(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		} else if err != nil {
			writeInternalError(w, "share delete", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSharesTest mounts the share to a scratch temp directory, lists it,
// and unmounts — without touching the share's persistent mount point or
// stored status — to verify the configured credentials/path actually work.
func handleSharesTest(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		if err := share.Test(r.Context(), s, secretKey); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleSharesMount mounts a share at its persistent mount point and
// updates its stored status.
func handleSharesMount(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		if err := share.MountCIFS(r.Context(), s, secretKey); err != nil {
			_ = store.ShareSetStatus(r.Context(), id, "error:"+err.Error(), false, "")
			writeJSONError(w, http.StatusBadGateway, "mount failed: "+err.Error())
			return
		}
		_ = store.ShareSetStatus(r.Context(), id, "mounted", true, time.Now().UTC().Format(time.RFC3339))

		s, err = store.ShareGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "share get after mount", err)
			return
		}
		writeJSON(w, http.StatusOK, toShareView(s))
	}
}

// handleSharesUnmount unmounts a share and updates its stored status.
func handleSharesUnmount(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseShareID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s, err := store.ShareGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		}
		if err != nil {
			writeInternalError(w, "share get", err)
			return
		}

		if s.MountPoint != "" {
			if err := share.Unmount(r.Context(), s.MountPoint); err != nil {
				writeJSONError(w, http.StatusBadGateway, "unmount failed: "+err.Error())
				return
			}
		}
		_ = store.ShareSetStatus(r.Context(), id, "unmounted", false, "")

		s, err = store.ShareGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "share get after unmount", err)
			return
		}
		writeJSON(w, http.StatusOK, toShareView(s))
	}
}

func parseShareID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errors.New("invalid share id")
	}
	return id, nil
}
