import { describe, expect, it } from 'vitest'
import { buildOBDIndex, sampleAt, MAX_GAP_S } from './obdInterp'
import type { OBDPoint } from '../../types'

function point(overrides: Partial<OBDPoint> = {}): OBDPoint {
  return {
    offset_s: 0,
    speed_kph: 50,
    rpm: 2000,
    throttle_pct: 20,
    accel_mps2: 0,
    coolant_c: 90,
    batt_v: 13.8,
    ...overrides,
  }
}

describe('buildOBDIndex / sampleAt', () => {
  it('linearly interpolates every field between bracketing points', () => {
    const points = [
      point({ offset_s: 0, speed_kph: 40, rpm: 1500, throttle_pct: 10, accel_mps2: 0, coolant_c: 80, batt_v: 13.0 }),
      point({ offset_s: 1, speed_kph: 60, rpm: 2500, throttle_pct: 30, accel_mps2: 2, coolant_c: 90, batt_v: 14.0 }),
    ]
    const idx = buildOBDIndex(points)
    const s = sampleAt(idx, 0.5)
    expect(s).toEqual({
      speed_kph: 50,
      rpm: 2000,
      throttle_pct: 20,
      accel_mps2: 1,
      coolant_c: 85,
      batt_v: 13.5,
    })
  })

  it('interpolates at an arbitrary fraction, not just the midpoint', () => {
    const points = [point({ offset_s: 0, speed_kph: 0 }), point({ offset_s: 2, speed_kph: 100 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 0.5)?.speed_kph).toBe(25)
  })

  it('sorts unsorted input before interpolating', () => {
    const points = [point({ offset_s: 2, speed_kph: 100 }), point({ offset_s: 0, speed_kph: 0 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 1)?.speed_kph).toBe(50)
  })

  it('does not interpolate across a gap larger than MAX_GAP_S', () => {
    // Gap wide enough that its midpoint is itself more than MAX_GAP_S from
    // both edges — the genuinely unknown middle, not just past one edge's
    // hold window.
    const gapEnd = 2 * MAX_GAP_S + 2
    const points = [point({ offset_s: 0, speed_kph: 0 }), point({ offset_s: gapEnd, speed_kph: 100 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, gapEnd / 2)).toBeNull()
  })

  it('still returns the known edge values at/near a too-large gap, only blanking the genuinely unknown middle', () => {
    const points = [point({ offset_s: 0, speed_kph: 40 }), point({ offset_s: 10, speed_kph: 90 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 0)?.speed_kph).toBe(40)
    expect(sampleAt(idx, 10)?.speed_kph).toBe(90)
    expect(sampleAt(idx, 5)).toBeNull()
  })

  it('holds the last point within MAX_GAP_S past it, and returns null beyond that', () => {
    const points = [point({ offset_s: 0, speed_kph: 42 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, MAX_GAP_S - 0.1)?.speed_kph).toBe(42)
    expect(sampleAt(idx, MAX_GAP_S + 0.1)).toBeNull()
  })

  it('holds the first point within MAX_GAP_S before it, and returns null beyond that', () => {
    const points = [point({ offset_s: 10, speed_kph: 42 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 10 - (MAX_GAP_S - 0.1))?.speed_kph).toBe(42)
    expect(sampleAt(idx, 10 - (MAX_GAP_S + 0.1))).toBeNull()
  })

  it('returns null for an empty index', () => {
    const idx = buildOBDIndex([])
    expect(sampleAt(idx, 0)).toBeNull()
  })

  it('falls back to whichever side has a value when one field is null', () => {
    const points = [
      point({ offset_s: 0, rpm: null }),
      point({ offset_s: 1, rpm: 3000 }),
    ]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 0.5)?.rpm).toBe(3000)
  })

  it('returns null for a field that is null on both bracketing points', () => {
    const points = [point({ offset_s: 0, coolant_c: null }), point({ offset_s: 1, coolant_c: null })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 0.5)?.coolant_c).toBeNull()
  })

  it('resolves exactly at a sample point without needing a neighbor', () => {
    const points = [point({ offset_s: 3, speed_kph: 77 })]
    const idx = buildOBDIndex(points)
    expect(sampleAt(idx, 3)?.speed_kph).toBe(77)
  })
})
