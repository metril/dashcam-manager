package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
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
	ID int64 `json:"id"`
	// Kind is db.KindCIFS or db.KindLocal (local-storage plan task 11):
	// the frontend uses it to show a kind badge and hide CIFS-only fields
	// (server/share/username/domain/options) for local rows.
	Kind          string  `json:"kind"`
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
		ID: s.ID, Kind: s.Kind, Name: s.Name, Server: s.Server, ShareName: s.ShareName, Subpath: s.Subpath,
		Username: s.Username, Domain: s.Domain, MountPoint: s.MountPoint, Options: s.Options,
		Enabled: s.Enabled, Status: s.Status, LastMountedAt: s.LastMountedAt,
		HasPassword: len(s.PasswordEnc) > 0,
	}
	// Free/total space (used by the ingest UI and Shares.tsx to show
	// available room on a target share) is only meaningful once the
	// storage is actually usable — reuses handlers_storages.go's
	// storageFreeSpace, which branches on kind: a cifs share must be
	// mounted (checked live against /proc/mounts, not the stored status
	// column, which can lag reality by up to HealthInterval); a local
	// directory is checked read-only-readable instead (fix round 1 —
	// share.IsMounted was always used here regardless of kind, so a
	// kind=local row's free/total stayed nil forever, since a
	// bind-mounted directory has no /proc/mounts entry to match).
	v.FreeBytes, v.TotalBytes = storageFreeSpace(s.Kind, s.MountPoint)
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
			src, err := store.SourceGetByShareID(r.Context(), s.ID)
			switch {
			case err == nil:
				id := src.ID
				v.SourceID = &id
			case errors.Is(err, db.ErrNotFound):
				// Expected for a share whose source row hasn't landed yet
				// (shouldn't normally happen post-M9's atomic create, but
				// tolerated rather than treated as a server error).
			default:
				log.Printf("[api] shares: source lookup for share %d: %v", s.ID, err)
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

// handleSharesPost creates a new share and its linked "share"-kind source
// (rooted at the share's mount point, so it can be scanned/browsed like
// the SSD once mounted) atomically — see db.ShareCreateWithSource. Does
// not mount it — call POST /api/shares/{id}/mount (or /test) for that.
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
		if err := share.ValidateOptions(body.Options); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Name == "" {
			body.Name = body.Server + "/" + body.Share
		}

		var passwordEnc []byte
		if body.Password != nil && *body.Password != "" {
			if secretKey == "" {
				writeJSONError(w, http.StatusServiceUnavailable, "DASHCAM_SECRET_KEY is not set; share credentials cannot be encrypted. Set it and restart.")
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

		id, _, err := store.ShareCreateWithSource(r.Context(), db.ShareCreateParams{
			Name: body.Name, Server: body.Server, ShareName: body.Share, Subpath: body.Subpath,
			Username: body.Username, PasswordEnc: passwordEnc, Domain: body.Domain,
			Options: body.Options, Enabled: enabled,
		}, func(shareID int64) string {
			return share.MountPointFor(share.DefaultMountBase, share.Slug(body.Name, shareID))
		}, body.Name)
		if err != nil {
			writeJSONError(w, http.StatusConflict, "could not create share: "+err.Error())
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
// empty string clears it. Changing server/share/subpath (i.e. what's
// actually mounted) or explicitly disabling the share unmounts it
// (best-effort) first, since whatever is currently mounted no longer
// matches the new config (or shouldn't be mounted at all) — the health
// loop will remount under the new config on its next pass if the share is
// still enabled.
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
		if err := share.ValidateOptions(body.Options); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Name == "" {
			body.Name = existing.Name
		}

		enabled := existing.Enabled
		if body.Enabled != nil {
			enabled = *body.Enabled
		}

		params := db.ShareUpdateParams{
			Name: body.Name, Server: body.Server, ShareName: body.Share, Subpath: body.Subpath,
			Username: body.Username, Domain: body.Domain, Options: body.Options,
			Enabled: enabled,
		}
		if body.Password != nil {
			if *body.Password == "" {
				params.SetPassword = true
				params.PasswordEnc = nil
			} else {
				if secretKey == "" {
					writeJSONError(w, http.StatusServiceUnavailable, "DASHCAM_SECRET_KEY is not set; share credentials cannot be encrypted. Set it and restart.")
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

		// mount_point is scoped to the share's id (see share.Slug), so a
		// rename never moves it — only server/share/subpath (what's
		// actually served at that mount point) or disabling can make the
		// current mount stale.
		mountTargetChanged := body.Server != existing.Server || body.Share != existing.ShareName || body.Subpath != existing.Subpath
		nowDisabled := existing.Enabled && !enabled
		if (mountTargetChanged || nowDisabled) && existing.MountPoint != "" {
			if err := share.Unmount(r.Context(), existing.MountPoint); err != nil {
				log.Printf("[api] shares: unmount %d before patch: %v", id, err)
			}
		}

		if err := store.ShareUpdate(r.Context(), id, params); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "share not found")
			return
		} else if err != nil {
			writeInternalError(w, "share update", err)
			return
		}

		if mountTargetChanged || nowDisabled {
			status := "unmounted"
			if nowDisabled {
				status = "disabled"
			}
			if err := store.ShareSetStatus(r.Context(), id, status, false, ""); err != nil {
				log.Printf("[api] shares: set status %d after patch: %v", id, err)
			}
		}

		s, err := store.ShareGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "share get after update", err)
			return
		}
		writeJSON(w, http.StatusOK, toShareView(s))
	}
}

// handleSharesDelete unmounts (best-effort), removes the share's mount
// point directory, and deletes the share row.
//
// The linked source's share_id column is ON DELETE SET NULL (not CASCADE —
// see schema.go's comment on the sources table), so deleting a share alone
// no longer silently wipes an indexed source's segments/trips. If the
// share's source still has indexed segments, the delete is refused with
// 409 unless the caller passes ?purge_index=1, in which case the source
// (and, transitively, its segments/trips/detections) is explicitly deleted
// too. A source with zero segments (nothing to lose) is left in place with
// share_id NULL either way, preserving it as a plain orphaned source rather
// than destroying it for no reason.
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

		purgeIndex := r.URL.Query().Get("purge_index") == "1"

		if ok, conflictMsg, err := shareDeleteIndexGuard(r.Context(), store, id, purgeIndex); err != nil {
			writeInternalError(w, "share delete index guard", err)
			return
		} else if !ok {
			writeJSONError(w, http.StatusConflict, conflictMsg)
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

		if s.MountPoint != "" {
			if err := os.RemoveAll(s.MountPoint); err != nil {
				// Best-effort cleanup; the share row is already gone.
				log.Printf("[api] shares: remove mount point %d (%s): %v", id, s.MountPoint, err)
			}
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

// shareDeleteIndexGuard is the "would this delete orphan indexed
// segments?" guard shared by handleSharesDelete and the /api/storages
// DELETE wrapper (see handleSharesDelete's doc comment for the full
// rationale). If the share's source has indexed segments and purgeIndex is
// false, it returns ok=false with a 409-worthy conflict message and
// changes nothing. If purgeIndex is true, it deletes the source (and,
// transitively, its segments/trips/detections) before returning ok=true —
// the caller is still responsible for actually deleting the share row
// itself (and any kind-specific cleanup, e.g. unmounting).
func shareDeleteIndexGuard(ctx context.Context, store *db.DB, shareID int64, purgeIndex bool) (ok bool, conflictMsg string, err error) {
	src, srcErr := store.SourceGetByShareID(ctx, shareID)
	hasSource := srcErr == nil
	if srcErr != nil && !errors.Is(srcErr, db.ErrNotFound) {
		return false, "", srcErr
	}

	var segCount int
	if hasSource {
		segCount, err = store.SourceSegmentCount(ctx, src.ID)
		if err != nil {
			return false, "", err
		}
	}

	if hasSource && segCount > 0 && !purgeIndex {
		return false, fmt.Sprintf(
			"share %d's source has %d indexed segment(s); deleting the share would leave them orphaned. "+
				"Retry with ?purge_index=1 to delete the share AND its indexed segments/trips, or unlink/reassign the source first.",
			shareID, segCount), nil
	}

	if hasSource && segCount > 0 && purgeIndex {
		if err := store.SourceDelete(ctx, src.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
			return false, "", err
		}
	}

	return true, "", nil
}

func parseShareID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errors.New("invalid share id")
	}
	return id, nil
}
