package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func putSettings(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSettingsPutMergesKnownKeys(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"workers": 6}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got["workers"]) != "6" {
		t.Fatalf("workers = %s, want 6", got["workers"])
	}
}

func TestSettingsPutRejectsUnknownKey(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"totally_unknown_key": "x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutRejectsWrongType(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"workers": "not-a-number"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}

	rec = putSettings(t, mux, `{"workers": 1.5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-integer workers: status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

// TestSettingsPutRejectsNegativeDashcamPollInterval covers fix round 1's
// minor item: dashcam_poll_interval_s must reject a negative value rather
// than silently accepting one that dashcamPollInterval would then treat
// as "disabled" (intervalS <= 0) — a caller passing -1 almost certainly
// meant something other than "disabled" (that's 0).
func TestSettingsPutRejectsNegativeDashcamPollInterval(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"dashcam_poll_interval_s": -1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}

	rec = putSettings(t, mux, `{"dashcam_poll_interval_s": 0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashcam_poll_interval_s=0: status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	rec = putSettings(t, mux, `{"dashcam_poll_interval_s": 60}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashcam_poll_interval_s=60: status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutRejectsNullBody(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `null`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutRejectsNonObjectBody(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `[1,2,3]`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("array body: status = %d, want 400, body=%s", rec.Code, rec.Body)
	}

	rec = putSettings(t, mux, `"just a string"`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("string body: status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutRejectsOversizedBody(t *testing.T) {
	mux := newTestMux(t)

	// One valid key with a very large value; must exceed maxSettingsBodyBytes.
	var buf bytes.Buffer
	buf.WriteString(`{"timezone": "`)
	buf.WriteString(strings.Repeat("a", maxSettingsBodyBytes+1))
	buf.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPut, "/api/settings", &buf)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutValidatesSSDUUID(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"ssd_uuid": "not a valid uuid!"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ssd_uuid: status = %d, want 400, body=%s", rec.Code, rec.Body)
	}

	rec = putSettings(t, mux, `{"ssd_uuid": ""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty ssd_uuid (no override — DASHCAM_SSD_UUID applies): status = %d, want 200, body=%s", rec.Code, rec.Body)
	}

	rec = putSettings(t, mux, `{"ssd_uuid": "1234-5678-ABCD"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid ssd_uuid: status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutDoesNotAllowSchemaVersionOverwrite(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"schema_version": 999}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (schema_version is system-managed), body=%s", rec.Code, rec.Body)
	}
}

// ---- detection_filter (Task 3) ----

func TestSettingsPutDetectionFilterRoundTrips(t *testing.T) {
	mux := newTestMux(t)

	body := `{"detection_filter": {"min_conf": 0.3, "objects": true, "plates": false, "vehicles": true, "classes": ["car", "person"]}}`
	rec := putSettings(t, mux, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	var put map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	var putFilter map[string]any
	if err := json.Unmarshal(put["detection_filter"], &putFilter); err != nil {
		t.Fatalf("decode detection_filter from PUT response: %v", err)
	}
	if putFilter["min_conf"] != 0.3 || putFilter["plates"] != false {
		t.Fatalf("PUT response detection_filter = %+v, want min_conf=0.3, plates=false", putFilter)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, req)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body=%s", getRec.Code, getRec.Body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	var gotFilter map[string]any
	if err := json.Unmarshal(got["detection_filter"], &gotFilter); err != nil {
		t.Fatalf("decode detection_filter from GET response: %v", err)
	}
	if gotFilter["min_conf"] != 0.3 || gotFilter["plates"] != false {
		t.Fatalf("GET after PUT detection_filter = %+v, want it to round-trip PUT's value", gotFilter)
	}
	classes, ok := gotFilter["classes"].([]any)
	if !ok || len(classes) != 2 || classes[0] != "car" || classes[1] != "person" {
		t.Fatalf("GET after PUT detection_filter classes = %v, want [car person]", gotFilter["classes"])
	}
}

func TestSettingsPutDetectionFilterClampsMinConf(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"detection_filter": {"min_conf": 3.0, "objects": true, "plates": true, "vehicles": true, "classes": []}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var filter map[string]any
	if err := json.Unmarshal(got["detection_filter"], &filter); err != nil {
		t.Fatalf("decode detection_filter: %v", err)
	}
	if filter["min_conf"] != 1.0 {
		t.Fatalf("min_conf = %v, want clamped to 1", filter["min_conf"])
	}
}

func TestSettingsPutDetectionFilterRejectsUnknownClass(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"detection_filter": {"min_conf": 0.5, "objects": true, "plates": true, "vehicles": true, "classes": ["spaceship"]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsPutDetectionFilterRejectsNonObject(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"detection_filter": "not-an-object"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body)
	}
}

// TestSettingsPutDetectionFilterPartialJSONUsesDefaults is fix-round-1
// coverage for IMPORTANT 2: PUTting {"detection_filter": {"min_conf":0.7}}
// (a client only overriding the confidence floor) must persist
// objects/plates/vehicles=true (the default), not false — a bare JSON
// decode into DetectionFilter{} would previously turn every omitted bool
// off and silently disable burn-in / hide every track.
func TestSettingsPutDetectionFilterPartialJSONUsesDefaults(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"detection_filter": {"min_conf": 0.7}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var filter map[string]any
	if err := json.Unmarshal(got["detection_filter"], &filter); err != nil {
		t.Fatalf("decode detection_filter: %v", err)
	}
	if filter["min_conf"] != 0.7 {
		t.Fatalf("min_conf = %v, want 0.7", filter["min_conf"])
	}
	if filter["objects"] != true || filter["plates"] != true || filter["vehicles"] != true {
		t.Fatalf("detection_filter = %+v, want objects/plates/vehicles all true (defaults for omitted fields)", filter)
	}
}

// TestSettingsPutDetectionFilterNullUsesDefaults covers an explicit JSON
// null value for detection_filter itself (as opposed to a merge-PUT that
// simply omits the key, which leaves the stored value untouched): it
// should persist the full default filter, not an all-zero one.
func TestSettingsPutDetectionFilterNullUsesDefaults(t *testing.T) {
	mux := newTestMux(t)

	rec := putSettings(t, mux, `{"detection_filter": null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var filter map[string]any
	if err := json.Unmarshal(got["detection_filter"], &filter); err != nil {
		t.Fatalf("decode detection_filter: %v", err)
	}
	if filter["min_conf"] != 0.5 || filter["objects"] != true || filter["plates"] != true || filter["vehicles"] != true {
		t.Fatalf("detection_filter (from null) = %+v, want the full default filter", filter)
	}
}

// TestValidateSettingsPatchRejectsUnhandledKind is the MINOR fix-round-1
// coverage: knownSettings mapping a key to a settingsKind with no
// validating case must fail loudly rather than silently skipping
// validation and persisting an unchecked value.
func TestValidateSettingsPatchRejectsUnhandledKind(t *testing.T) {
	const bogusKind settingsKind = 99
	knownSettings["__bogus_test_key__"] = bogusKind
	defer delete(knownSettings, "__bogus_test_key__")

	err := validateSettingsPatch(map[string]json.RawMessage{"__bogus_test_key__": json.RawMessage(`"x"`)})
	if err == nil {
		t.Fatal("validateSettingsPatch() = nil, want error for an unhandled settingsKind")
	}
}
