// useTimelineViewport: the zoom/pan model shared by TimelineStrip and
// TimelineOverview. The viewport is a window [start, end] expressed as
// fractions (0..1) of the trip's total duration. `minSpanS` (one segment's
// duration, or 10s, whichever is larger) caps how far a caller can zoom in —
// past that point there's nothing meaningful left to show.
import { useCallback, useMemo, useState } from 'react'

export interface Viewport {
  start: number
  end: number
}

const FULL: Viewport = { start: 0, end: 1 }

function clampViewport(v: Viewport, minFrac: number): Viewport {
  let width = Math.min(1, Math.max(minFrac, v.end - v.start))
  let start = v.start
  if (start < 0) start = 0
  if (start + width > 1) start = 1 - width
  if (start < 0) start = 0
  return { start, end: start + width }
}

export interface TimelineViewportApi {
  viewport: Viewport
  minSpanFrac: number
  isZoomed: boolean
  setViewport: (v: Viewport) => void
  zoomAt: (centerFrac: number, scale: number) => void
  zoomIn: (centerFrac?: number) => void
  zoomOut: (centerFrac?: number) => void
  fit: () => void
  pan: (deltaFrac: number) => void
}

export function useTimelineViewport(totalS: number, minSpanS: number): TimelineViewportApi {
  const [viewport, setViewportRaw] = useState<Viewport>(FULL)
  const minSpanFrac = totalS > 0 ? Math.min(1, Math.max(minSpanS, 1) / totalS) : 0.01

  const setViewport = useCallback(
    (v: Viewport) => setViewportRaw(clampViewport(v, minSpanFrac)),
    [minSpanFrac],
  )

  const zoomAt = useCallback(
    (centerFrac: number, scale: number) => {
      setViewportRaw((prev) => {
        const width = prev.end - prev.start
        const newWidth = Math.min(1, Math.max(minSpanFrac, width * scale))
        const ratio = width > 0 ? (centerFrac - prev.start) / width : 0.5
        const start = centerFrac - ratio * newWidth
        return clampViewport({ start, end: start + newWidth }, minSpanFrac)
      })
    },
    [minSpanFrac],
  )

  const zoomIn = useCallback(
    (centerFrac?: number) => {
      zoomAt(centerFrac ?? (viewport.start + viewport.end) / 2, 0.5)
    },
    [zoomAt, viewport.start, viewport.end],
  )

  const zoomOut = useCallback(
    (centerFrac?: number) => {
      zoomAt(centerFrac ?? (viewport.start + viewport.end) / 2, 2)
    },
    [zoomAt, viewport.start, viewport.end],
  )

  const fit = useCallback(() => setViewportRaw(FULL), [])

  const pan = useCallback((deltaFrac: number) => {
    setViewportRaw((prev) => clampViewport({ start: prev.start + deltaFrac, end: prev.end + deltaFrac }, minSpanFrac))
  }, [minSpanFrac])

  const isZoomed = useMemo(() => viewport.end - viewport.start < 1 - 1e-6, [viewport])

  return { viewport, minSpanFrac, isZoomed, setViewport, zoomAt, zoomIn, zoomOut, fit, pan }
}

// pickTickStepS chooses a label step (seconds) from a fixed ladder so that
// the visible span shows a reasonable number of ticks (roughly 4-10).
const TICK_LADDER_S = [10, 30, 60, 300, 600, 1800, 3600, 7200, 14400]
export function pickTickStepS(visibleSpanS: number): number {
  for (const step of TICK_LADDER_S) {
    if (visibleSpanS / step <= 10) return step
  }
  return TICK_LADDER_S[TICK_LADDER_S.length - 1]
}
