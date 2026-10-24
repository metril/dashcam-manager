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

	"golang.org/x/sync/singleflight"

	"dashcam/internal/db"
	"dashcam/internal/media"
)

// onDemandGenerateTimeout bounds a synchronous ffmpeg run triggered by a
// cache-miss GET (a slow/failing extraction must not hang the request
// forever; the background "thumbs" job is what normally pre-warms this
// cache well before a user browses to it).
const onDemandGenerateTimeout = 30 * time.Second

// genGroup coalesces concurrent on-demand generation requests for the same
// output (e.g. two browser tabs, or a thumbnail request racing the
// background thumbs job, both hitting the same segment's cache miss at
// once) so only one ffmpeg run happens per key — followers block on Do and
// get the same result instead of each starting their own redundant (and,
// pre-this-fix, colliding-on-tmp-filename) ffmpeg process. Shared between
// thumbnails and sprites: keys are cache paths, which are already unique
// per segment and kind.
var genGroup singleflight.Group

// handleSegmentThumbGet serves GET /api/segments/{id}/thumb.jpg: the
// cached thumbnail if present, else generates it synchronously so a trip
// browsed before its background "thumbs" job has caught up still shows a
// real frame instead of a broken image.
func handleSegmentThumbGet(store *db.DB, cacheDir string, devs devicesFn) http.HandlerFunc {
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

			in := filepath.Join(seg.RootPath, seg.RelPath)
			genErr := generateThumbOnDemand(r.Context(), store, out, in, id, seg.TripID, devs(), thumbAtSec(seg.DurationS))
			if genErr != nil {
				log.Printf("[api] on-demand thumbnail %d: %v", id, genErr)
				writeGenerationError(w, "thumbnail", in, genErr)
				return
			}
		}

		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, out)
	}
}

// generateThumbOnDemand runs (via genGroup, keyed by out) the same
// generate-and-record-status logic the "thumbs" job uses for one segment,
// so a cache-miss GET behaves identically whether the job or a request
// gets there first. Re-checks fileExists(out) inside the singleflight
// function in case a concurrent request (or the background job) already
// finished generating it while this call was waiting to run/join.
func generateThumbOnDemand(reqCtx context.Context, store *db.DB, out, in string, segID int64, tripID *int64, devs media.Devices, atSec float64) error {
	_, err, _ := genGroup.Do(out, func() (any, error) {
		if fileExists(out) {
			return nil, nil
		}

		genCtx, cancel := context.WithTimeout(reqCtx, onDemandGenerateTimeout)
		defer cancel()
		// genCtx is derived from reqCtx (so it's cancelled if the
		// triggering request is, e.g. the client disconnects) but must
		// not be cancelled just because *this* request's context is
		// (other requests may be waiting on the same singleflight key) —
		// context.WithTimeout still ties genCtx's cancellation to
		// reqCtx's, which is acceptable here since reqCtx.Done() firing
		// mid-generation only shortens, never corrupts, the run.
		if err := acquireFFmpegSlot(genCtx); err != nil {
			return nil, err
		}
		hwMode := getHWMode()
		genErr := media.Thumbnail(genCtx, in, out, media.ThumbOptions{
			Mode: hwMode, Device: devs.For(hwMode), AtSec: atSec, Width: 480,
		})
		releaseFFmpegSlot()

		if genErr != nil {
			if reason, ok := media.UnreadableReason(genErr); ok {
				if setErr := store.SegmentMarkUnreadable(context.Background(), segID, reason); setErr != nil {
					log.Printf("[api] mark unreadable %d: %v", segID, setErr)
				}
			} else if setErr := store.SegmentSetThumbStatus(context.Background(), segID, "failed"); setErr != nil {
				log.Printf("[api] set thumb status %d: %v", segID, setErr)
			}
			return nil, genErr
		}
		if setErr := store.SegmentSetThumbStatus(context.Background(), segID, "done"); setErr != nil {
			log.Printf("[api] set thumb status %d: %v", segID, setErr)
		}
		if tripID != nil {
			if cerr := assignTripCover(context.Background(), store, *tripID); cerr != nil {
				log.Printf("[api] trip cover %d: %v", *tripID, cerr)
			}
		}
		return nil, nil
	})
	return err
}

// spriteFile selects which half of a sprite pair (jpg tile sheet or vtt
// cue file) an on-demand handler serves.
type spriteFile int

const (
	spriteJPG spriteFile = iota
	spriteVTT
)

// handleSegmentSpriteJPGGet serves GET /api/segments/{id}/sprite.jpg.
func handleSegmentSpriteJPGGet(store *db.DB, cacheDir string, devs devicesFn) http.HandlerFunc {
	return handleSegmentSprite(store, cacheDir, devs, spriteJPG)
}

// handleSegmentSpriteVTTGet serves GET /api/segments/{id}/sprite.vtt.
func handleSegmentSpriteVTTGet(store *db.DB, cacheDir string, devs devicesFn) http.HandlerFunc {
	return handleSegmentSprite(store, cacheDir, devs, spriteVTT)
}

// handleSegmentSprite serves whichever half of the sprite pair `want`
// selects, generating both files together on a cache miss (media.Sprite
// always writes the jpg and vtt as one unit) — so whichever of
// /sprite.jpg or /sprite.vtt a client happens to request first triggers
// generation, and the other is then already cached.
func handleSegmentSprite(store *db.DB, cacheDir string, devs devicesFn, want spriteFile) http.HandlerFunc {
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

			in := filepath.Join(seg.RootPath, seg.RelPath)
			duration := 30.0
			if seg.DurationS != nil && *seg.DurationS > 0 {
				duration = *seg.DurationS
			}

			genErr := generateSpriteOnDemand(r.Context(), store, id, jpgPath, vttPath, in, devs(), duration)
			if genErr != nil {
				log.Printf("[api] on-demand sprite %d: %v", id, genErr)
				writeGenerationError(w, "sprite", in, genErr)
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

// generateSpriteOnDemand mirrors generateThumbOnDemand for the jpg+vtt
// sprite pair, coalesced on jpgPath so a /sprite.jpg and /sprite.vtt
// request landing at the same time for the same segment share one ffmpeg
// run.
func generateSpriteOnDemand(reqCtx context.Context, store *db.DB, segID int64, jpgPath, vttPath, in string, devs media.Devices, duration float64) error {
	_, err, _ := genGroup.Do(jpgPath, func() (any, error) {
		if fileExists(jpgPath) && fileExists(vttPath) {
			return nil, nil
		}

		genCtx, cancel := context.WithTimeout(reqCtx, onDemandGenerateTimeout)
		defer cancel()
		if err := acquireFFmpegSlot(genCtx); err != nil {
			return nil, err
		}
		hwMode := getHWMode()
		genErr := media.Sprite(genCtx, in, jpgPath, vttPath, media.SpriteOptions{
			Mode: hwMode, Device: devs.For(hwMode), SegmentDurationS: duration,
			OnDurationCorrected: func(correctedS float64) {
				if setErr := store.SegmentSetDurationS(context.Background(), segID, correctedS); setErr != nil {
					log.Printf("[api] correct duration_s for segment %d: %v", segID, setErr)
				}
			},
		})
		releaseFFmpegSlot()
		return nil, genErr
	})
	return err
}

// writeGenerationError responds to a failed on-demand thumbnail/sprite
// generation: 404 when the underlying source video is missing from disk
// (a segment the scanner indexed but that's since been deleted/moved off
// the read-only SSD — no amount of retrying fixes that, and it's the same
// "not found" shape as an unknown segment id), 500 for every other
// failure (ffmpeg not installed, a corrupt/unsupported file, a QSV/VAAPI
// pipeline that failed even after the software fallback, etc. — all
// server-side problems, not the client asking for something that doesn't
// exist).
func writeGenerationError(w http.ResponseWriter, kind, sourcePath string, genErr error) {
	if !fileExists(sourcePath) {
		writeJSONError(w, http.StatusNotFound, "source file not found")
		return
	}
	writeInternalError(w, "generate "+kind, genErr)
}

// fileExists reports whether path names a regular, readable file (a
// generation-in-progress temp file at a different name doesn't count,
// since media.Thumbnail/Sprite only rename their finished output into
// place — so this never observes a partial write).
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
