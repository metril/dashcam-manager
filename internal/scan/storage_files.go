// storage_files.go (local-storage plan task 5) upserts a storage_files row
// for every regular file found under a share/local-backed source's root —
// video and non-video alike (exports, sidecars), not just the
// segment_NNNNN.mp4 files walker.go's walkRoot indexes into segments.
// Never runs for a kind=ssd source (the SSD is never a storage; see the
// plan's Context section) — scanStorageFiles no-ops when the source's
// ShareID is nil.
package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dashcam/internal/db"
)

// localProbeFilePrefix/writeProbeFilePrefix mirror the scratch-file names
// internal/share.ProbeLocal and internal/ingest.WriteProbe create (and
// remove) to test a directory is writable — see share/mount.go's
// localProbeFilePrefix doc comment, which already documents that any
// future walker touching a storage root directly (this one) must skip
// these. Duplicated as literals here rather than imported: internal/share
// doesn't export its constant, and internal/ingest already imports
// internal/share so importing either back from internal/scan would risk a
// cycle for no real benefit (these are fixed, documented literals).
const (
	localProbeFilePrefix = ".dashcam-local-probe-"
	writeProbeFilePrefix = ".dashcam-write-probe-"
)

// shouldSkipStorageFile reports whether name (a base filename) should never
// become a storage_files row: the usual junk (IsJunk), the write-probe
// scratch files above, and any "hidden dotfile ending .tmp" — the shape of
// internal/ingest/copy.go's in-progress-copy temp file (".ingest-*.tmp").
func shouldSkipStorageFile(name string) bool {
	if IsJunk(name) {
		return true
	}
	if strings.HasPrefix(name, localProbeFilePrefix) || strings.HasPrefix(name, writeProbeFilePrefix) {
		return true
	}
	if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
		return true
	}
	return false
}

// scanStorageFiles walks rootPath in full and upserts every regular file it
// finds as a storage_files row under sourceID's backing share, then prunes
// rows not seen by this walk. No-op when sourceID's source isn't
// share/local-backed (ShareID nil, i.e. kind=ssd — never a storage).
func scanStorageFiles(ctx context.Context, database *db.DB, sourceID int64, rootPath string, seenAt time.Time) error {
	src, err := database.SourceGet(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("scan: storage files: source %d: %w", sourceID, err)
	}
	if src.ShareID == nil {
		return nil
	}
	shareID := *src.ShareID
	seenAtStr := seenAt.UTC().Format(time.RFC3339Nano)

	if err := walkStorageDir(ctx, database, shareID, rootPath, rootPath, seenAtStr); err != nil {
		return fmt.Errorf("scan: storage files: walk %s: %w", rootPath, err)
	}
	if _, err := database.StorageFilesPruneNotSeenSince(ctx, shareID, seenAtStr); err != nil {
		return fmt.Errorf("scan: storage files: prune share %d: %w", shareID, err)
	}
	return nil
}

// walkStorageDir recursively walks dir (rootPath, then each subdirectory),
// upserting one storage_files batch per directory as soon as that
// directory's entries are read — rather than accumulating the whole tree in
// memory first — before recursing into its subdirectories.
func walkStorageDir(ctx context.Context, database *db.DB, shareID int64, rootPath, dir, seenAt string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Gone since the parent directory was listed (race with a
			// concurrent delete/unmount) — nothing to index here.
			return nil
		}
		return err
	}

	var batch []db.StorageFileUpsertRow
	var subdirs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if IsJunk(name) {
				continue
			}
			subdirs = append(subdirs, filepath.Join(dir, name))
			continue
		}
		if !e.Type().IsRegular() {
			continue // symlinks/sockets/devices are never a "file copy"
		}
		if shouldSkipStorageFile(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Vanished between ReadDir and Info (temp file being cleaned
			// up, etc.) — skip; a later scan settles it either way.
			continue
		}
		rel, err := filepath.Rel(rootPath, filepath.Join(dir, name))
		if err != nil {
			return err
		}
		batch = append(batch, db.StorageFileUpsertRow{
			RelPath: filepath.ToSlash(rel),
			Bytes:   info.Size(),
			Mtime:   info.ModTime().Unix(),
		})
	}
	if len(batch) > 0 {
		if err := database.StorageFileUpsert(ctx, shareID, batch, seenAt); err != nil {
			return err
		}
	}
	for _, sd := range subdirs {
		if err := walkStorageDir(ctx, database, shareID, rootPath, sd, seenAt); err != nil {
			return err
		}
	}
	return nil
}
