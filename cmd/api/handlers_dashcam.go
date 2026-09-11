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
	if err != nil || !u.IsAbs() || u.Host == "" {
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
		if _, err := store.DashcamGet(r.Context(), id); errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "dashcam not found")
			return
		} else if err != nil {
			writeInternalError(w, "dashcam get", err)
			return
		}

		_, client, err := dashcamClientFor(r.Context(), store, secretKey, id)
		if err != nil {
			if code, ok := dashcamClientErrorCode(err); ok {
				writeJSON(w, http.StatusOK, dashcamTestResult{ErrorCode: code})
				return
			}
			writeInternalError(w, "dashcam client", err)
			return
		}

		st, err := client.MaintenanceStatus(r.Context())
		if err != nil {
			result := dashcamTestResult{ErrorCode: "unreachable"}
			if ae, ok := asAPIError(err); ok {
				result.ErrorCode = ae.Code
				// APIError.ServerTime is the recorder's own clock, sent
				// by every non-2xx response body (see client.go's
				// attempt) — report it verbatim when we have it, rather
				// than guessing from our own clock.
				result.ServerTime = time.Unix(0, int64(ae.ServerTime*float64(time.Second))).UTC().Format(time.RFC3339)
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		// client.Skew() is the correction MaintenanceStatus's own call
		// just learned (or re-confirmed as 0) via the transparent
		// 401-clock_skew retry in doHeaders/attempt — a real measured
		// value, not a guess. server_time is derived from it (our clock
		// plus that correction) since a successful 200 response carries
		// no server_time field of its own to report verbatim.
		skew := client.Skew()
		writeJSON(w, http.StatusOK, dashcamTestResult{
			OK: true, ServerTime: time.Now().UTC().Add(skew).Format(time.RFC3339),
			SkewS: skew.Seconds(), Lease: &st,
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

// dashcamClientErrorCode maps a dashcamClientFor error to a stable,
// machine-readable code suitable for a 200 ok:false response body. Only
// errDashcamKeyNotConfigured (an expected, actionable "not set up yet"
// state) gets one; any other error (e.g. LoadKey failing to decrypt a
// corrupted stored key) is an unexpected internal error the caller should
// report as a 500 rather than silently swallow into the response body —
// signalled by ok=false.
func dashcamClientErrorCode(err error) (code string, ok bool) {
	if errors.Is(err, errDashcamKeyNotConfigured) {
		return "key_not_configured", true
	}
	return "", false
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

// dashcamStatusResult is GET /api/dashcams/{id}/status's response shape.
// error_code/lease/reprocess are always present keys (never dropped via
// omitempty), so a client can rely on their presence — error_code is ""
// when there's nothing to report, lease/reprocess are JSON null when the
// dashcam couldn't be reached (or, for reprocess, when it could be
// reached but ReprocessStatus itself failed).
type dashcamStatusResult struct {
	Reachable  bool                  `json:"reachable"`
	ErrorCode  string                `json:"error_code"`
	Lease      *dashcam.LeaseStatus  `json:"lease"`
	Reprocess  *dashcamReprocessView `json:"reprocess"`
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
			code, ok := dashcamClientErrorCode(err)
			if !ok {
				writeInternalError(w, "dashcam client", err)
				return
			}
			result.ErrorCode = code
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
