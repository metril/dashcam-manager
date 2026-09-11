import { describe, expect, it } from 'vitest'
import { boxesAt, buildTrackIndex, nearestPreviousFrame, HOLD_S, MAX_GAP_S, UNTRACKED_HOLD_S } from './detectionInterp'
import type { DetFrame, DetObject } from '../../types'

function obj(overrides: Partial<DetObject> = {}): DetObject {
  return { class: 'car', confidence: 0.9, bbox: [0, 0, 0.1, 0.1], ...overrides }
}

function frame(overrides: Partial<DetFrame> = {}): DetFrame {
  return {
    timestamp: 0,
    wall_time: '',
    frame_index: 0,
    pts_offset: 0,
    objects: [],
    plates: [],
    vehicles: [],
    ...overrides,
  }
}

describe('buildTrackIndex / boxesAt', () => {
  it('linearly interpolates between bracketing samples of the same track', () => {
    const frames = [
      frame({ pts_offset: 0, objects: [obj({ track_id: 1, bbox: [0, 0, 0.2, 0.2] })] }),
      frame({ pts_offset: 0.4, objects: [obj({ track_id: 1, bbox: [1, 1, 1.2, 1.2] })] }),
    ]
    const idx = buildTrackIndex(frames)
    const { objects } = boxesAt(idx, 0.2)
    expect(objects).toHaveLength(1)
    expect(objects[0].bbox).toEqual([0.5, 0.5, 0.7, 0.7])
  })

  it('carries class/label/confidence from the earlier sample when interpolating', () => {
    const frames = [
      frame({ pts_offset: 0, objects: [obj({ track_id: 1, class: 'truck', confidence: 0.7 })] }),
      frame({ pts_offset: 0.2, objects: [obj({ track_id: 1, class: 'car', confidence: 0.99, bbox: [1, 1, 1.1, 1.1] })] }),
    ]
    const idx = buildTrackIndex(frames)
    const { objects } = boxesAt(idx, 0.1)
    expect(objects[0].class).toBe('truck')
    expect(objects[0].confidence).toBe(0.7)
  })

  it('does not interpolate across a gap larger than MAX_GAP_S, and holds the prior box within HOLD_S past it', () => {
    const frames = [
      frame({ pts_offset: 0, objects: [obj({ track_id: 1, bbox: [0, 0, 0.1, 0.1] })] }),
      frame({ pts_offset: MAX_GAP_S + 1, objects: [obj({ track_id: 1, bbox: [9, 9, 9.1, 9.1] })] }),
    ]
    const idx = buildTrackIndex(frames)

    // Just past the first sample, within HOLD_S: hold its box verbatim.
    const held = boxesAt(idx, HOLD_S - 0.01)
    expect(held.objects).toHaveLength(1)
    expect(held.objects[0].bbox).toEqual([0, 0, 0.1, 0.1])

    // Past HOLD_S but still far from the next sample: nothing renders —
    // no interpolation across the large gap, no stale hold either.
    const gapped = boxesAt(idx, HOLD_S + 0.1)
    expect(gapped.objects).toHaveLength(0)
  })

  it('never renders a lone future-only sample', () => {
    const frames = [frame({ pts_offset: 5, objects: [obj({ track_id: 1 })] })]
    const idx = buildTrackIndex(frames)
    const { objects } = boxesAt(idx, 0)
    expect(objects).toHaveLength(0)
  })

  it('falls back to nearest-previous-frame hold for untracked entries', () => {
    const frames = [
      frame({ pts_offset: 0, objects: [obj({ track_id: null, bbox: [0, 0, 0.1, 0.1] })] }),
      frame({ pts_offset: 1, objects: [obj({ track_id: null, bbox: [0.5, 0.5, 0.6, 0.6] })] }),
    ]
    const idx = buildTrackIndex(frames)

    // Within HOLD_S of the pts_offset=0 frame: that frame's boxes show.
    const near = boxesAt(idx, HOLD_S - 0.01)
    expect(near.objects).toHaveLength(1)
    expect(near.objects[0].bbox).toEqual([0, 0, 0.1, 0.1])

    // Past HOLD_S from the previous frame and before the next: nothing.
    const between = boxesAt(idx, 0.5)
    expect(between.objects).toHaveLength(0)

    // At the second frame's own timestamp: its boxes show.
    const atSecond = boxesAt(idx, 1)
    expect(atSecond.objects).toHaveLength(1)
    expect(atSecond.objects[0].bbox).toEqual([0.5, 0.5, 0.6, 0.6])
  })

  it('holds untracked entries for UNTRACKED_HOLD_S, longer than the tracked HOLD_S', () => {
    expect(UNTRACKED_HOLD_S).toBeGreaterThan(HOLD_S)
    const frames = [frame({ pts_offset: 0, objects: [obj({ track_id: null, bbox: [0, 0, 0.1, 0.1] })] })]
    const idx = buildTrackIndex(frames)

    // Past the tracked HOLD_S but still within UNTRACKED_HOLD_S: still shown.
    const past = boxesAt(idx, HOLD_S + 0.05)
    expect(past.objects).toHaveLength(1)
    expect(past.objects[0].bbox).toEqual([0, 0, 0.1, 0.1])

    // Past UNTRACKED_HOLD_S: no longer shown.
    const gone = boxesAt(idx, UNTRACKED_HOLD_S + 0.01)
    expect(gone.objects).toHaveLength(0)
  })

  it('keeps distinct tracks independent', () => {
    const frames = [
      frame({
        pts_offset: 0,
        objects: [obj({ track_id: 1, bbox: [0, 0, 0.1, 0.1] }), obj({ track_id: 2, bbox: [0.5, 0.5, 0.6, 0.6] })],
      }),
      frame({
        pts_offset: 1,
        objects: [obj({ track_id: 1, bbox: [0.2, 0.2, 0.3, 0.3] }), obj({ track_id: 2, bbox: [0.7, 0.7, 0.8, 0.8] })],
      }),
    ]
    const idx = buildTrackIndex(frames)
    const { objects } = boxesAt(idx, 0)
    expect(objects).toHaveLength(2)
  })
})

describe('nearestPreviousFrame', () => {
  it('returns the nearest at-or-before frame within tolerance', () => {
    const frames = [frame({ pts_offset: 0 }), frame({ pts_offset: 1 })]
    expect(nearestPreviousFrame(frames, 1.05, 0.1)).toBe(frames[1])
  })

  it('never returns a future frame even if closer', () => {
    const frames = [frame({ pts_offset: 0 }), frame({ pts_offset: 1 })]
    // 0.97 is closer to the pts=1 frame by absolute distance, but that
    // frame is still in the future — must fall back to pts=0 or, if out
    // of tolerance, null.
    expect(nearestPreviousFrame(frames, 0.97, 0.1)).toBeNull()
  })

  it('returns null when nothing is within tolerance', () => {
    const frames = [frame({ pts_offset: 0 })]
    expect(nearestPreviousFrame(frames, 5, 0.1)).toBeNull()
  })
})
