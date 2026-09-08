package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// countingWriter tracks bytes written through it, calling onWrite after
// each chunk so a caller can drive progress reporting without buffering
// the whole file.
type countingWriter struct {
	onWrite func(n int)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(len(p))
	}
	return len(p), nil
}

// copyResult is what streamCopy reports about a completed copy.
type copyResult struct {
	Bytes  int64
	SHA256 string
}

// streamCopy copies src to dest by writing to a temp file in dest's
// directory first and renaming it into place only once the copy has fully
// succeeded — so a crash or cancellation mid-copy never leaves a partial
// file at the real destination path for a later dedupe check to
// mistakenly trust. sha256 is always computed while copying (cheap given
// the data is already being read) even when verify mode is "size" —
// segments.sha256 records it regardless, and a later run with
// verify=sha256 can then dedupe against it without re-reading the file.
// onBytes (optional) is called with each chunk's size as it's written, for
// progress reporting.
func streamCopy(ctx context.Context, src, dest string, onBytes func(n int)) (copyResult, error) {
	in, err := os.Open(src)
	if err != nil {
		return copyResult{}, fmt.Errorf("ingest: open source %s: %w", src, err)
	}
	defer in.Close()

	destDir := filepath.Dir(dest)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return copyResult{}, fmt.Errorf("ingest: mkdir %s: %w", destDir, err)
	}

	tmp, err := os.CreateTemp(destDir, ".ingest-*.tmp")
	if err != nil {
		return copyResult{}, fmt.Errorf("ingest: create temp file in %s: %w", destDir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup of the temp file on any early return; a
	// successful path renames it away first, so this Remove becomes a
	// silently-ignored no-op (file no longer exists) in the success case.
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	progress := &countingWriter{onWrite: onBytes}
	dst := io.MultiWriter(tmp, hasher, progress)

	n, copyErr := io.Copy(dst, contextReader{ctx: ctx, r: in})
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyResult{}, fmt.Errorf("ingest: copy %s -> %s: %w", src, dest, copyErr)
	}
	if closeErr != nil {
		return copyResult{}, fmt.Errorf("ingest: close temp file %s: %w", tmpPath, closeErr)
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		return copyResult{}, fmt.Errorf("ingest: rename %s -> %s: %w", tmpPath, dest, err)
	}

	return copyResult{Bytes: n, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// hashFile computes the sha256 of an existing file without holding it all
// in memory.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyFile confirms that the file at path has size wantSize and, when
// mode == "sha256", that its content hash equals wantSHA256. Exposed
// (rather than kept private) so tests can corrupt a destination file after
// a real ingest and confirm this rejects it directly, without needing to
// simulate corruption mid-copy.
func VerifyFile(path string, wantSize int64, wantSHA256, mode string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("ingest: verify %s: %w", path, err)
	}
	if info.Size() != wantSize {
		return fmt.Errorf("ingest: verify %s: size mismatch: got %d, want %d", path, info.Size(), wantSize)
	}
	if mode != verifySHA256 {
		return nil
	}
	got, _, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("ingest: verify %s: hash: %w", path, err)
	}
	if got != wantSHA256 {
		return fmt.Errorf("ingest: verify %s: sha256 mismatch: got %s, want %s", path, got, wantSHA256)
	}
	return nil
}

// contextReader wraps an io.Reader, returning ctx.Err() instead of reading
// further once ctx is done — so a long copy of a big segment file notices
// job cancellation promptly instead of only between whole-file copies.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr contextReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// copySidecarBestEffort copies one sidecar file (timestamp/detections/gps
// JSON) alongside a segment's video, skipping quietly if the source
// sidecar doesn't exist (not every segment has every sidecar) and
// overwriting the destination unconditionally if it's missing or a
// different size — sidecars are small and not individually tracked in the
// `ingests` table, so a coarse size check (rather than a full verify
// round-trip) is enough to avoid needless re-copies on a resumed job.
func copySidecarBestEffort(ctx context.Context, src, dest string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if destInfo, err := os.Stat(dest); err == nil && destInfo.Size() == srcInfo.Size() {
		return nil
	}
	_, err = streamCopy(ctx, src, dest, nil)
	return err
}
