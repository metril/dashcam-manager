package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dashcam/internal/jobs"
)

// validJobStates is the set of values GET /api/jobs?state= accepts, beyond
// the empty string (meaning "any state").
var validJobStates = map[string]bool{
	string(jobs.StateQueued):    true,
	string(jobs.StateRunning):   true,
	string(jobs.StateDone):      true,
	string(jobs.StateFailed):    true,
	string(jobs.StateCancelled): true,
}

// handleEventsGet streams SSE "job" events (id, type, state, progress,
// message) — one per job state/progress change — for as long as the client
// stays connected. Modeled on meter-hunt's GET /api/stream. appCtx is the
// server's lifetime context: the loop also exits when it's done (server
// shutdown), not just when the client disconnects (r.Context().Done()) —
// otherwise http.Server.Shutdown would block on every open SSE connection
// until its own timeout instead of returning as soon as clients are told
// to go away.
func handleEventsGet(b *broker, appCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		ch := b.add()
		defer b.remove(ch)

		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()

		fmt.Fprint(w, "retry: 3000\n\n")
		flusher.Flush()

		for {
			select {
			case <-appCtx.Done():
				return
			case <-r.Context().Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "event: job\ndata: %s\n\n", msg)
				flusher.Flush()
			case <-keepalive.C:
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}

// handleJobsGet lists jobs, optionally filtered by ?state=, newest first,
// capped by ?limit= (default/max enforced by jobs.Queue.List).
func handleJobsGet(q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if state != "" && !validJobStates[state] {
			writeJSONError(w, http.StatusBadRequest, "state must be one of queued, running, done, failed, cancelled")
			return
		}
		limit := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
			limit = n
		}

		list, err := q.List(r.Context(), state, limit)
		if err != nil {
			writeInternalError(w, "jobs list", err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// handleJobGet returns a single job by id.
func handleJobGet(q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseJobID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		job, err := q.Get(r.Context(), id)
		if errors.Is(err, jobs.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "job not found")
			return
		}
		if err != nil {
			writeInternalError(w, "job get", err)
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

// handleJobCancel requests cancellation of a queued or running job. A
// no-op (still 200) if the job has already finished.
func handleJobCancel(q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseJobID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := q.Cancel(r.Context(), id); errors.Is(err, jobs.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "job not found")
			return
		} else if err != nil {
			writeInternalError(w, "job cancel", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "cancel_requested", "id": id})
	}
}

// handleJobDelete removes a finished job. 409 if the job exists but hasn't
// finished yet.
func handleJobDelete(q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseJobID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := q.Delete(r.Context(), id); errors.Is(err, jobs.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "job not found")
			return
		} else if err != nil {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func parseJobID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid job id")
	}
	return id, nil
}
