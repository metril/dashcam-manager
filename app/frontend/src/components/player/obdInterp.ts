// obdInterp: pure helpers that turn the sparse (~2Hz) OBD telemetry
// samples returned by GET /api/segments/{id}/obd into a smooth per-instant
// reading for TelemetryHUD/TelemetryStrip to draw — same shape of problem
// as detectionInterp.ts's per-track interpolation, just over a single flat
// timeline (one segment's points) instead of per-track ones.
import type { OBDPoint } from '../../types'

// MAX_GAP_S: the largest gap between two bracketing samples (or between t
// and a lone edge sample) we'll still interpolate/hold across. Points
// arrive at ~2Hz (≈0.5s spacing), so a few seconds comfortably covers a
// dropped sample or two without smearing a real telemetry gap (recorder
// stalled, OBD disconnected) into a straight-line lerp.
export const MAX_GAP_S = 3

export interface OBDIndex {
  points: OBDPoint[] // sorted by offset_s ascending
}

// buildOBDIndex sorts a segment's OBD points by offset_s once, so sampleAt
// can linear-scan them repeatedly (called every playback tick) without
// re-sorting.
export function buildOBDIndex(points: OBDPoint[]): OBDIndex {
  return { points: [...points].sort((a, b) => a.offset_s - b.offset_s) }
}

export interface OBDSample {
  speed_kph: number | null
  rpm: number | null
  throttle_pct: number | null
  accel_mps2: number | null
  coolant_c: number | null
  batt_v: number | null
}

function lerp(a: number, b: number, f: number): number {
  return a + (b - a) * f
}

// interpField linearly interpolates one numeric field between two samples,
// falling back to whichever side has a value when the other is null (a
// single PID can be missing from one sample without dropping the whole
// reading) and null when neither does.
function interpField(a: number | null, b: number | null, f: number): number | null {
  if (a == null) return b
  if (b == null) return a
  return lerp(a, b, f)
}

function sampleFrom(p: OBDPoint): OBDSample {
  return {
    speed_kph: p.speed_kph,
    rpm: p.rpm,
    throttle_pct: p.throttle_pct,
    accel_mps2: p.accel_mps2,
    coolant_c: p.coolant_c,
    batt_v: p.batt_v,
  }
}

// findBracket returns the last point at-or-before t (prev) and the first
// point strictly after t (next) from an offset_s-ascending-sorted list.
function findBracket(points: OBDPoint[], t: number): [OBDPoint | null, OBDPoint | null] {
  let prev: OBDPoint | null = null
  let next: OBDPoint | null = null
  for (const p of points) {
    if (p.offset_s <= t) prev = p
    else {
      next = p
      break
    }
  }
  return [prev, next]
}

// sampleAt resolves the telemetry reading at time t (seconds into the
// segment):
//  - t between two bracketing points no more than MAX_GAP_S apart: linear
//    interpolation, per field.
//  - t past the last point (or before the first), within MAX_GAP_S of it:
//    hold that edge point's reading.
//  - otherwise (empty index, gap too large, or too far past an edge):
//    null — nothing to show.
export function sampleAt(index: OBDIndex, t: number): OBDSample | null {
  const { points } = index
  if (points.length === 0) return null

  const [prev, next] = findBracket(points, t)

  if (prev && next) {
    if (next.offset_s - prev.offset_s > MAX_GAP_S) return null
    const f = next.offset_s === prev.offset_s ? 0 : (t - prev.offset_s) / (next.offset_s - prev.offset_s)
    return {
      speed_kph: interpField(prev.speed_kph, next.speed_kph, f),
      rpm: interpField(prev.rpm, next.rpm, f),
      throttle_pct: interpField(prev.throttle_pct, next.throttle_pct, f),
      accel_mps2: interpField(prev.accel_mps2, next.accel_mps2, f),
      coolant_c: interpField(prev.coolant_c, next.coolant_c, f),
      batt_v: interpField(prev.batt_v, next.batt_v, f),
    }
  }

  if (prev && !next) {
    if (t - prev.offset_s > MAX_GAP_S) return null
    return sampleFrom(prev)
  }

  if (!prev && next) {
    if (next.offset_s - t > MAX_GAP_S) return null
    return sampleFrom(next)
  }

  return null
}
