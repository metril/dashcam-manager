package dashcam

// White-box: needs to shrink maxDownloadAttempts (unexported) to keep the
// "every attempt fails" case fast, so this lives in package dashcam rather
// than alongside client_test.go's black-box dashcam_test suite.

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dashcam/internal/dashcam/dashcamtest"
)

// TestDownload_ExhaustedRetriesDoesNotRenameTruncatedFile is the direct
// regression test for the bug where Download, after exhausting every
// retry attempt against a connection that never completes, still renamed
// the (truncated) ".part" file to destPath and returned it as a success —
// because the final "did this actually succeed" check only looked at
// `written == 0`, which a truncated-but-nonzero partial download doesn't
// satisfy. The fix tracks an explicit `succeeded` flag set only on the
// loop's err == nil break.
func TestDownload_ExhaustedRetriesDoesNotRenameTruncatedFile(t *testing.T) {
	orig := maxDownloadAttempts
	maxDownloadAttempts = 2 // keep the real 1s/2s/4s... backoff bounded
	t.Cleanup(func() { maxDownloadAttempts = orig })

	fake := dashcamtest.New(t.TempDir())
	t.Cleanup(fake.Close)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fake.AddKey("manager-test", pub)

	content := strings.Repeat("z", 4000)
	rel := "front/flaky.mp4"
	if err := fake.AddSegment("front", rel, []byte(content), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	c := New(fake.URL(), "manager-test", priv)

	// Keep re-arming a one-shot connection kill so every attempt (not just
	// the first) dies partway through, leaving a permanently truncated
	// .part file behind — the scenario the fix has to survive.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fake.KillConnectionAfter(rel, 500)
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	dest := filepath.Join(t.TempDir(), "flaky.mp4")
	_, downloadErr := c.Download(context.Background(), rel, dest)
	close(stop)
	wg.Wait()

	if downloadErr == nil {
		t.Fatal("expected Download to report an error after exhausting retries against a connection that never completes")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destPath must not exist after a failed download (a truncated .part must never be renamed into place as if it succeeded); stat error: %v", statErr)
	}
}
