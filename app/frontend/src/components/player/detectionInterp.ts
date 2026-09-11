// detectionInterp: pure helpers that turn the sparse, per-frame detection
// samples returned by GET /api/segments/{id}/detections into a smooth
// per-instant set of boxes to draw.
//
// Detections arrive at 30-150ms spacing (not every video frame), so drawing
// straight off "the nearest sample" makes boxes visibly lag/snap. Instead we
// group each object/plate/vehicle by its det_tracks identity (track_id) into
// per-track timelines, then at any playback time t find the bracketing
// samples and linearly interpolate the box between them. Entries with no
// track_id (untracked — pre-tracker data, or the tracker didn't link them)
// can't be interpolated meaningfully, so they just hold the nearest-previous
// frame's box briefly.
import type { DetFrame, DetObject, DetPlate, DetVehicle } from '../../types'
import type { DetTrack } from '../../lib/api'

// MAX_GAP_S: the largest gap between two bracketing samples of the same
// track that we'll still interpolate across. Beyond this the track likely
// dropped out (occlusion, re-detection miss) and a straight-line lerp
// between two far-apart boxes would read as a warp, not motion.
export const MAX_GAP_S = 0.5
// HOLD_S: how long to keep showing the last-known box past its own
// timestamp (when there's no future sample yet, or the gap to it was too
// large to interpolate) before letting it disappear.
export const HOLD_S = 0.1
// UNTRACKED_HOLD_S: same idea as HOLD_S but for untracked entries (no
// track_id to interpolate across), which come from nearestPreviousFrameItems
// rather than resolveTrack. Untracked samples are more prone to
// frame-to-frame flicker (nothing links them across detections), so they
// get a longer hold to avoid boxes blinking in and out.
export const UNTRACKED_HOLD_S = 0.2

interface TrackSample<T> {
  pts: number
  item: T
}

export interface TrackIndex {
  objects: Map<number, TrackSample<DetObject>[]>
  plates: Map<number, TrackSample<DetPlate>[]>
  vehicles: Map<number, TrackSample<DetVehicle>[]>
  // Untracked (track_id null/undefined) entries, kept as flat per-kind
  // lists (not grouped by track) since there's no identity to interpolate
  // across.
  untrackedObjects: TrackSample<DetObject>[]
  untrackedPlates: TrackSample<DetPlate>[]
  untrackedVehicles: TrackSample<DetVehicle>[]
  // trackMake: track_id -> canonical make, built from the `tracks` array's
  // vehicle rows that have a non-null make (the tracker's per-track vote —
  // see cmd/api/queries_tracks.go). boxesAt uses this to override a
  // resolved vehicle box's own sample make, so one track shows one stable
  // make instead of flickering between per-frame guesses.
  trackMake: Map<number, string>
}

export interface BoxesAtResult {
  objects: DetObject[]
  plates: DetPlate[]
  vehicles: DetVehicle[]
}

function pushTrack<T>(map: Map<number, TrackSample<T>[]>, trackId: number, pts: number, item: T): void {
  let list = map.get(trackId)
  if (!list) {
    list = []
    map.set(trackId, list)
  }
  list.push({ pts, item })
}

// buildTrackIndex groups every object/plate/vehicle across frames by
// (kind, track_id) into lists sorted by pts_offset (ascending), so boxesAt
// can binary/linear-scan each track's timeline independently.
export function buildTrackIndex(frames: DetFrame[], tracks?: DetTrack[]): TrackIndex {
  const objects = new Map<number, TrackSample<DetObject>[]>()
  const plates = new Map<number, TrackSample<DetPlate>[]>()
  const vehicles = new Map<number, TrackSample<DetVehicle>[]>()
  const untrackedObjects: TrackSample<DetObject>[] = []
  const untrackedPlates: TrackSample<DetPlate>[] = []
  const untrackedVehicles: TrackSample<DetVehicle>[] = []

  for (const frame of frames) {
    const pts = frame.pts_offset
    for (const o of frame.objects) {
      if (o.track_id != null) pushTrack(objects, o.track_id, pts, o)
      else untrackedObjects.push({ pts, item: o })
    }
    for (const p of frame.plates) {
      if (p.track_id != null) pushTrack(plates, p.track_id, pts, p)
      else untrackedPlates.push({ pts, item: p })
    }
    for (const v of frame.vehicles) {
      if (v.track_id != null) pushTrack(vehicles, v.track_id, pts, v)
      else untrackedVehicles.push({ pts, item: v })
    }
  }

  const byPts = <T,>(a: TrackSample<T>, b: TrackSample<T>) => a.pts - b.pts
  for (const list of objects.values()) list.sort(byPts)
  for (const list of plates.values()) list.sort(byPts)
  for (const list of vehicles.values()) list.sort(byPts)
  untrackedObjects.sort(byPts)
  untrackedPlates.sort(byPts)
  untrackedVehicles.sort(byPts)

  const trackMake = new Map<number, string>()
  if (tracks) {
    for (const t of tracks) {
      if (t.kind === 'vehicle' && t.make != null) trackMake.set(t.id, t.make)
    }
  }

  return { objects, plates, vehicles, untrackedObjects, untrackedPlates, untrackedVehicles, trackMake }
}

function lerpBbox(
  a: [number, number, number, number],
  b: [number, number, number, number],
  f: number,
): [number, number, number, number] {
  return [a[0] + (b[0] - a[0]) * f, a[1] + (b[1] - a[1]) * f, a[2] + (b[2] - a[2]) * f, a[3] + (b[3] - a[3]) * f]
}

// findBracket returns the last sample at-or-before t (prev) and the first
// sample strictly after t (next) from a pts-ascending-sorted list.
function findBracket<T>(samples: TrackSample<T>[], t: number): [TrackSample<T> | null, TrackSample<T> | null] {
  let prev: TrackSample<T> | null = null
  let next: TrackSample<T> | null = null
  for (const s of samples) {
    if (s.pts <= t) prev = s
    else {
      next = s
      break
    }
  }
  return [prev, next]
}

// resolveTrack finds t's bracketing samples in one track's timeline and
// returns the box to draw, or null if nothing qualifies:
//  - a <= t <= b with (b.pts - a.pts) <= MAX_GAP_S: linear-interpolate the
//    bbox between a and b, carrying a's class/label/confidence.
//  - otherwise, if a prev sample exists within HOLD_S of t: hold its box.
//  - a lone future sample (only `next`, no `prev`) is never rendered.
function resolveTrack<T extends { bbox: [number, number, number, number] }>(
  samples: TrackSample<T>[],
  t: number,
): T | null {
  const [prev, next] = findBracket(samples, t)
  if (prev && next && next.pts - prev.pts <= MAX_GAP_S) {
    const f = next.pts === prev.pts ? 0 : (t - prev.pts) / (next.pts - prev.pts)
    return { ...prev.item, bbox: lerpBbox(prev.item.bbox, next.item.bbox, f) }
  }
  if (prev && t - prev.pts <= HOLD_S) return prev.item
  return null
}

// nearestPreviousFrameItems returns every untracked item sharing the
// latest pts at-or-before t, provided that pts is within toleranceS of t —
// i.e. "hold the last untracked frame's boxes briefly," never a future one.
function nearestPreviousFrameItems<T>(samples: TrackSample<T>[], t: number, toleranceS: number): T[] {
  let bestPts = -Infinity
  for (const s of samples) {
    if (s.pts <= t && s.pts > bestPts) bestPts = s.pts
  }
  if (bestPts === -Infinity || t - bestPts > toleranceS) return []
  return samples.filter((s) => s.pts === bestPts).map((s) => s.item)
}

// boxesAt resolves, for every track plus the untracked pool, the box (if
// any) to draw at time t. Returns the same per-kind arrays the draw routine
// already consumes (DetObject[]/DetPlate[]/DetVehicle[]), just re-sourced
// from interpolation instead of "the nearest whole frame."
export function boxesAt(index: TrackIndex, t: number): BoxesAtResult {
  const objects: DetObject[] = []
  for (const samples of index.objects.values()) {
    const r = resolveTrack(samples, t)
    if (r) objects.push(r)
  }
  // Per-kind gate: the untracked pool (pre-tracker data, or items the
  // tracker didn't link) is only shown when this kind has no tracks at
  // all — once a kind has tracks, its untracked leftovers would otherwise
  // double up with (or flicker alongside) the tracked boxes.
  if (index.objects.size === 0) {
    objects.push(...nearestPreviousFrameItems(index.untrackedObjects, t, UNTRACKED_HOLD_S))
  }

  const plates: DetPlate[] = []
  for (const samples of index.plates.values()) {
    const r = resolveTrack(samples, t)
    if (r) plates.push(r)
  }
  if (index.plates.size === 0) {
    plates.push(...nearestPreviousFrameItems(index.untrackedPlates, t, UNTRACKED_HOLD_S))
  }

  const vehicles: DetVehicle[] = []
  for (const [trackId, samples] of index.vehicles) {
    const r = resolveTrack(samples, t)
    if (!r) continue
    const canonicalMake = index.trackMake.get(trackId)
    vehicles.push(canonicalMake != null ? { ...r, make: canonicalMake } : r)
  }
  if (index.vehicles.size === 0) {
    vehicles.push(...nearestPreviousFrameItems(index.untrackedVehicles, t, UNTRACKED_HOLD_S))
  }

  return { objects, plates, vehicles }
}

// nearestPreviousFrame: the whole-frame analogue used by the HUD (gps
// speed/class counts) — deliberately nearest-*previous*, not nearest by
// absolute distance, so the HUD never reflects a not-yet-reached future
// sample either.
export function nearestPreviousFrame(frames: DetFrame[], atS: number, toleranceS: number): DetFrame | null {
  let best: DetFrame | null = null
  for (const f of frames) {
    if (f.pts_offset <= atS && (best == null || f.pts_offset > best.pts_offset)) best = f
  }
  if (best == null || atS - best.pts_offset > toleranceS) return null
  return best
}
