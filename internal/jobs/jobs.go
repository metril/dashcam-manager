// Package jobs implements a DB-backed background job queue on top of the
// `jobs` table (internal/db/schema.go). A Queue enqueues work, a pool of
// worker goroutines polls for queued rows and dispatches them to handlers
// registered by job type, and every state/progress change is pushed to an
// optional EventFunc (cmd/api wires this to an SSE broker).
package jobs

import (
	"context"
	"encoding/json"
)

// State is the lifecycle of a job row, matching the values this package
// writes to the `state` column.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Job is a snapshot of one row from the jobs table.
type Job struct {
	ID              int64           `json:"id"`
	Type            string          `json:"type"`
	State           State           `json:"state"`
	Priority        int             `json:"priority"`
	Params          json.RawMessage `json:"params,omitempty"`
	Progress        float64         `json:"progress"`
	Message         string          `json:"message,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	CreatedAt       string          `json:"created_at"`
	StartedAt       *string         `json:"started_at,omitempty"`
	FinishedAt      *string         `json:"finished_at,omitempty"`
	CancelRequested bool            `json:"cancel_requested"`
}

// ReportFunc lets a running Handler push a progress update (0..1) and a
// short status message back to the queue, which persists it (throttled)
// and fans it out via the queue's EventFunc. Only safe to call from within
// the handler's own goroutine.
type ReportFunc func(progress float64, message string)

// Handler runs one job to completion. It must respect ctx cancellation:
// the queue cancels ctx once the job's cancel_requested flag is observed
// (polled every second), and a Handler that ignores ctx will keep running
// past a requested cancellation. The returned result (if non-nil) is
// JSON-marshalled into the job's result_json column.
type Handler func(ctx context.Context, job *Job, report ReportFunc) (result any, err error)
