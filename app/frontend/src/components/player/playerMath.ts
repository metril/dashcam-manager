// Pure helpers for locating a point in trip-relative time within the
// front/rear segment playlists, shared by DualVideoPlayer. Kept
// dependency-free (no React) so they're trivial to reason about/test.
import type { TripSegment } from '../../types'

export interface PlaylistEntry {
  segment: TripSegment
  absStartMs: number
  absEndMs: number
}

// buildPlaylist sorts camera's segments by their actual start_time (not
// `seq`, which is assigned per-recording-directory and can disagree with
// wall-clock order across a trip spanning more than one directory) and
// precomputes each one's absolute [start,end) in epoch ms.
export function buildPlaylist(segments: TripSegment[], camera: 'front' | 'rear'): PlaylistEntry[] {
  return segments
    .filter((s) => s.camera === camera)
    .map((s) => {
      const absStartMs = Date.parse(s.start_time)
      const durMs = (s.duration_s ?? 30) * 1000
      const absEndMs = s.end_time ? Date.parse(s.end_time) : absStartMs + durMs
      return { segment: s, absStartMs, absEndMs }
    })
    .sort((a, b) => a.absStartMs - b.absStartMs)
}

// entryContaining returns the playlist entry whose [absStartMs,absEndMs)
// interval contains absMs, or null if absMs falls in a gap / outside the
// playlist's range entirely — the caller (rear slaving) treats "no match"
// as "nothing to show right now" rather than guessing.
export function entryContaining(playlist: PlaylistEntry[], absMs: number): PlaylistEntry | null {
  for (const e of playlist) {
    if (absMs >= e.absStartMs && absMs < e.absEndMs) return e
  }
  return null
}

// indexAtOrAfter returns the index of the first entry whose absStartMs is
// >= absMs (playlist.length if none), for seeking to "the segment covering
// this instant, or the next one if it falls in a gap".
export function indexAtOrAfter(playlist: PlaylistEntry[], absMs: number): number {
  for (let i = 0; i < playlist.length; i++) {
    if (playlist[i].absEndMs > absMs) return i
  }
  return playlist.length
}
