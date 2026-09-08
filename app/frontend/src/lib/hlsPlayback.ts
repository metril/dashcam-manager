// hlsPlayback.ts: picks between the original HEVC source and the
// on-demand HLS fallback (cmd/api/handlers_hls.go) for a <video> element,
// per the design spec's HLS fallback requirement (final-fix-wave item
// 11). Most browsers on a reasonably modern OS can play HEVC natively
// (Safari, Edge/Chrome on Windows with the HEVC extension, ...) — for
// those, playback is unchanged: the original mp4 is streamed directly
// (Range-request seeking, no transcode). Only a browser that reports it
// can't decode hvc1/hev1 falls back to the transcoded HLS stream.
import Hls from 'hls.js'
import { segmentHLSMasterUrl, segmentMediaUrl } from './api'

// canPlayNativeHEVC reports whether this browser's <video> element claims
// it can decode the HEVC profile dashcam-manager's recorder produces
// (Main profile, level 5.1 — see the design spec / README's "3840x2160
// HEVC" note). Checks both the "hvc1" and "hev1" codec-tag spellings
// (ffmpeg/mp4 muxers vary on which one they stamp; different browsers can
// be picky about which they recognize) — either reporting non-empty means
// native playback should work. Memoized: canPlayType's answer for a given
// browser/OS/hardware combination doesn't change over the page's
// lifetime, and creating a throwaway <video> element for every segment
// swap would be wasteful.
let cachedCanPlayHEVC: boolean | null = null
export function canPlayNativeHEVC(): boolean {
  if (cachedCanPlayHEVC !== null) return cachedCanPlayHEVC
  const probe = document.createElement('video')
  const hvc1 = probe.canPlayType('video/mp4; codecs="hvc1.1.6.L153.B0"')
  const hev1 = probe.canPlayType('video/mp4; codecs="hev1.1.6.L153.B0"')
  cachedCanPlayHEVC = hvc1 !== '' || hev1 !== ''
  return cachedCanPlayHEVC
}

// hlsInstances tracks the hls.js instance (if any) currently attached to
// each <video> element, so attachSegmentSource/detachSegmentSource can
// clean up the previous one before attaching a new source — hls.js
// instances are relatively heavy (their own worker/buffer state) and are
// not something a plain `el.src = ...` reassignment cleans up on its own.
const hlsInstances = new WeakMap<HTMLVideoElement, Hls>()

// attachSegmentSource points video at segmentId's media, choosing the
// original source when the browser can play it natively and the HLS
// fallback otherwise (via hls.js, or a plain src for a browser — Safari —
// that plays HLS natively). Unlike the plain `el.src = url; el.load()`
// pattern this replaces, callers should NOT call el.load() themselves
// afterward: the hls.js path drives loading itself (calling load()
// afterward would tear down the MediaSource attachment hls.js just made).
// Callers that need a "metadata loaded" signal should listen for
// 'loadedmetadata' on video as before — that fires the same way regardless
// of which path was taken.
export function attachSegmentSource(video: HTMLVideoElement, segmentId: number | string): void {
  detachSegmentSource(video)

  if (canPlayNativeHEVC()) {
    video.src = segmentMediaUrl(segmentId)
    video.load()
    return
  }

  const hlsUrl = segmentHLSMasterUrl(segmentId)

  // Safari (and other WebKit-based browsers that can't play HEVC, if any)
  // support HLS natively through a plain <video src> — no hls.js needed,
  // and hls.js's MediaSource-based approach is unnecessary/unsupported
  // there anyway.
  if (video.canPlayType('application/vnd.apple.mpegurl') !== '') {
    video.src = hlsUrl
    video.load()
    return
  }

  if (Hls.isSupported()) {
    const hls = new Hls()
    hls.loadSource(hlsUrl)
    hls.attachMedia(video)
    hlsInstances.set(video, hls)
    return
  }

  // Neither native HEVC, native HLS, nor hls.js (MediaSource Extensions
  // unsupported) — fall back to the raw source anyway so at least a
  // normal <video> error event fires instead of silently doing nothing.
  video.src = segmentMediaUrl(segmentId)
  video.load()
}

// clearSegmentSource empties video's source (used when there's no next
// segment to preload — e.g. the inactive front double-buffer slot at the
// end of a trip) — detaches any hls.js instance first, then clears src the
// same way the pre-HLS code did.
export function clearSegmentSource(video: HTMLVideoElement): void {
  detachSegmentSource(video)
  video.removeAttribute('src')
  video.load()
}

// detachSegmentSource destroys and forgets video's hls.js instance, if
// any. Safe to call on a video that never had one (native-HEVC playback,
// or one that's already been detached).
export function detachSegmentSource(video: HTMLVideoElement): void {
  const existing = hlsInstances.get(video)
  if (existing) {
    existing.destroy()
    hlsInstances.delete(video)
  }
}
