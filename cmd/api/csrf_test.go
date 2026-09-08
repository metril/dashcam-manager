package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func passThroughHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestCSRFProtectAllowsGET(t *testing.T) {
	h := csrfProtect(passThroughHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/trips", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET (even with a foreign Origin) = %d, want 200", rec.Code)
	}
}

func TestCSRFProtectRejectsMutatingRequestWithoutJSONContentType(t *testing.T) {
	h := csrfProtect(passThroughHandler())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/jobs/1/cancel", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s without Content-Type = %d, want 415 (body: %s)", method, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCSRFProtectRejectsFormEncodedContentType(t *testing.T) {
	// The classic drive-by CSRF vector: a plain HTML form can only send
	// x-www-form-urlencoded, multipart/form-data, or text/plain — never
	// application/json — so this must be refused even though it "has" a
	// Content-Type.
	h := csrfProtect(passThroughHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/sources/scan", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form-encoded POST = %d, want 415", rec.Code)
	}
}

func TestCSRFProtectAllowsJSONContentTypeWithCharset(t *testing.T) {
	h := csrfProtect(passThroughHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/sources/scan", nil)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("application/json; charset=utf-8 POST = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestCSRFProtectRejectsCrossOriginRequest(t *testing.T) {
	h := csrfProtect(passThroughHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/sources/scan", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "dashcam.local:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestCSRFProtectAllowsSameOriginRequest(t *testing.T) {
	h := csrfProtect(passThroughHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/sources/scan", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "dashcam.local:8080"
	req.Header.Set("Origin", "http://dashcam.local:8080")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("same-origin POST = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestCSRFProtectAllowsMissingOrigin(t *testing.T) {
	// No Origin header (e.g. curl, server-to-server, some same-origin
	// navigations) is allowed through — its absence isn't itself
	// suspicious for a non-CORS app with no session cookies.
	h := csrfProtect(passThroughHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/sources/scan", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST with no Origin header = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}
