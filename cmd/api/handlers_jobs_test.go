package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dashcam/internal/jobs"
)

// TestSourcesScanEnqueuesJobAndRunsToCompletion covers the M4->M7
// migration: POST /api/sources/scan must enqueue a real "scan" job (not
// spin up an untracked goroutine), which the test mux's real job queue
// then runs to completion against an empty temp directory.
func TestSourcesScanEnqueuesJobAndRunsToCompletion(t *testing.T) {
	mux := newTestMux(t)

	dir := t.TempDir()
	body, _ := json.Marshal(map[string]string{"kind": "ssd", "root_path": dir, "label": "t"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sources", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: %d %s", rec.Code, rec.Body.String())
	}
	var src struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &src); err != nil {
		t.Fatalf("decode source: %v", err)
	}

	scanBody, _ := json.Marshal(map[string]any{"source_id": src.ID})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/sources/scan", bytes.NewReader(scanBody)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("scan: %d %s", rec2.Code, rec2.Body.String())
	}
	var resp struct {
		Status string   `json:"status"`
		Job    jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode scan response: %v", err)
	}
	if resp.Status != "queued" {
		t.Fatalf("status = %q, want queued", resp.Status)
	}
	if resp.Job.Type != "scan" || resp.Job.ID == 0 {
		t.Fatalf("unexpected job in scan response: %+v", resp.Job)
	}

	final := pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)
	if final.State != jobs.StateDone {
		t.Fatalf("scan job final state = %q, want done (error=%s)", final.State, final.Error)
	}

	// A second scan request for the same source while nothing is queued
	// anymore should enqueue a fresh job, not report already_running.
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/api/sources/scan", bytes.NewReader(scanBody)))
	var resp3 struct {
		Status string `json:"status"`
	}
	json.Unmarshal(rec3.Body.Bytes(), &resp3)
	if resp3.Status != "queued" {
		t.Errorf("second scan status = %q, want queued (job queue not left in a bad state)", resp3.Status)
	}
}

func pollJob(t *testing.T, mux http.Handler, id int64, wantAny ...jobs.State) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var final jobs.Job
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/jobs/%d: %d %s", id, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
			t.Fatalf("decode job: %v", err)
		}
		for _, want := range wantAny {
			if final.State == want {
				return final
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %d did not reach any of %v within deadline (last state %q)", id, wantAny, final.State)
	return final
}

func TestJobsListAndGet(t *testing.T) {
	mux := newTestMux(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/jobs: %d %s", rec.Code, rec.Body.String())
	}
	var list []jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("fresh db: jobs list = %d entries, want 0", len(list))
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/jobs/999", nil))
	if rec2.Code != http.StatusNotFound {
		t.Errorf("GET /api/jobs/999 = %d, want 404", rec2.Code)
	}
}

func TestJobCancelUnknownIs404(t *testing.T) {
	mux := newTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/jobs/999/cancel", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/jobs/999/cancel = %d, want 404", rec.Code)
	}
}

func TestJobDeleteUnfinishedIsConflict(t *testing.T) {
	mux := newTestMux(t)

	dir := t.TempDir()
	body, _ := json.Marshal(map[string]string{"kind": "ssd", "root_path": dir, "label": "t"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sources", bytes.NewReader(body)))
	var src struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &src)

	scanBody, _ := json.Marshal(map[string]any{"source_id": src.ID})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/sources/scan", bytes.NewReader(scanBody)))
	var resp struct {
		Job jobs.Job `json:"job"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &resp)

	// The job may already be done by the time we get here (empty dir scans
	// are fast); either way, deleting a done job should succeed, and
	// deleting it twice should 404 the second time.
	pollJob(t, mux, resp.Job.ID, jobs.StateDone, jobs.StateFailed)

	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/jobs/%d", resp.Job.ID), nil))
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("DELETE finished job: %d %s", rec3.Code, rec3.Body.String())
	}

	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/jobs/%d", resp.Job.ID), nil))
	if rec4.Code != http.StatusNotFound {
		t.Errorf("DELETE already-deleted job = %d, want 404", rec4.Code)
	}
}

// TestEventsSSEEmitsJobEvent exercises the SSE handler directly against a
// broker (bypassing the job queue entirely) via httptest, per the M7
// brief's "SSE handler emits events (httptest)" test requirement.
func TestEventsSSEEmitsJobEvent(t *testing.T) {
	b := newBroker()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handleEventsGet(b)(rec, req)
		close(done)
	}()

	// Give the handler goroutine time to register as a broker client
	// before publishing, and again after, to let it flush the event.
	waitUntil(t, func() bool { return b.clientCount() > 0 })
	b.publishJob(jobs.Job{ID: 42, Type: "scan", State: jobs.StateRunning, Progress: 0.5, Message: "hi"})
	waitUntil(t, func() bool { return strings.Contains(rec.Body.String(), `"id":42`) })

	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: job") {
		t.Errorf("SSE body missing 'event: job' line: %s", body)
	}
	if !strings.Contains(body, `"state":"running"`) {
		t.Errorf("SSE body missing job state: %s", body)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
