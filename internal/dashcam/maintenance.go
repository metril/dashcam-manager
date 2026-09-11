package dashcam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// LeaseStatus is GET /api/ingest/maintenance's shape.
type LeaseStatus struct {
	Active     bool   `json:"active"`
	LeaseID    string `json:"lease_id"`
	ExpiresAt  string `json:"expires_at"`
	RemainingS int64  `json:"remaining_s"`
	Reason     string `json:"reason"`
}

// Lease is what POST /api/ingest/maintenance returns on success.
type Lease struct {
	LeaseID    string `json:"lease_id"`
	ExpiresAt  string `json:"expires_at"`
	RemainingS int64  `json:"remaining_s"`
	Reason     string `json:"reason"`
	Recording  bool   `json:"recording"`
	Inference  bool   `json:"inference"`
}

// MaintenanceStatus fetches the current lease state.
func (c *Client) MaintenanceStatus(ctx context.Context) (LeaseStatus, error) {
	resp, err := c.do(ctx, "GET", "/api/ingest/maintenance", nil)
	if err != nil {
		return LeaseStatus{}, err
	}
	defer resp.Body.Close()
	var st LeaseStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return LeaseStatus{}, fmt.Errorf("dashcam: decode maintenance status: %w", err)
	}
	return st, nil
}

// AcquireLease acquires a maintenance lease, pausing recording/inference on
// the recorder for up to ttl (0 uses the server's default).
func (c *Client) AcquireLease(ctx context.Context, ttl time.Duration, reason string) (Lease, error) {
	body := map[string]any{"reason": reason}
	if ttl > 0 {
		body["ttl_s"] = int64(ttl / time.Second)
	}
	raw, _ := json.Marshal(body)
	resp, err := c.do(ctx, "POST", "/api/ingest/maintenance", raw)
	if err != nil {
		return Lease{}, err
	}
	defer resp.Body.Close()
	var lease Lease
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		return Lease{}, fmt.Errorf("dashcam: decode lease: %w", err)
	}
	return lease, nil
}

// RenewLease extends an active lease's expiry.
func (c *Client) RenewLease(ctx context.Context, id string, ttl time.Duration) error {
	body := map[string]any{"lease_id": id}
	if ttl > 0 {
		body["ttl_s"] = int64(ttl / time.Second)
	}
	raw, _ := json.Marshal(body)
	resp, err := c.do(ctx, "POST", "/api/ingest/maintenance/renew", raw)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// ReleaseLease ends a lease, resuming recording/inference. force releases
// even while a reprocess job the lease is protecting is still running.
func (c *Client) ReleaseLease(ctx context.Context, id string, force bool) error {
	raw, _ := json.Marshal(map[string]any{"lease_id": id, "force": force})
	resp, err := c.do(ctx, "POST", "/api/ingest/maintenance/release", raw)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// Keeper keeps a maintenance lease alive by renewing it in the background
// on roughly a ttl/3 cadence, so a caller holding the lease for a long-
// running job doesn't have to manage the timer itself.
type Keeper struct {
	c      *Client
	lease  Lease
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	closed   bool
	closeErr error
}

// KeepLease starts a Keeper for lease, renewing every ttl/3 until Close is
// called. ttl <= 0 falls back to a 5-minute cadence (60s/3).
func (c *Client) KeepLease(ctx context.Context, lease Lease, ttl time.Duration) *Keeper {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	kctx, cancel := context.WithCancel(ctx)
	k := &Keeper{c: c, lease: lease, cancel: cancel, done: make(chan struct{})}
	go k.run(kctx, ttl/3)
	return k
}

func (k *Keeper) run(ctx context.Context, interval time.Duration) {
	defer close(k.done)
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = k.c.RenewLease(ctx, k.lease.LeaseID, 0)
		}
	}
}

// Close stops the renew loop and releases the lease. Idempotent — a second
// call returns the same result as the first without releasing again.
func (k *Keeper) Close(force bool) error {
	k.mu.Lock()
	if k.closed {
		defer k.mu.Unlock()
		return k.closeErr
	}
	k.closed = true
	k.mu.Unlock()

	k.cancel()
	<-k.done
	err := k.c.ReleaseLease(context.Background(), k.lease.LeaseID, force)

	k.mu.Lock()
	k.closeErr = err
	k.mu.Unlock()
	return err
}
