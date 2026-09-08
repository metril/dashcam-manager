package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dashcam/internal/db"
)

// handleSegmentMediaGet serves GET /api/segments/{id}/media: the original
// mp4 for a segment, streamed via http.ServeContent (so Range requests —
// which every native <video> element issues for seeking — and conditional
// GETs work for free). The on-disk path is source.root_path + rel_path;
// resolveMediaPath rejects anything that would resolve outside root_path
// even though rel_path is scanner-produced (not raw user input), since a
// corrupt DB row or future bug elsewhere is not a license to serve
// arbitrary files off the host.
func handleSegmentMediaGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}

		media, err := store.SegmentMediaGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "segment not found")
			return
		}
		if err != nil {
			writeInternalError(w, "segment media get", err)
			return
		}

		path, err := resolveMediaPath(media.SourceRoot, media.RelPath)
		if err != nil {
			writeInternalError(w, "resolve media path", err)
			return
		}

		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSONError(w, http.StatusNotFound, "media file not found on disk")
				return
			}
			writeInternalError(w, "open media file", err)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			writeInternalError(w, "stat media file", err)
			return
		}
		if info.IsDir() {
			writeJSONError(w, http.StatusNotFound, "media file not found on disk")
			return
		}

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", mediaETag(info.Size(), info.ModTime().Unix()))
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
	}
}

// mediaETag builds a weak ETag from size+mtime — cheap to compute (no file
// hashing) and stable across requests/restarts since it's derived purely
// from stat() output, matching what http.ServeContent needs for
// conditional requests and Range validation.
func mediaETag(size int64, mtimeUnix int64) string {
	return fmt.Sprintf(`W/"%x-%x"`, size, mtimeUnix)
}

// resolveMediaPath joins root and rel, then verifies the cleaned result is
// still lexically inside root — rejecting any rel_path (".." components, a
// symlink-like absolute path, etc.) that would otherwise escape the source
// root. It works purely lexically (filepath.Clean, no symlink resolution),
// which is sufficient here since rel_path values are written by the
// scanner, not supplied by request callers.
func resolveMediaPath(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("resolve media path: empty source root")
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Join(cleanRoot, rel)

	sep := string(filepath.Separator)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+sep) {
		return "", fmt.Errorf("resolve media path: rel_path %q escapes source root %q", rel, root)
	}
	return joined, nil
}

// handleTripTimelineGet serves GET /api/trips/{id}/timeline: per-segment
// detection-density info for the trip's timeline strip.
func handleTripTimelineGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid trip id")
			return
		}

		timeline, err := store.TripTimelineGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "trip not found")
			return
		}
		if err != nil {
			writeInternalError(w, "trip timeline get", err)
			return
		}
		writeJSON(w, http.StatusOK, timeline)
	}
}
