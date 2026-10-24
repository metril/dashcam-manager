package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"

	"dashcam/internal/auth"
	"dashcam/internal/config"
	"dashcam/internal/db"
)

// handleSettingsMetaGet serves GET /api/settings/meta: read-only facts the
// settings page shows next to the editable values.
func handleSettingsMetaGet(store *db.DB, ap *applier, cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stored, err := store.SettingsGetAll(r.Context())
		if err != nil {
			writeInternalError(w, "settings meta", err)
			return
		}
		// env_differs lists settings seeded from a DASHCAM_* variable that
		// has since been changed in the UI (the saved value wins; this is
		// just a hint). The client secret is never compared.
		differs := []string{}
		seeds := envSeedSettings(cfg)
		for key := range cfg.SeedSet {
			want, ok := seeds[key]
			if !ok {
				continue
			}
			if have, ok := stored[key]; ok && !jsonEqual(have, want) {
				differs = append(differs, key)
			}
		}
		sort.Strings(differs)

		redirect := ""
		if u := getRT().PublicURL; u != "" {
			redirect = u + "/auth/callback"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"bootstrap":         map[string]string{"db": cfg.DB, "cache": cfg.Cache, "addr": cfg.Addr},
			"secret_key_source": cfg.SecretKeySource,
			"auth_disabled":     cfg.AuthDisable,
			"env_differs":       differs,
			"restart_required":  ap.RestartRequired(),
			"redirect_uri":      redirect,
		})
	}
}

const maxOIDCBodyBytes = 16 * 1024

// handleOIDCDiscoverPost serves POST /api/settings/oidc/discover {issuer}:
// runs OIDC discovery (10s timeout) so the UI can check an issuer before
// testing. Discovery failure is a 200 with ok:false and an error message;
// only an unparseable issuer is a 400.
func handleOIDCDiscoverPost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxOIDCBodyBytes)
		var body struct {
			Issuer string `json:"issuer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		issuer := strings.TrimSpace(body.Issuer)
		if err := config.ValidIssuerURL(issuer); err != nil {
			writeJSONError(w, http.StatusBadRequest, "issuer "+err.Error())
			return
		}
		res, err := auth.Discover(r.Context(), issuer)
		if err != nil {
			// The raw error can carry the IdP's response body; log it, tell
			// the client only what to check.
			log.Printf("[api] oidc discover %s: %v", issuer, err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "Could not read the OpenID configuration from this issuer. Check the URL and that the server can reach it."})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "issuer": res.Issuer, "authorization_endpoint": res.AuthorizationEndpoint, "end_session": res.EndSession,
		})
	}
}

// handleOIDCTestPost serves POST /api/settings/oidc/test: validates the
// candidate sign-in config, stores it as pending (client secret encrypted,
// 10 minute expiry) and returns the URL the browser must open to prove it
// works. Nothing becomes live until that login succeeds (auth.Callback).
// client_secret omitted keeps the stored secret; "" means a public client.
func handleOIDCTestPost(store *db.DB, ap *applier, authn *auth.Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxOIDCBodyBytes)
		var body struct {
			Issuer        string   `json:"issuer"`
			ClientID      string   `json:"client_id"`
			ClientSecret  *string  `json:"client_secret"`
			Scopes        []string `json:"scopes"`
			AllowedEmails []string `json:"allowed_emails"`
			AllowedGroups []string `json:"allowed_groups"`
			GroupsClaim   string   `json:"groups_claim"`
			PublicURL     *string  `json:"public_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}

		issuer := strings.TrimSpace(body.Issuer)
		if err := config.ValidIssuerURL(issuer); err != nil {
			writeJSONError(w, http.StatusBadRequest, "issuer "+err.Error())
			return
		}
		clientID := strings.TrimSpace(body.ClientID)
		if clientID == "" || len(clientID) > 256 {
			writeJSONError(w, http.StatusBadRequest, "client_id is required")
			return
		}
		publicURL := getRT().PublicURL
		if body.PublicURL != nil {
			publicURL = strings.TrimSpace(*body.PublicURL)
		}
		if publicURL == "" {
			writeJSONErrorCode(w, http.StatusBadRequest, "public_url_required", "set the public URL first: it forms the redirect URI")
			return
		}
		publicURL, err := config.ValidPublicURL(publicURL)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "public_url "+err.Error())
			return
		}
		scopes, err := canonicalList("oidc_scopes", body.Scopes)
		if err != nil {
			writeSettingsError(w, err)
			return
		}
		emails, err := canonicalList("oidc_allowed_emails", body.AllowedEmails)
		if err != nil {
			writeSettingsError(w, err)
			return
		}
		groups, err := canonicalList("oidc_allowed_groups", body.AllowedGroups)
		if err != nil {
			writeSettingsError(w, err)
			return
		}
		claim := strings.TrimSpace(body.GroupsClaim)
		if claim == "" {
			claim = "groups"
		}
		if !claimNamePattern.MatchString(claim) {
			writeJSONError(w, http.StatusBadRequest, "groups_claim must be a claim name such as groups")
			return
		}

		var enc string
		switch {
		case body.ClientSecret == nil:
			raw, err := store.SettingsGet(r.Context(), "oidc_client_secret_enc")
			if err == nil {
				_ = json.Unmarshal(raw, &enc)
			}
		case *body.ClientSecret != "":
			if len(*body.ClientSecret) > 1024 {
				writeJSONError(w, http.StatusBadRequest, "client_secret is too long")
				return
			}
			if enc, err = encryptSecret(ap.secret, *body.ClientSecret); err != nil {
				writeInternalError(w, "encrypt oidc client secret", err)
				return
			}
		}

		err = authn.StartTest(r.Context(), auth.PendingConfig{
			Issuer: issuer, ClientID: clientID, ClientSecretEnc: enc, Scopes: scopes,
			AllowedEmails: emails, AllowedGroups: groups, GroupsClaim: claim, PublicURL: publicURL,
		})
		if err != nil {
			writeInternalError(w, "oidc test start", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"login_url": "/auth/test-login"})
	}
}
