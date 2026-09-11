package dashcam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// LeaseStatus is GET /api/ingest/maintenance's shape. RemainingS is
// float64 (not int64) because lease_status() computes it as
// expires_at - time.time(), a Python float with a fractional part —
// decoding it as int64 fails outright whenever a lease is actually
// active, which is the only time this field is interesting.
type LeaseStatus struct {
	Active     bool    `json:"active"`
	LeaseID    string  `json:"lease_id"`
	ExpiresAt  string  `json:"expires_at"`
	RemainingS float64 `json:"remaining_s"`
	Reason     string  `json:"reason"`
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
	ttl    time.Duration
	cancel context.CancelFunc
	done   chan struct{}

	once     sync.Once
	closeErr error
}

// KeepLease starts a Keeper for lease, renewing with ttl every ttl/3 until
// Close is called. ttl <= 0 falls back to a 60-second ttl (so a 20-second
// renew cadence).
func (c *Client) KeepLease(ctx context.Context, lease Lease, ttl time.Duration) *Keeper {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	kctx, cancel := context.WithCancel(ctx)
	k := &Keeper{c: c, lease: lease, ttl: ttl, cancel: cancel, done: make(chan struct{})}
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
			_ = k.c.RenewLease(ctx, k.lease.LeaseID, k.ttl)
		}
	}
}

// keeperReleaseMaxAttempts bounds Close's retry loop (1 initial try + 3
// retries) when the recorder answers release with 409 reprocess_running —
// the recorder refuses to release a maintenance lease while its own
// background reprocess thread still holds it busy for a moment after the
// job-visible terminal state (done/error) lands; a few retries with
// backoff ride out that window instead of giving up and leaking the
// lease. A var, not a const, so a test can shrink it.
var keeperReleaseMaxAttempts = 4

// keeperReleaseBackoff is attempt (0-indexed retry number) -> sleep
// duration, matching download.go's downloadBackoff shape (1s, 2s, 4s...).
func keeperReleaseBackoff(retry int) time.Duration {
	d := time.Second
	for i := 0; i < retry; i++ {
		d *= 2
	}
	return d
}

// keeperReleaseSleep is time.Sleep, overridable in tests so a retry loop
// doesn't actually wait real seconds.
var keeperReleaseSleep = time.Sleep

// Close stops the renew loop and releases the lease. Idempotent — a
// concurrent or repeated call blocks until (or observes) the same result
// as the one call that actually does the work, via sync.Once's
// happens-before guarantee (a second caller who didn't run the release
// still sees closeErr's final value once Do returns, so there's no window
// where it could read a still-zero error).
//
// A release that fails with 409 reprocess_running (the recorder refusing
// to let go of the lease while it still thinks a reprocess run is active)
// is retried up to keeperReleaseMaxAttempts times with backoff before
// giving up — see that var's doc comment. Any other error (including a
// non-transient failure) is returned immediately, unretried.
func (k *Keeper) Close(force bool) error {
	k.once.Do(func() {
		k.cancel()
		<-k.done
		var err error
		for attempt := 0; attempt < keeperReleaseMaxAttempts; attempt++ {
			if attempt > 0 {
				keeperReleaseSleep(keeperReleaseBackoff(attempt - 1))
			}
			err = k.c.ReleaseLease(context.Background(), k.lease.LeaseID, force)
			if err == nil || !IsCode(err, "reprocess_running") {
				break
			}
		}
		k.closeErr = err
	})
	return k.closeErr
}
