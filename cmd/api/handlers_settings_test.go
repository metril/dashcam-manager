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
