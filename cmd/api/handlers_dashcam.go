package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
)

// maxDashcamBodyBytes caps every dashcam request body — all of them are
// small fixed-shape objects, mirroring maxSharesBodyBytes.
const maxDashcamBodyBytes = 16 * 1024

// --- shared view / lookups --------------------------------------------------

// dashcamView is the JSON shape returned for a dashcam source.
type dashcamView struct {
	ID              int64   `json:"id"`
	Label           string  `json:"label"`
	BaseURL         string  `json:"base_url"`
	TargetShareID   int64   `json:"target_share_id"`
	TargetShareName string  `json:"target_share_label"`
	PullCursor      *string `json:"pull_cursor"`
	LastPullAt      *string `json:"last_pull_at"`
	Status          string  `json:"status"`
}

// toDashcamView renders s (a kind='dashcam' Source) as a dashcamView.
// targetShareLabel is looked up by the caller (a failed lookup — e.g. the
// target share was since deleted — is tolerated and simply left "").
func toDashcamView(s db.Source, targetShareLabel string) dashcamView {
	v := dashcamView{
		ID: s.ID, Label: s.Label, PullCursor: s.PullCursor, LastPullAt: s.LastPullAt, Status: s.Status,
		TargetShareName: targetShareLabel,
	}
	if s.BaseURL != nil {
		v.BaseURL = *s.BaseURL
	}
	if s.TargetShareID != nil {
		v.TargetShareID = *s.TargetShareID
	}
	return v
}

// targetShareLabel best-effort resolves shareID's display label, tolerating
// a since-deleted share rather than failing the whole response.
func targetShareLabel(r *http.Request, store *db.DB, shareID int64) string {
	s, err := store.ShareGet(r.Context(), shareID)
	if err != nil {
		return ""
	}
	return s.Name
}

func parseDashcamID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errors.New("invalid dashcam id")
	}
	return id, nil
}

// validBaseURL reports whether raw parses as an absolute http(s) URL.
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// --- GET /api/dashcams -------------------------------------------------------

func handleDashcamsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.DashcamList(r.Context())
		if err != nil {
			writeInternalError(w, "dashcams list", err)
			return
		}
		views := make([]dashcamView, 0, len(list))
		for _, s := range list {
			label := ""
			if s.TargetShareID != nil {
				label = targetShareLabel(r, store, *s.TargetShareID)
			}
			views = append(views, toDashcamView(s, label))
		}
		writeJSON(w, http.StatusOK, views)
	}
}

// --- POST /api/dashcams -------------------------------------------------------

// dashcamRequestBody is the JSON body for POST and PATCH /api/dashcams;
// pointer fields let PATCH distinguish "omitted, leave unchanged" from a
// present value, matching shareRequestBody's convention. POST requires
// all three.
type dashcamRequestBody struct {
	Label         *string `json:"label"`
	BaseURL       *string `json:"base_url"`
	TargetShareID *int64  `json:"target_share_id"`
}

func handleDashcamsPost(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxDashcamBodyBytes)

		var body dashcamRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.Label == nil || *body.Label == "" {
			writeJSONError(w, http.StatusBadRequest, "label is required")
			return
		}
		if body.BaseURL == nil || !validBaseURL(*body.BaseURL) {
			writeJSONError(w, http.StatusBadRequest, "base_url must be an absolute http(s) URL")
			return
		}
		if body.TargetShareID == nil || *body.TargetShareID == 0 {
			writeJSONError(w, http.StatusBadRequest, "target_share_id is required")
			return
		}
		if _, err := store.ShareGet(r.Context(), *body.TargetShareID); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusBadRequest, "target_share_id does not name an existing share")
			return
		} else if err != nil {
			writeInternalError(w, "target share get", err)
			return
		}

		id, err := store.DashcamCreate(r.Context(), *body.Label, *body.BaseURL, *body.TargetShareID)
		if err != nil {
			writeJSONError(w, http.StatusConflict, "could not create dashcam: "+err.Error())
			return
		}
		s, err := store.DashcamGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "dashcam get after create", err)
			return
		}
		writeJSON(w, http.StatusCreated, toDashcamView(s, targetShareLabel(r, store, *body.TargetShareID)))
	}
}

// --- PATCH /api/dashcams/{id} -------------------------------------------------

// handleDashcamsPatch applies a partial update: any of label/base_url/
// target_share_id may be omitted, in which case the existing value is
// kept (db.DashcamUpdate itself takes the full row, so this merges before
// calling it — mirroring handleSharesPatch's own merge-then-full-update
// shape).
func handleDashcamsPatch(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		existing, err := store.DashcamGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		}
		if err != nil {
			writeInternalError(w, "dashcam get", err)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxDashcamBodyBytes)
		var body dashcamRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		label := existing.Label
		if body.Label != nil {
			if *body.Label == "" {
				writeJSONError(w, http.StatusBadRequest, "label must not be empty")
				return
			}
			label = *body.Label
		}
		baseURL := ""
		if existing.BaseURL != nil {
			baseURL = *existing.BaseURL
		}
		if body.BaseURL != nil {
			if !validBaseURL(*body.BaseURL) {
				writeJSONError(w, http.StatusBadRequest, "base_url must be an absolute http(s) URL")
				return
			}
			baseURL = *body.BaseURL
		}
		targetShareID := int64(0)
		if existing.TargetShareID != nil {
			targetShareID = *existing.TargetShareID
		}
		if body.TargetShareID != nil {
			if *body.TargetShareID == 0 {
				writeJSONError(w, http.StatusBadRequest, "target_share_id must not be 0")
				return
			}
			if _, err := store.ShareGet(r.Context(), *body.TargetShareID); errors.Is(err, db.ErrNotFound) {
				writeJSONError(w, http.StatusBadRequest, "target_share_id does not name an existing share")
				return
			} else if err != nil {
				writeInternalError(w, "target share get", err)
				return
			}
			targetShareID = *body.TargetShareID
		}

		if err := store.DashcamUpdate(r.Context(), id, label, baseURL, targetShareID, true); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		} else if err != nil {
			writeInternalError(w, "dashcam update", err)
			return
		}

		s, err := store.DashcamGet(r.Context(), id)
		if err != nil {
			writeInternalError(w, "dashcam get after update", err)
			return
		}
		writeJSON(w, http.StatusOK, toDashcamView(s, targetShareLabel(r, store, targetShareID)))
	}
}

// --- DELETE /api/dashcams/{id} ------------------------------------------------

func handleDashcamsDelete(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.DashcamDelete(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		} else if err != nil {
			writeInternalError(w, "dashcam delete", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- POST /api/dashcams/{id}/test --------------------------------------------

// dashcamTestResult is POST /api/dashcams/{id}/test's response shape. A
// remote/connectivity/configuration failure is never a 500 here — it's
// reported as ok:false with an error_code, since "the dashcam is
// unreachable" is an expected, actionable outcome of this endpoint, not a
// server bug.
type dashcamTestResult struct {
	OK         bool                 `json:"ok"`
	ErrorCode  string               `json:"error_code"`
	ServerTime string               `json:"server_time,omitempty"`
	SkewS      float64              `json:"skew_s"`
	Lease      *dashcam.LeaseStatus `json:"lease,omitempty"`
}

func handleDashcamTest(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		_, client, err := dashcamClientFor(r.Context(), store, secretKey, id)
		if err != nil {
			writeJSON(w, http.StatusOK, dashcamTestResult{ErrorCode: err.Error()})
			return
		}

		now := time.Now().UTC()
		st, err := client.MaintenanceStatus(r.Context())
		if err != nil {
			code := "unreachable"
			if ae, ok := asAPIError(err); ok {
				code = ae.Code
			}
			writeJSON(w, http.StatusOK, dashcamTestResult{ErrorCode: code})
			return
		}
		// The client corrects clock skew transparently and internally
		// (see internal/dashcam.Client's unexported skew field); the
		// public API surface has no way to read the measured delta back
		// out, so skew_s is reported as 0 rather than fabricated.
		// server_time reflects our own clock at the moment of a
		// successful round trip, not the recorder's (MaintenanceStatus's
		// 200 response carries no server_time field — only its error
		// bodies do).
		writeJSON(w, http.StatusOK, dashcamTestResult{
			OK: true, ServerTime: now.Format(time.RFC3339), SkewS: 0, Lease: &st,
		})
	}
}

// asAPIError unwraps err into a *dashcam.APIError if it is one.
func asAPIError(err error) (*dashcam.APIError, bool) {
	var ae *dashcam.APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// --- POST /api/dashcams/{id}/pull ---------------------------------------------

func handleDashcamPull(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := store.DashcamGet(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		} else if err != nil {
			writeInternalError(w, "dashcam get", err)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxDashcamBodyBytes)
		var body struct {
			Full bool `json:"full"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}

		job, err := q.Enqueue(r.Context(), "dashcam_pull", dashcamPullJobParams{SourceID: id, Full: body.Full}, 0)
		if err != nil {
			writeInternalError(w, "enqueue dashcam_pull job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// --- POST /api/dashcams/{id}/cleanup ------------------------------------------

func handleDashcamCleanup(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := store.DashcamGet(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		} else if err != nil {
			writeInternalError(w, "dashcam get", err)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxDashcamBodyBytes)
		var body struct {
			DryRun *bool `json:"dry_run"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}

		job, err := q.Enqueue(r.Context(), "dashcam_cleanup", dashcamCleanupJobParams{SourceID: id, DryRun: body.DryRun}, 0)
		if err != nil {
			writeInternalError(w, "enqueue dashcam_cleanup job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// --- POST /api/dashcams/{id}/reprocess -----------------------------------------

func handleDashcamReprocess(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := store.DashcamGet(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		} else if err != nil {
			writeInternalError(w, "dashcam get", err)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxDashcamBodyBytes)
		var body struct {
			Camera      string `json:"camera"`
			Date        string `json:"date"`
			Path        string `json:"path"`
			Segment     string `json:"segment"`
			OnlyMissing bool   `json:"only_missing"`
			SkipFrames  bool   `json:"skip_frames"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		params := dashcamReprocessJobParams{
			SourceID: id, Camera: body.Camera, Date: body.Date, Path: body.Path, Segment: body.Segment,
			OnlyMissing: body.OnlyMissing, SkipFrames: body.SkipFrames,
		}
		if err := params.validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		job, err := q.Enqueue(r.Context(), "dashcam_reprocess", params, 0)
		if err != nil {
			writeInternalError(w, "enqueue dashcam_reprocess job", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
	}
}

// --- GET /api/dashcams/{id}/status --------------------------------------------

// dashcamReprocessView is the reprocess portion of GET
// /api/dashcams/{id}/status. State mirrors the recorder's own reprocess
// state machine (resources/dashboard.py): "idle" (never run),
// "starting"/"running" (in progress), "done"/"error" (terminal — persist
// until the next run overwrites them).
type dashcamReprocessView struct {
	State         string `json:"state"`
	Camera        string `json:"camera,omitempty"`
	Date          string `json:"date,omitempty"`
	Processed     int    `json:"processed,omitempty"`
	TotalSegments int    `json:"total_segments,omitempty"`
	Error         string `json:"error,omitempty"`
}

type dashcamStatusResult struct {
	Reachable  bool                  `json:"reachable"`
	ErrorCode  string                `json:"error_code,omitempty"`
	Lease      *dashcam.LeaseStatus  `json:"lease,omitempty"`
	Reprocess  *dashcamReprocessView `json:"reprocess,omitempty"`
	LastPullAt *string               `json:"last_pull_at"`
	PullCursor *string               `json:"pull_cursor"`
	Pulls      dashcamPullCounts     `json:"pulls"`
}

type dashcamPullCounts struct {
	Verified       int `json:"verified"`
	PendingCleanup int `json:"pending_cleanup"`
}

func handleDashcamStatus(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseDashcamID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		src, err := store.DashcamGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		}
		if err != nil {
			writeInternalError(w, "dashcam get", err)
			return
		}

		verified, pendingCleanup, err := store.PullCounts(r.Context(), id)
		if err != nil {
			writeInternalError(w, "dashcam pull counts", err)
			return
		}

		result := dashcamStatusResult{
			LastPullAt: src.LastPullAt, PullCursor: src.PullCursor,
			Pulls: dashcamPullCounts{Verified: verified, PendingCleanup: pendingCleanup},
		}

		_, client, err := dashcamClientFor(r.Context(), store, secretKey, id)
		if err != nil {
			result.ErrorCode = err.Error()
			writeJSON(w, http.StatusOK, result)
			return
		}

		st, err := client.MaintenanceStatus(r.Context())
		if err != nil {
			result.ErrorCode = "unreachable"
			if ae, ok := asAPIError(err); ok {
				result.ErrorCode = ae.Code
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		result.Reachable = true
		result.Lease = &st

		if rst, err := client.ReprocessStatus(r.Context()); err == nil {
			result.Reprocess = &dashcamReprocessView{
				State: rst.State, Camera: rst.Camera, Date: rst.Date,
				Processed: rst.Processed, TotalSegments: rst.TotalSegments, Error: rst.Error,
			}
		}

		writeJSON(w, http.StatusOK, result)
	}
}

// --- GET /api/dashcams/key, POST /api/dashcams/key/generate --------------------

// dashcamKeyView is the response shape for both GET /api/dashcams/key and
// POST /api/dashcams/key/generate.
type dashcamKeyView struct {
	Configured    bool   `json:"configured"`
	KeyID         string `json:"key_id,omitempty"`
	PublicKeyB64  string `json:"public_key_b64,omitempty"`
	ConfigSnippet string `json:"config_snippet,omitempty"`
}

// dashcamConfigSnippet renders the paste-ready YAML the operator adds
// under the recorder's `ingest:` config block (see the sibling dashcam
// repo's config/config.opi5.yaml: `ingest.enabled` and
// `ingest.public_keys` — a key_id -> base64 Ed25519 public key map, same
// format scripts/ingest-keygen.py produces) to accept this manager's
// signed requests.
func dashcamConfigSnippet(keyID, pubB64 string) string {
	return fmt.Sprintf("ingest:\n  enabled: true\n  public_keys:\n    %s: %s\n", keyID, pubB64)
}

// dashcamKeyResponseFor renders the configured-key response shared by GET
// /api/dashcams/key and POST /api/dashcams/key/generate.
func dashcamKeyResponseFor(keyID string, priv ed25519.PrivateKey) dashcamKeyView {
	pubB64 := dashcam.PublicKeyB64(priv.Public().(ed25519.PublicKey))
	return dashcamKeyView{
		Configured: true, KeyID: keyID, PublicKeyB64: pubB64,
		ConfigSnippet: dashcamConfigSnippet(keyID, pubB64),
	}
}

func handleDashcamKeyGet(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID, priv, ok, err := dashcam.LoadKey(r.Context(), store, secretKey)
		if err != nil {
			writeInternalError(w, "dashcam key load", err)
			return
		}
		if !ok {
			writeJSON(w, http.StatusOK, dashcamKeyView{Configured: false})
			return
		}
		writeJSON(w, http.StatusOK, dashcamKeyResponseFor(keyID, priv))
	}
}

func handleDashcamKeyGenerate(store *db.DB, secretKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxDashcamBodyBytes)
		var body struct {
			Force bool `json:"force"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}

		_, _, ok, err := dashcam.LoadKey(r.Context(), store, secretKey)
		if err != nil {
			writeInternalError(w, "dashcam key load", err)
			return
		}
		if ok && !body.Force {
			writeJSONError(w, http.StatusConflict, "a dashcam key is already configured; pass force:true to replace it")
			return
		}

		keyID, _, priv, err := dashcam.GenerateKey()
		if err != nil {
			writeInternalError(w, "dashcam key generate", err)
			return
		}
		if err := dashcam.SaveKey(r.Context(), store, secretKey, keyID, priv); err != nil {
			writeInternalError(w, "dashcam key save", err)
			return
		}
		writeJSON(w, http.StatusCreated, dashcamKeyResponseFor(keyID, priv))
	}
}
