package dashcam_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dashcam/internal/dashcam"
	"dashcam/internal/dashcam/dashcamtest"
	"dashcam/internal/db"
)

func newTestClient(t *testing.T) (*dashcam.Client, *dashcamtest.Fake) {
	t.Helper()
	fake := dashcamtest.New(t.TempDir())
	t.Cleanup(fake.Close)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fake.AddKey("manager-test", pub)
	c := dashcam.New(fake.URL(), "manager-test", priv)
	return c, fake
}

// -- canonical string / RequestURI match --------------------------------

func TestCanonicalString_QueryEncodingMatchesRequestURI(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	if err := fake.AddSegment("front", "front/2026/09/11/rec1/segment_00001.mp4",
		[]byte("hello"), "2026-09-11T00:00:00", "2026-09-11T00:01:00",
		false, false, false, false); err != nil {
		t.Fatal(err)
	}

	// camera value chosen to exercise '/', space and '=' in the query.
	camera := "front cam/a=b"
	page, err := c.Manifest(ctx, "", camera, 0)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if page.Count != 0 {
		t.Fatalf("expected 0 matches for camera %q, got %d", camera, page.Count)
	}

	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	last := reqs[len(reqs)-1]
	v := url.Values{}
	v.Set("camera", camera)
	wantQS := "/api/ingest/manifest?" + v.Encode()
	gotQS := last.Path
	if last.RawQuery != "" {
		gotQS += "?" + last.RawQuery
	}
	if gotQS != wantQS {
		t.Fatalf("pathQS mismatch: got %q want %q", gotQS, wantQS)
	}
}

// -- clock skew retry -----------------------------------------------------

func TestClockSkewRetry(t *testing.T) {
	c, fake := newTestClient(t)
	// Server clock is 10 minutes ahead of the client's — beyond the 300s
	// skew tolerance, so the first attempt gets clock_skew and the client
	// must retry with a corrected timestamp. server_time in the error body
	// is a Python float (see APIError.ServerTime); the fake sends a genuine
	// float so this also exercises that decode.
	offset := 10 * time.Minute
	skewedNow := time.Now().Add(offset)
	fake.SetNow(func() time.Time { return skewedNow })

	_, err := c.Manifest(context.Background(), "", "", 0)
	if err != nil {
		t.Fatalf("Manifest after clock-skew retry: %v", err)
	}

	reqs := fake.Requests()
	if len(reqs) < 2 {
		t.Fatalf("expected at least 2 requests (initial + retry), got %d", len(reqs))
	}
	firstTs := parseTs(t, reqs[0].Ts)
	retryTs := parseTs(t, reqs[len(reqs)-1].Ts)

	// The retry's timestamp must actually be shifted by ~offset, not just
	// "different" — a broken skew computation (e.g. built from a
	// ServerTime that silently failed to decode and stayed 0) would still
	// produce *some* different timestamp on retry without producing the
	// *correct* one.
	gotShift := retryTs - firstTs
	wantShift := int64(offset / time.Second)
	if d := gotShift - wantShift; d < -2 || d > 2 {
		t.Fatalf("retry timestamp shift = %ds, want ~%ds (first=%d retry=%d)", gotShift, wantShift, firstTs, retryTs)
	}
}

func parseTs(t *testing.T, raw string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", raw, err)
	}
	return n
}

// -- nonce uniqueness / replay -------------------------------------------

func TestNonceUniqueAndReplayRejected(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	if _, err := c.Manifest(ctx, "", "", 0); err != nil {
		t.Fatalf("first manifest: %v", err)
	}
	if _, err := c.Manifest(ctx, "", "", 0); err != nil {
		t.Fatalf("second manifest: %v", err)
	}

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	if reqs[0].Nonce == reqs[1].Nonce {
		t.Fatalf("nonces should differ across calls: %q == %q", reqs[0].Nonce, reqs[1].Nonce)
	}

	// Hand-build a request that reuses a nonce the fake has already seen
	// (from reqs[1]) and confirm the server rejects it as a replay.
	dupReq, err := http.NewRequest("GET", fake.URL()+"/api/ingest/manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sign a fresh canonical string but deliberately reuse reqs[1]'s nonce.
	ts := time.Now().Unix()
	msg := testCanonicalString("GET", "/api/ingest/manifest", ts, reqs[1].Nonce, nil)
	sig := ed25519.Sign(c.Priv, msg)
	dupReq.Header.Set("X-Ingest-Key-Id", "manager-test")
	dupReq.Header.Set("X-Ingest-Timestamp", fmt.Sprint(ts))
	dupReq.Header.Set("X-Ingest-Nonce", reqs[1].Nonce)
	dupReq.Header.Set("X-Ingest-Signature", base64Std(sig))

	httpResp, err := http.DefaultClient.Do(dupReq)
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 replay, got %d", httpResp.StatusCode)
	}
}

// -- sticky next_since termination ----------------------------------------

func TestEachSegment_StickyCursorTermination(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	// Force the fake to answer every manifest request with the exact same
	// page (same paths, same next_since) — a server that legitimately
	// repeats next_since without new content. Without the sticky-cursor
	// guard this would page forever, re-delivering "a" and "b" to fn on
	// every iteration.
	fake.SetStickyManifestPage([]string{"front/a.mp4", "front/b.mp4"}, "stuck-cursor")

	var seen []string
	done := make(chan struct{})
	var cursor string
	var err error
	go func() {
		defer close(done)
		cursor, err = c.EachSegment(ctx, "", "", 10, func(s dashcam.Segment) error {
			seen = append(seen, s.Path)
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EachSegment did not terminate against a sticky next_since")
	}
	if err != nil {
		t.Fatalf("EachSegment: %v", err)
	}
	// fn must have been called exactly once per path — the guard has to
	// actually be *reached* (i.e. the second page must be fetched and
	// compared) rather than the loop never advancing past the first page.
	if len(seen) != 2 || seen[0] != "front/a.mp4" || seen[1] != "front/b.mp4" {
		t.Fatalf("expected exactly one pass over [a b], got %v", seen)
	}
	if cursor != "stuck-cursor" {
		t.Fatalf("expected terminal cursor %q, got %q", "stuck-cursor", cursor)
	}

	manifestCalls := 0
	for _, r := range fake.Requests() {
		if r.Path == "/api/ingest/manifest" {
			manifestCalls++
		}
	}
	if manifestCalls != 2 {
		t.Fatalf("expected exactly 2 manifest calls (page 1, then the repeat that trips the guard), got %d", manifestCalls)
	}
}

func TestEachSegment_LimitOnePagedAndTerminates(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	must(t, fake.AddSegment("front", "front/a.mp4", []byte("a"), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))
	must(t, fake.AddSegment("front", "front/b.mp4", []byte("b"), "2026-09-11T00:02:00", "2026-09-11T00:03:00", false, false, false, false))
	must(t, fake.AddSegment("front", "front/c.mp4", []byte("c"), "2026-09-11T00:04:00", "2026-09-11T00:05:00", false, false, false, false))

	var seen []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.EachSegment(ctx, "", "", 1, func(s dashcam.Segment) error {
			seen = append(seen, s.Path)
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EachSegment did not terminate (limit=1 paging)")
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 segments across pages, got %d: %v", len(seen), seen)
	}
}

func TestEachSegment_NextSinceNullEndsStream(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	must(t, fake.AddSegment("front", "front/n.mp4", []byte("n"), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))
	fake.SetForceNextSinceNull(true)

	// Start from a non-empty cursor so a bug that treats null next_since
	// as "" would visibly restart from the beginning (a second Manifest
	// call with since="") instead of stopping.
	startCursor := "2020-01-01T00:00:00"
	var seen []string
	cursor, err := c.EachSegment(ctx, startCursor, "", 10, func(s dashcam.Segment) error {
		seen = append(seen, s.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("EachSegment: %v", err)
	}
	if len(seen) != 1 || seen[0] != "front/n.mp4" {
		t.Fatalf("expected exactly [front/n.mp4], got %v", seen)
	}
	if cursor != startCursor {
		t.Fatalf("expected cursor to stay at %q on a null next_since, got %q", startCursor, cursor)
	}

	manifestCalls := 0
	for _, r := range fake.Requests() {
		if r.Path == "/api/ingest/manifest" {
			manifestCalls++
		}
	}
	if manifestCalls != 1 {
		t.Fatalf("expected exactly 1 manifest call (a null next_since must not trigger a since=\"\" restart), got %d", manifestCalls)
	}
}

// -- SidecarRelPath --------------------------------------------------------

func TestSidecarRelPath(t *testing.T) {
	rel, err := dashcam.SidecarRelPath("/api/ingest/file?path=front%2F2026%2F09%2F11%2Frec1%2Fsegment_00001.timestamp.json")
	if err != nil {
		t.Fatalf("SidecarRelPath: %v", err)
	}
	want := "front/2026/09/11/rec1/segment_00001.timestamp.json"
	if rel != want {
		t.Fatalf("got %q want %q", rel, want)
	}

	if _, err := dashcam.SidecarRelPath("/api/ingest/file?nope=1"); err == nil {
		t.Fatal("expected error for url with no path param")
	}
}

// -- 429 backoff + Range resume --------------------------------------------

func TestDownload_TooManyDownloadsBackoffThenSucceeds(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	content := strings.Repeat("x", 1000)
	must(t, fake.AddSegment("front", "front/seg.mp4", []byte(content), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))

	fake.SetMaxConcurrent(0) // force every attempt to 429 until we bump it back up

	dest := filepath.Join(t.TempDir(), "seg.mp4")
	resultCh := make(chan error, 1)
	go func() {
		_, err := c.Download(ctx, "front/seg.mp4", dest)
		resultCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	fake.SetMaxConcurrent(2) // let the next retry through

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Download did not complete after 429 backoff")
	}
}

func TestDownload_RangeResumeYieldsCorrectSHA256(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	content := strings.Repeat("abcdefgh", 4096) // 32KB, bigger than one read chunk
	rel := "front/seg2.mp4"
	must(t, fake.AddSegment("front", rel, []byte(content), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))

	fake.KillConnectionAfter(rel, 5000) // die partway through the first attempt

	dest := filepath.Join(t.TempDir(), "seg2.mp4")
	result, err := c.Download(ctx, rel, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if result.Bytes != int64(len(content)) {
		t.Fatalf("got %d bytes, want %d", result.Bytes, len(content))
	}
	wantSHA := sha256Hex([]byte(content))
	if result.SHA256 != wantSHA {
		t.Fatalf("sha256 mismatch: got %s want %s", result.SHA256, wantSHA)
	}

	got, err := readFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatal("downloaded content does not match source")
	}
}

func TestDownload_ContentRangeMismatchRestartsFromZero(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	content := strings.Repeat("m", 9000)
	rel := "front/mismatch.mp4"
	must(t, fake.AddSegment("front", rel, []byte(content), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))

	fake.KillConnectionAfter(rel, 3000)      // first attempt dies partway, ~3000 bytes land on disk
	fake.ForceNextRangeMismatch(rel, 999999) // the resume attempt gets a lying Content-Range

	dest := filepath.Join(t.TempDir(), "mismatch.mp4")
	result, err := c.Download(ctx, rel, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	wantSHA := sha256Hex([]byte(content))
	if result.SHA256 != wantSHA {
		t.Fatalf("sha256 mismatch (mismatched Content-Range should have forced a clean restart): got %s want %s", result.SHA256, wantSHA)
	}
	got, err := readFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Fatal("downloaded content does not match source after a Content-Range mismatch")
	}
}

// -- key save/load round trip ----------------------------------------------

func TestSaveLoadKey_RoundTrip(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "keys-test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	keyID, _, priv, err := dashcam.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ctx := context.Background()
	secret := "test-secret-key"
	if err := dashcam.SaveKey(ctx, d, secret, keyID, priv); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}

	gotID, gotPriv, ok, err := dashcam.LoadKey(ctx, d, secret)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if !ok {
		t.Fatal("LoadKey: ok=false, expected true")
	}
	if gotID != keyID {
		t.Fatalf("key id mismatch: got %q want %q", gotID, keyID)
	}
	if !priv.Equal(gotPriv) {
		t.Fatal("private key mismatch after round trip")
	}

	// Wrong secret must not silently succeed.
	if _, _, _, err := dashcam.LoadKey(ctx, d, "wrong-secret"); err == nil {
		t.Fatal("expected decrypt error with wrong secret")
	}
}

func TestLoadKey_NotSaved(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "keys-empty.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	_, _, ok, err := dashcam.LoadKey(context.Background(), d, "secret")
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for an empty settings table")
	}
}

// -- Keeper renew/release ---------------------------------------------------

func TestKeeper_RenewsAndReleases(t *testing.T) {
	c, fake := newTestClient(t)
	fake.DefaultTTL = 300 * time.Millisecond
	ctx := context.Background()

	lease, err := c.AcquireLease(ctx, 300*time.Millisecond, "test")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	keeper := c.KeepLease(ctx, lease, 300*time.Millisecond)
	time.Sleep(400 * time.Millisecond) // longer than ttl/3 renew interval

	status, err := c.MaintenanceStatus(ctx)
	if err != nil {
		t.Fatalf("MaintenanceStatus: %v", err)
	}
	if !status.Active {
		t.Fatal("expected lease still active thanks to Keeper renewals")
	}

	if err := keeper.Close(false); err != nil {
		t.Fatalf("Keeper.Close: %v", err)
	}
	// Idempotent.
	if err := keeper.Close(false); err != nil {
		t.Fatalf("second Keeper.Close: %v", err)
	}

	status, err = c.MaintenanceStatus(ctx)
	if err != nil {
		t.Fatalf("MaintenanceStatus after release: %v", err)
	}
	if status.Active {
		t.Fatal("expected lease released after Keeper.Close")
	}
}

// -- lease lifecycle basics -------------------------------------------------

func TestMaintenanceLifecycle(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()

	if _, err := c.MaintenanceStatus(ctx); err != nil {
		t.Fatalf("initial status: %v", err)
	}

	lease, err := c.AcquireLease(ctx, time.Second, "maint")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	if _, err := c.AcquireLease(ctx, time.Second, "again"); !dashcam.IsCode(err, "lease_active") {
		t.Fatalf("expected lease_active, got %v", err)
	}

	if err := c.RenewLease(ctx, lease.LeaseID, 2*time.Second); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}

	if err := c.ReleaseLease(ctx, lease.LeaseID, false); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	if err := c.ReleaseLease(ctx, lease.LeaseID, false); !dashcam.IsCode(err, "no_lease") {
		t.Fatalf("expected no_lease after already-released, got %v", err)
	}
}

// TestMaintenanceStatus_RemainingSFloat guards against LeaseStatus.RemainingS
// being int64: lease_status() computes remaining_s as expires_at -
// time.time(), which is essentially never an exact integer, and decoding a
// genuine JSON float into an int64 field fails outright (unlike the error
// body path, MaintenanceStatus's json.Decoder surfaces that failure as an
// error rather than silently swallowing it).
func TestMaintenanceStatus_RemainingSFloat(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()

	if _, err := c.AcquireLease(ctx, 5*time.Second, "float-check"); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // widen the odds remaining_s lands on a non-integer

	status, err := c.MaintenanceStatus(ctx)
	if err != nil {
		t.Fatalf("MaintenanceStatus (RemainingS float decode?): %v", err)
	}
	if !status.Active || status.RemainingS <= 0 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

// -- delete ------------------------------------------------------------

func TestDelete_ChecksumMismatchSkipped(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	must(t, fake.AddSegment("front", "front/d.mp4", []byte("data"), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))

	result, err := c.Delete(ctx, []string{"front/d.mp4"}, map[string]string{"front/d.mp4": "deadbeef"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(result.Deleted) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("expected 1 skip on checksum mismatch, got %+v", result)
	}
	if result.Skipped[0].Reason != "checksum_mismatch" {
		t.Fatalf("expected checksum_mismatch, got %q", result.Skipped[0].Reason)
	}

	result, err = c.Delete(ctx, []string{"front/d.mp4"}, map[string]string{"front/d.mp4": sha256Hex([]byte("data"))})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(result.Deleted) != 1 {
		t.Fatalf("expected 1 deleted, got %+v", result)
	}
}

// -- reprocess ---------------------------------------------------------

func TestReprocess_RequiresMaintenanceLease(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()

	_, err := c.StartReprocess(ctx, dashcam.ReprocessRequest{Camera: "front"})
	if !dashcam.IsCode(err, "maintenance_required") {
		t.Fatalf("expected maintenance_required, got %v", err)
	}

	if _, err := c.AcquireLease(ctx, time.Second, "reprocess"); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	started, err := c.StartReprocess(ctx, dashcam.ReprocessRequest{Camera: "front", Date: "2026-09-11"})
	if err != nil {
		t.Fatalf("StartReprocess: %v", err)
	}
	if started.Status != "started" {
		t.Fatalf("expected status=started, got %+v", started)
	}

	st, err := c.ReprocessStatus(ctx)
	if err != nil {
		t.Fatalf("ReprocessStatus: %v", err)
	}
	if st.State != "running" {
		t.Fatalf("expected running, got %+v", st)
	}
}

// TestReprocessStatus_TrackingIsStringLabel guards against
// ReprocessStatus.Tracking being bool: the dashboard reports a tracking
// *mode* label ("off"/"single_segment"/"per_recording"), and decoding a
// JSON string into a bool field is a hard decode error, not just a
// "wrong but present" value.
func TestReprocessStatus_TrackingIsStringLabel(t *testing.T) {
	c, fake := newTestClient(t)
	fake.ReprocessTracking = "single_segment"
	ctx := context.Background()

	if _, err := c.AcquireLease(ctx, time.Second, "tracking-check"); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if _, err := c.StartReprocess(ctx, dashcam.ReprocessRequest{Camera: "front"}); err != nil {
		t.Fatalf("StartReprocess: %v", err)
	}

	st, err := c.ReprocessStatus(ctx)
	if err != nil {
		t.Fatalf("ReprocessStatus (Tracking string decode?): %v", err)
	}
	if st.Tracking != "single_segment" {
		t.Fatalf("expected tracking=%q, got %q", "single_segment", st.Tracking)
	}
}

// TestEachSegmentPaged_InvokesOnPagePerPage exercises the dashcam_pull
// job's cursor-persistence hook: onPage must fire once per manifest page,
// each time with that page's next_since cursor, in the same order pages
// were delivered to fn.
func TestEachSegmentPaged_InvokesOnPagePerPage(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	must(t, fake.AddSegment("front", "front/a.mp4", []byte("a"), "2026-09-11T00:00:00", "2026-09-11T00:01:00", false, false, false, false))
	must(t, fake.AddSegment("front", "front/b.mp4", []byte("b"), "2026-09-11T00:02:00", "2026-09-11T00:03:00", false, false, false, false))
	must(t, fake.AddSegment("front", "front/c.mp4", []byte("c"), "2026-09-11T00:04:00", "2026-09-11T00:05:00", false, false, false, false))

	var pages []string
	var seen []string
	_, err := c.EachSegmentPaged(ctx, "", "", 1, func(cursor string) error {
		pages = append(pages, cursor)
		return nil
	}, func(s dashcam.Segment) error {
		seen = append(seen, s.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("EachSegmentPaged: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 segments, got %d: %v", len(seen), seen)
	}
	if len(pages) != 3 {
		t.Fatalf("expected onPage called once per page (3 pages of limit=1), got %d: %v", len(pages), pages)
	}
	if pages[len(pages)-1] != "2026-09-11T00:04:00" {
		t.Fatalf("expected final onPage cursor to be the last segment's start_time, got %q", pages[len(pages)-1])
	}
}
