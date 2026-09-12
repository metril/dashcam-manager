package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
//
// Durability: the temp file is fsync'd before it's closed, and dest's
// directory is fsync'd again after the rename — without both, a power
// loss right after streamCopy returns "success" could leave the rename
// only in the directory's page cache (never reaching disk), so a crash
// could make a file this function reported as copied vanish, or (worse)
// leave a stale directory entry the next boot's dedupe check trusts
// without the data actually being durable. Both matter given this
// package's whole purpose is enabling SSD-original cleanup on the
// strength of "the destination is verified".
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
	if copyErr != nil {
		tmp.Close()
		return copyResult{}, fmt.Errorf("ingest: copy %s -> %s: %w", src, dest, copyErr)
	}

	// Sync before Close: Close alone doesn't guarantee the written data
	// itself has reached disk (only that the fd is released), and this
	// data is what a later verify/dedupe pass is going to trust.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return copyResult{}, fmt.Errorf("ingest: sync temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return copyResult{}, fmt.Errorf("ingest: close temp file %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		return copyResult{}, fmt.Errorf("ingest: rename %s -> %s: %w", tmpPath, dest, err)
	}

	if err := fsyncDir(destDir); err != nil {
		return copyResult{}, fmt.Errorf("ingest: fsync dir %s: %w", destDir, err)
	}

	return copyResult{Bytes: n, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// fsyncDir opens dir and fsyncs it, to persist a rename's directory-entry
// change (see streamCopy's doc comment).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// HashFile computes the sha256 of an existing file without holding it all
// in memory. Exported (task 6) so cmd/api's hash_backfill job can stream
// storage_files/segments hashes through the same implementation VerifyFile
// uses below, rather than duplicating it.
func HashFile(path string) (string, int64, error) {
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
	got, _, err := HashFile(path)
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

// copySidecars copies every sidecar file (timestamp/detections/gps JSON)
// that exists alongside relPath's video from sourceRoot to targetMount,
// verifying each destination's size against the source right after (a
// missing source sidecar is fine — not every segment has every sidecar —
// but a copy that lands with the wrong size is not). Sidecars aren't
// individually tracked in the `ingests` table the way the video is, so
// this check — called before IngestMarkVerified — is the only thing
// standing between "the segment is marked verified" and its sidecars
// having silently failed to make it to the destination; a caller must
// treat a non-nil error here as a whole-segment ingest failure, not a
// best-effort warning.
func copySidecars(ctx context.Context, sourceRoot, targetMount, relPath string) error {
	base := strings.TrimSuffix(relPath, ".mp4")
	for _, ext := range sidecarExts {
		src := filepath.Join(sourceRoot, base+ext)
		srcInfo, statErr := os.Stat(src)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("stat sidecar %s: %w", src, statErr)
		}

		dest := filepath.Join(targetMount, base+ext)
		if destInfo, err := os.Stat(dest); err == nil && destInfo.Size() == srcInfo.Size() {
			continue // already there (matching size) from a previous run — dedupe
		}

		if _, err := streamCopy(ctx, src, dest, nil); err != nil {
			return fmt.Errorf("copy sidecar %s: %w", src, err)
		}
		destInfo, err := os.Stat(dest)
		if err != nil {
			return fmt.Errorf("verify sidecar %s: %w", dest, err)
		}
		if destInfo.Size() != srcInfo.Size() {
			return fmt.Errorf("verify sidecar %s: size mismatch: got %d, want %d", dest, destInfo.Size(), srcInfo.Size())
		}
	}
	return nil
}
