package dashcam

// White-box: needs to shrink keeperReleaseMaxAttempts and stub out
// keeperReleaseSleep (both unexported) to keep the retry-with-backoff
// tests fast, so this lives in package dashcam rather than alongside
// client_test.go's black-box dashcam_test suite.

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"dashcam/internal/dashcam/dashcamtest"
)

// TestKeeperClose_RetriesReprocessRunningThenSucceeds is final-fix-wave
// item 3's covering test: the recorder can briefly answer a lease release
// with 409 reprocess_running while its own background thread still holds
// the lease busy for a moment after the job-visible terminal state
// lands — Close must retry (with backoff) rather than giving up and
// leaking the lease.
func TestKeeperClose_RetriesReprocessRunningThenSucceeds(t *testing.T) {
	origAttempts := keeperReleaseMaxAttempts
	origSleep := keeperReleaseSleep
	keeperReleaseMaxAttempts = 4
	keeperReleaseSleep = func(time.Duration) {} // no real backoff wait
	t.Cleanup(func() {
		keeperReleaseMaxAttempts = origAttempts
		keeperReleaseSleep = origSleep
	})

	fake := dashcamtest.New(t.TempDir())
	t.Cleanup(fake.Close)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fake.AddKey("manager-test", pub)
	c := New(fake.URL(), "manager-test", priv)
	ctx := context.Background()

	lease, err := c.AcquireLease(ctx, 30*time.Second, "reprocess")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	// The first 2 release attempts fail as if the recorder's reprocess
	// thread still holds the lease; the 3rd (within keeperReleaseMaxAttempts)
	// must succeed.
	fake.ForceReleaseError(2, 409, "reprocess_running")

	keeper := c.KeepLease(ctx, lease, 30*time.Second)
	if err := keeper.Close(false); err != nil {
		t.Fatalf("Close after 2 transient reprocess_running failures = %v, want nil (retried through)", err)
	}

	status, err := c.MaintenanceStatus(ctx)
	if err != nil {
		t.Fatalf("MaintenanceStatus: %v", err)
	}
	if status.Active {
		t.Fatal("expected the lease to actually be released after Close retried past reprocess_running")
	}
}

// TestKeeperClose_GivesUpAfterMaxReprocessRunningRetries covers the other
// half: once the recorder keeps answering reprocess_running past
// keeperReleaseMaxAttempts, Close must give up and return that error
// rather than retrying forever.
func TestKeeperClose_GivesUpAfterMaxReprocessRunningRetries(t *testing.T) {
	origAttempts := keeperReleaseMaxAttempts
	origSleep := keeperReleaseSleep
	keeperReleaseMaxAttempts = 3
	keeperReleaseSleep = func(time.Duration) {}
	t.Cleanup(func() {
		keeperReleaseMaxAttempts = origAttempts
		keeperReleaseSleep = origSleep
	})

	fake := dashcamtest.New(t.TempDir())
	t.Cleanup(fake.Close)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fake.AddKey("manager-test", pub)
	c := New(fake.URL(), "manager-test", priv)
	ctx := context.Background()

	lease, err := c.AcquireLease(ctx, 30*time.Second, "reprocess")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	// Every release attempt fails — more than keeperReleaseMaxAttempts.
	fake.ForceReleaseError(10, 409, "reprocess_running")

	keeper := c.KeepLease(ctx, lease, 30*time.Second)
	err = keeper.Close(false)
	if err == nil {
		t.Fatal("expected Close to give up and return an error after exhausting retries")
	}
	if !IsCode(err, "reprocess_running") {
		t.Fatalf("Close error = %v, want a reprocess_running APIError", err)
	}
}
