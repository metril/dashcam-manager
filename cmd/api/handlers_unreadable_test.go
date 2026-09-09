package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSegmentsUnreadableGetListsMarkedSegments(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, _ := seedTrip(t, store, "2026-03-01T10:00:00Z")

	if err := store.SegmentMarkUnreadable(context.Background(), frontSegID, "Missing moov atom"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	var view unreadableSegmentsView
	rec := getJSON(t, mux, "/api/segments/unreadable", &view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	if len(view.Segments) != 1 {
		t.Fatalf("got %d unreadable segments, want 1: %+v", len(view.Segments), view.Segments)
	}
	if view.Segments[0].ID != frontSegID {
		t.Errorf("segment id = %d, want %d", view.Segments[0].ID, frontSegID)
	}
	if view.Segments[0].Reason != "Missing moov atom" {
		t.Errorf("reason = %q, want \"Missing moov atom\"", view.Segments[0].Reason)
	}
}

func TestSegmentsUnreadableRecheckScopedClearsAndEnqueues(t *testing.T) {
	mux, store, queue := newTestMuxWithStoreAndQueueNoWorkers(t)
	_, frontSegID, rearSegID := seedTrip(t, store, "2026-03-01T10:00:00Z")

	if err := store.SegmentMarkUnreadable(context.Background(), frontSegID, "Missing moov atom"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}
	if err := store.SegmentMarkUnreadable(context.Background(), rearSegID, "Corrupt header"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"ids": []int64{frontSegID}})
	req := httptest.NewRequest(http.MethodPost, "/api/segments/unreadable/recheck", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}

	seg, err := store.SegmentGet(context.Background(), frontSegID)
	if err != nil {
		t.Fatalf("SegmentGet: %v", err)
	}
	if seg.UnreadableReason != nil {
		t.Errorf("front segment still unreadable after scoped recheck: %v", seg.UnreadableReason)
	}
	seg, err = store.SegmentGet(context.Background(), rearSegID)
	if err != nil {
		t.Fatalf("SegmentGet: %v", err)
	}
	if seg.UnreadableReason == nil {
		t.Errorf("rear segment unreadable flag was cleared but recheck wasn't scoped to it")
	}

	jobs, err := queue.List(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("queue.List: %v", err)
	}
	found := false
	for _, j := range jobs {
		if j.Type == "thumbs" {
			found = true
		}
	}
	if !found {
		t.Errorf("no thumbs job enqueued by recheck, jobs=%+v", jobs)
	}
}

func TestSegmentsUnreadableRecheckAllClearsEverything(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	_, frontSegID, rearSegID := seedTrip(t, store, "2026-03-01T10:00:00Z")

	if err := store.SegmentMarkUnreadable(context.Background(), frontSegID, "Missing moov atom"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}
	if err := store.SegmentMarkUnreadable(context.Background(), rearSegID, "Corrupt header"); err != nil {
		t.Fatalf("SegmentMarkUnreadable: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/segments/unreadable/recheck", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", rec.Code, rec.Body)
	}

	n, err := store.SegmentsUnreadableCount(context.Background())
	if err != nil {
		t.Fatalf("SegmentsUnreadableCount: %v", err)
	}
	if n != 0 {
		t.Errorf("SegmentsUnreadableCount after recheck-all = %d, want 0", n)
	}
}

func TestSegmentsUnreadableRecheckNoopWhenNoneUnreadable(t *testing.T) {
	mux, store := newTestMuxWithStore(t)
	seedTrip(t, store, "2026-03-01T10:00:00Z")

	req := httptest.NewRequest(http.MethodPost, "/api/segments/unreadable/recheck", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (noop), body=%s", rec.Code, rec.Body)
	}
}
