package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/media"
)

// onDemandGenerateTimeout bounds a synchronous ffmpeg run triggered by a
// cache-miss GET (a slow/failing extraction must not hang the request
// forever; the background "thumbs" job is what normally pre-warms this
// cache well before a user browses to it).
const onDemandGenerateTimeout = 30 * time.Second

// handleSegmentThumbGet serves GET /api/segments/{id}/thumb.jpg: the
// cached thumbnail if present, else generates it synchronously so a trip
// browsed before its background "thumbs" job has caught up still shows a
// real frame instead of a broken image.
func handleSegmentThumbGet(store *db.DB, cacheDir, device string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}

		out := media.ThumbPath(cacheDir, id)
		if !fileExists(out) {
			seg, err := store.SegmentThumbInfoGet(r.Context(), id)
			if errors.Is(err, db.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "segment not found")
				return
			}
			if err != nil {
				writeInternalError(w, "segment thumb info", err)
				return
			}

			genCtx, cancel := context.WithTimeout(context.Background(), onDemandGenerateTimeout)
			genErr := media.Thumbnail(genCtx, filepath.Join(seg.RootPath, seg.RelPath), out, media.ThumbOptions{
				Mode: getHWMode(), Device: device, AtSec: thumbAtSec(seg.DurationS), Width: 480,
			})
			cancel()

			status := "done"
			if genErr != nil {
				status = "failed"
			}
			if setErr := store.SegmentSetThumbStatus(r.Context(), id, status); setErr != nil {
				log.Printf("[api] set thumb status %d: %v", id, setErr)
			}
			if genErr != nil {
				log.Printf("[api] on-demand thumbnail %d: %v", id, genErr)
				writeJSONError(w, http.StatusNotFound, "thumbnail not available")
				return
			}
			if seg.TripID != nil {
				if cerr := assignTripCover(r.Context(), store, *seg.TripID); cerr != nil {
					log.Printf("[api] trip cover %d: %v", *seg.TripID, cerr)
				}
			}
		}

		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, out)
	}
}

// spriteFile selects which half of a sprite pair (jpg tile sheet or vtt
// cue file) an on-demand handler serves.
type spriteFile int

const (
	spriteJPG spriteFile = iota
	spriteVTT
)

// handleSegmentSpriteJPGGet serves GET /api/segments/{id}/sprite.jpg.
func handleSegmentSpriteJPGGet(store *db.DB, cacheDir, device string) http.HandlerFunc {
	return handleSegmentSprite(store, cacheDir, device, spriteJPG)
}

// handleSegmentSpriteVTTGet serves GET /api/segments/{id}/sprite.vtt.
func handleSegmentSpriteVTTGet(store *db.DB, cacheDir, device string) http.HandlerFunc {
	return handleSegmentSprite(store, cacheDir, device, spriteVTT)
}

// handleSegmentSprite serves whichever half of the sprite pair `want`
// selects, generating both files together on a cache miss (media.Sprite
// always writes the jpg and vtt as one unit) — so whichever of
// /sprite.jpg or /sprite.vtt a client happens to request first triggers
// generation, and the other is then already cached.
func handleSegmentSprite(store *db.DB, cacheDir, device string, want spriteFile) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid segment id")
			return
		}

		jpgPath, vttPath := media.SpritePaths(cacheDir, id)
		wantPath := jpgPath
		if want == spriteVTT {
			wantPath = vttPath
		}

		if !fileExists(wantPath) {
			seg, err := store.SegmentThumbInfoGet(r.Context(), id)
			if errors.Is(err, db.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "segment not found")
				return
			}
			if err != nil {
				writeInternalError(w, "segment thumb info", err)
				return
			}

			duration := 30.0
			if seg.DurationS != nil && *seg.DurationS > 0 {
				duration = *seg.DurationS
			}

			genCtx, cancel := context.WithTimeout(context.Background(), onDemandGenerateTimeout)
			genErr := media.Sprite(genCtx, filepath.Join(seg.RootPath, seg.RelPath), jpgPath, vttPath, media.SpriteOptions{
				Mode: getHWMode(), Device: device, SegmentDurationS: duration,
			})
			cancel()

			if genErr != nil {
				log.Printf("[api] on-demand sprite %d: %v", id, genErr)
				writeJSONError(w, http.StatusNotFound, "sprite not available")
				return
			}
		}

		if want == spriteVTT {
			w.Header().Set("Content-Type", "text/vtt")
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, wantPath)
	}
}

// fileExists reports whether path names a regular, readable file (a
// generation-in-progress ".tmp" file at the same base path doesn't count,
// since media.Thumbnail/Sprite only rename their finished output into
// place — so this never observes a partial write).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
