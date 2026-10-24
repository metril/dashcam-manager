// handlers_hls.go: on-demand HLS fallback (final-fix-wave item 11 / the
// design spec's HLS fallback requirement) for browsers that can't play the
// source HEVC directly. GET /api/segments/{id}/media (handlers_media.go)
// serves the original file for a browser whose canPlayType says it can
// decode hvc1/hev1; these two routes serve an on-the-fly h264 fMP4 HLS
// transcode for one that can't. See app/frontend's player component for
// the canPlayType check that decides which URL to use.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/media"
)

// hlsGenerateTimeout bounds a synchronous on-demand HLS transcode. A
// segment is always ~30s, but a software (libx264 "veryfast") fallback
// transcode of a 4K source can still take a while on a constrained host —
// generous compared to onDemandGenerateTimeout's 30s (a single-frame
// thumbnail extract) since this decodes and re-encodes the whole clip.
const hlsGenerateTimeout = 3 * time.Minute

// ensureHLS resolves segID's source file, and — via the same genGroup
// singleflight used by thumbnails/sprites, keyed by the HLS output
// directory — generates its HLS playlist/segments if they aren't already
// cached, returning the directory once master.m3u8 is present there.
// Re-checks for the master playlist inside the singleflight function in
// case a concurrent request (two players opening the same segment at
// once) already finished generating it while this call was waiting.
func ensureHLS(reqCtx context.Context, store *db.DB, cacheDir string, devs media.Devices, segID int64) (string, error) {
	segMedia, err := store.SegmentMediaGet(reqCtx, segID)
	if errors.Is(err, db.ErrNotFound) {
		return "", errSegmentNotFound
	}
	if err != nil {
		return "", err
	}
	if segMedia.ArchivedAt != nil {
		return "", errSegmentArchived
	}

	outDir := media.HLSDir(cacheDir, segID)
	masterPath := filepath.Join(outDir, media.HLSMasterName)
	if fileExists(masterPath) {
		return outDir, nil
	}

	in := filepath.Join(segMedia.SourceRoot, segMedia.RelPath)
	_, err, _ = genGroup.Do(outDir, func() (any, error) {
		if fileExists(masterPath) {
			return nil, nil
		}
		// Deliberately detached from reqCtx (context.Background(), not
		// reqCtx) — this generation is shared via genGroup across every
		// concurrent request for the same outDir, and only the request
		// that happens to win the singleflight race actually runs this
		// closure. If it were derived from that one winning request's
		// context, the whole transcode (and every other request
		// currently blocked on the same genGroup.Do call, waiting for
		// its result) would be aborted the moment *that* request's own
		// connection was cancelled — e.g. its browser tab closed —
		// even though the other waiters still want the result and the
		// work is equally cacheable for the next request regardless.
		// hlsGenerateTimeout is still an upper bound so a stuck ffmpeg
		// process can't hang forever.
		genCtx, cancel := context.WithTimeout(context.Background(), hlsGenerateTimeout)
		defer cancel()
		if err := acquireFFmpegSlot(genCtx); err != nil {
			return nil, err
		}
		defer releaseFFmpegSlot()
		hwMode := getHWMode()
		return nil, media.GenerateHLS(genCtx, in, outDir, media.HLSOptions{Mode: hwMode, Device: devs.For(hwMode)})
	})
	if err != nil {
		return "", err
	}
	return outDir, nil
}

// errSegmentNotFound / errSegmentArchived let ensureHLS's callers map to
// the right HTTP status without string-matching error text.
var (
	errSegmentNotFound = errors.New("segment not found")
	errSegmentArchived = errors.New("segment archived")
)

// writeHLSError maps ensureHLS's error to an HTTP response: 404 for an
// unknown/archived segment or a missing source file (nothing further
// retrying would fix), 500 for everything else (ffmpeg not installed, a
// corrupt file, a hardware pipeline that failed even after the software
// fallback).
func writeHLSError(w http.ResponseWriter, in string, err error) {
	switch {
	case errors.Is(err, errSegmentNotFound):
		writeJSONError(w, http.StatusNotFound, "segment not found")
	case errors.Is(err, errSegmentArchived):
		writeJSONError(w, http.StatusNotFound, "segment archived (original deleted after verified ingest)")
	case in != "" && !fileExists(in):
		writeJSONError(w, http.StatusNotFound, "source file not found")
	default:
		writeInternalError(w, "generate hls", err)
	}
}

// handleSegmentHLSMasterGet serves GET /api/segments/{id}/hls/master.m3u8,
// generating the segment's HLS playlist on demand if it isn't already
// cached.
func handleSegmentHLSMasterGet(store *db.DB, cacheDir string, devs devicesFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}

		outDir, err := ensureHLS(r.Context(), store, cacheDir, devs(), id)
		if err != nil {
			log.Printf("[api] hls master %d: %v", id, err)
			in := ""
			if m, gerr := store.SegmentMediaGet(r.Context(), id); gerr == nil {
				in = filepath.Join(m.SourceRoot, m.RelPath)
			}
			writeHLSError(w, in, err)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		// Short max-age, not "immutable" like the .m4s/init segments
		// below: the playlist is what a player re-fetches to discover
		// whether generation has produced (or re-produced, e.g. after a
		// cache clear) content — caching it for a year would mean a
		// player that already has a stale/failed playlist never learns a
		// fresh one exists.
		w.Header().Set("Cache-Control", "public, max-age=60")
		http.ServeFile(w, r, filepath.Join(outDir, media.HLSMasterName))
	}
}

// handleSegmentHLSFileGet serves GET /api/segments/{id}/hls/{file}: the
// fMP4 init segment or one .m4s media segment referenced by master.m3u8.
// {file} is validated against media.IsValidHLSFile before ever touching
// the filesystem — the only names GenerateHLS could have produced — so a
// crafted path component can't escape the HLS cache directory.
func handleSegmentHLSFileGet(store *db.DB, cacheDir string, devs devicesFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}
		file := r.PathValue("file")
		if !media.IsValidHLSFile(file) {
			writeJSONError(w, http.StatusBadRequest, "invalid hls file name")
			return
		}

		outDir, err := ensureHLS(r.Context(), store, cacheDir, devs(), id)
		if err != nil {
			log.Printf("[api] hls file %d/%s: %v", id, file, err)
			in := ""
			if m, gerr := store.SegmentMediaGet(r.Context(), id); gerr == nil {
				in = filepath.Join(m.SourceRoot, m.RelPath)
			}
			writeHLSError(w, in, err)
			return
		}

		path := filepath.Join(outDir, file)
		if !fileExists(path) {
			writeJSONError(w, http.StatusNotFound, "hls file not found")
			return
		}

		if filepath.Ext(file) == ".m4s" || file == media.HLSInitName {
			w.Header().Set("Content-Type", "video/mp4")
		}
		// Long max-age but deliberately not "immutable": unlike a
		// thumbnail/sprite (whose cache key already includes nothing that
		// could change without also changing the segment id) these files
		// live under a fixed per-segment URL that a cache clear + re-
		// generation could, in principle, repopulate with different bytes
		// (e.g. a hw-mode change, or a future GenerateHLS parameter
		// change) — "immutable" tells a browser to never revalidate even
		// on a user-initiated refresh, which could then keep serving
		// stale content indefinitely. A long max-age still avoids
		// re-fetching on every play within a session; dropping
		// "immutable" just means a real refresh can notice new content.
		w.Header().Set("Cache-Control", "public, max-age=31536000")
		http.ServeFile(w, r, path)
	}
}
