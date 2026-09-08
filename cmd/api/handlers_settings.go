package main

import (
	"encoding/json"
	"net/http"

	"dashcam/internal/db"
)

// handleSettingsGet returns every settings row as a flat JSON object of
// key -> value (value already being JSON, so it's embedded raw rather than
// double-encoded as a string).
func handleSettingsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all, err := store.SettingsGetAll(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(all); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
		}
	}
}

// handleSettingsPut merges the JSON object in the request body into
// settings: every key present is upserted, keys not present are left
// untouched. Returns the full merged settings object.
func handleSettingsPut(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if len(patch) > 0 {
			if err := store.SettingsSetMany(r.Context(), patch); err != nil {
				writeJSONError(w, http.StatusInternalServerError, err)
				return
			}
		}
		all, err := store.SettingsGetAll(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(all); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
		}
	}
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
