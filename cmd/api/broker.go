package main

import (
	"encoding/json"
	"sync"

	"dashcam/internal/jobs"
)

// broker fans job/scan progress out to connected SSE clients (GET
// /api/events), modeled on meter-hunt's cmd/api/broker.go but fed directly
// by the in-process jobs.Queue's EventFunc rather than a Postgres LISTEN —
// dashcam-manager is single-process sqlite, so no cross-process fan-out is
// needed.
type broker struct {
	mu      sync.Mutex
	clients map[chan []byte]bool
}

func newBroker() *broker { return &broker{clients: map[chan []byte]bool{}} }

// add registers a new SSE client and returns its event channel. The
// channel is buffered so a burst of updates doesn't block the publisher;
// a slow/stalled client drops events rather than backing up the queue.
func (b *broker) add() chan []byte {
	ch := make(chan []byte, 32)
	b.mu.Lock()
	b.clients[ch] = true
	b.mu.Unlock()
	return ch
}

// clientCount returns the number of currently connected SSE clients
// (test-only helper, but harmless to keep in the production binary).
func (b *broker) clientCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

func (b *broker) remove(ch chan []byte) {
	b.mu.Lock()
	if b.clients[ch] {
		delete(b.clients, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *broker) publish(msg []byte) {
	b.mu.Lock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default: // drop for slow clients
		}
	}
	b.mu.Unlock()
}

// jobEventView is the JSON shape pushed for every "job" SSE event: the
// M7 brief's (id, type, state, progress, message).
type jobEventView struct {
	ID       int64   `json:"id"`
	Type     string  `json:"type"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Message  string  `json:"message,omitempty"`
}

// publishJob formats a jobs.Job as a "job" SSE event and publishes it. Wired
// as the jobs.Queue's EventFunc (queue.OnEvent) in main.go, so it fires on
// every state/progress change, already throttled by the queue itself.
func (b *broker) publishJob(j jobs.Job) {
	payload, err := json.Marshal(jobEventView{
		ID: j.ID, Type: j.Type, State: string(j.State),
		Progress: j.Progress, Message: j.Message,
	})
	if err != nil {
		return
	}
	b.publish(payload)
}
