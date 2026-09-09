// TimelineStrip renders the trip as a zoomable, pannable horizontal strip:
// segment boundaries (front + rear rows) with a detection-density heat
// tint, a playhead, and a thin overview bar showing the whole trip. The
// visible window is a `Viewport` (fractions of trip duration) from
// useTimelineViewport; segments/playhead are positioned relative to it.
//
// Interactions:
//  - wheel: zoom around the cursor. shift+wheel (or a horizontal trackpad
//    swipe): pan.
//  - mouse: plain left-drag on the strip selects an in/out range; middle-
//    drag pans; click seeks; double-click resets zoom to fit.
//  - touch: one-finger drag pans; two-finger pinch zooms; long-press then
//    drag selects a range.
//  - zoom in / zoom out / fit buttons, and +/-/0 keys, control zoom.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Minus, Plus, Scan } from 'lucide-react'
import type { TimelineSegment, TripTimeline } from '../../types'
import { formatDuration } from '../../lib/format'
import { pickTickStepS, useTimelineViewport, type Viewport } from './useTimelineViewport'
import TimelineOverview from './TimelineOverview'

const LONG_PRESS_MS = 450
const DRAG_THRESHOLD_PX = 4
const ZOOM_STEP = 0.5 // wheel zoom-in factor per notch (zoom-out is 1/this)

function heatColor(count: number, max: number): string {
  if (count <= 0 || max <= 0) return 'var(--color-surface-2)'
  const t = Math.min(1, count / max)
  // Warm ramp: dim at low density, saturated at max. Fixed regardless of
  // theme — it's a density heatmap color, not brand/accent chrome.
  const alpha = 0.25 + t * 0.65
  return `rgba(245,158,11,${alpha.toFixed(2)})`
}

function segmentAtFrac(segments: TimelineSegment[], totalS: number, frac: number): TimelineSegment | null {
  const atS = frac * totalS
  for (const seg of segments) {
    if (atS >= seg.offset_s && atS < seg.offset_s + seg.duration_s) return seg
  }
  return null
}

// toViewportPct maps an absolute trip-fraction to a percentage within the
// current viewport window, for positioning children of the (clipped) track.
function toViewportPct(absFrac: number, viewport: Viewport): number {
  const width = viewport.end - viewport.start
  return width > 0 ? ((absFrac - viewport.start) / width) * 100 : 0
}

function Row({
  segments,
  totalS,
  maxCount,
  viewport,
}: {
  segments: TimelineSegment[]
  totalS: number
  maxCount: number
  viewport: Viewport
}) {
  if (segments.length === 0 || totalS <= 0) return null
  return (
    <div className="relative h-3.5 w-full overflow-hidden rounded-sm bg-surface">
      {segments.map((seg) => {
        const segStartFrac = seg.offset_s / totalS
        const segEndFrac = (seg.offset_s + seg.duration_s) / totalS
        if (segEndFrac < viewport.start || segStartFrac > viewport.end) return null
        const leftPct = toViewportPct(segStartFrac, viewport)
        const widthPct = Math.max(0.3, toViewportPct(segEndFrac, viewport) - leftPct)
        return (
          <div
            key={seg.id}
            className="absolute top-0 h-full border-r border-black/40"
            style={{ left: `${leftPct}%`, width: `${widthPct}%`, background: heatColor(seg.count, maxCount) }}
            title={`${seg.camera} seg ${seg.seq} · ${seg.count} objects`}
          />
        )
      })}
    </div>
  )
}

type DragMode = 'none' | 'pending-mouse' | 'pending-touch' | 'pan' | 'select'

interface DragState {
  mode: DragMode
  pointerId: number
  startClientX: number
  startFrac: number
  viewportAtStart: Viewport
}

interface PinchState {
  pointerIds: [number, number]
  startDistPx: number
  centerFrac: number
  viewportAtStart: Viewport
}

export default function TimelineStrip({
  timeline,
  currentOffsetS,
  onSeek,
  onRangeSelect,
}: {
  timeline: TripTimeline | undefined
  currentOffsetS: number
  onSeek: (offsetS: number) => void
  /** Called when the user drags out an in/out range on the strip (mouse drag, or touch long-press + drag). */
  onRangeSelect?: (inS: number, outS: number) => void
}) {
  const trackRef = useRef<HTMLDivElement | null>(null)
  const [hover, setHover] = useState<{ absFrac: number; seg: TimelineSegment | null } | null>(null)
  const [selection, setSelection] = useState<{ startFrac: number; endFrac: number } | null>(null)

  const totalS = timeline?.duration_s ?? 0
  const front = timeline?.segments.filter((s) => s.camera === 'front') ?? []
  const rear = timeline?.segments.filter((s) => s.camera === 'rear') ?? []
  const allSegments = useMemo(() => timeline?.segments ?? [], [timeline])
  const maxCount = Math.max(1, ...(allSegments.map((s) => s.count) ?? [1]))

  const minSpanS = useMemo(() => {
    const durations = allSegments.map((s) => s.duration_s).filter((d) => d > 0)
    if (durations.length === 0) return 10
    const sorted = [...durations].sort((a, b) => a - b)
    const median = sorted[Math.floor(sorted.length / 2)]
    return Math.max(10, median)
  }, [allSegments])

  const { viewport, isZoomed, setViewport, zoomAt, zoomIn, zoomOut, fit, pan } = useTimelineViewport(totalS, minSpanS)

  const dragRef = useRef<DragState | null>(null)
  const pinchRef = useRef<PinchState | null>(null)
  const longPressTimerRef = useRef<number | null>(null)

  const clearLongPress = useCallback(() => {
    if (longPressTimerRef.current != null) {
      window.clearTimeout(longPressTimerRef.current)
      longPressTimerRef.current = null
    }
  }, [])

  const fracFromClientX = useCallback(
    (clientX: number) => {
      const el = trackRef.current
      if (!el) return viewport.start
      const rect = el.getBoundingClientRect()
      const pct = rect.width > 0 ? Math.min(1, Math.max(0, (clientX - rect.left) / rect.width)) : 0
      return viewport.start + pct * (viewport.end - viewport.start)
    },
    [viewport],
  )

  // --- wheel: zoom around cursor; shift+wheel (or horizontal scroll) pans ---
  useEffect(() => {
    const el = trackRef.current
    if (!el) return
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const centerFrac = fracFromClientX(e.clientX)
      if (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY)) {
        const deltaPx = e.shiftKey ? e.deltaY : e.deltaX
        const rect = el.getBoundingClientRect()
        const width = viewport.end - viewport.start
        pan((deltaPx / Math.max(1, rect.width)) * width)
        return
      }
      const scale = e.deltaY > 0 ? 1 / ZOOM_STEP : ZOOM_STEP
      zoomAt(centerFrac, scale)
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [fracFromClientX, zoomAt, pan, viewport.start, viewport.end])

  // --- +/-/0 keys zoom/fit, ignoring typing contexts (same guard as the player's shortcuts) ---
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.ctrlKey || e.metaKey || e.altKey) return
      if (e.target instanceof HTMLElement) {
        if (['INPUT', 'TEXTAREA', 'SELECT'].includes(e.target.tagName)) return
        if (e.target.isContentEditable) return
      }
      if (e.key === '+' || e.key === '=') {
        zoomIn()
      } else if (e.key === '-' || e.key === '_') {
        zoomOut()
      } else if (e.key === '0') {
        fit()
      } else {
        return
      }
      e.preventDefault()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [zoomIn, zoomOut, fit])

  const finishSelection = useCallback(
    (startFrac: number, endFrac: number) => {
      if (totalS <= 0 || !onRangeSelect) return
      const [a, b] = startFrac <= endFrac ? [startFrac, endFrac] : [endFrac, startFrac]
      if (b - a < 0.001) return // treat as a click, not a real selection
      onRangeSelect(a * totalS, b * totalS)
    },
    [totalS, onRangeSelect],
  )

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      ;(e.target as Element).setPointerCapture(e.pointerId)
      const startFrac = fracFromClientX(e.clientX)

      if (pinchRef.current) return // already pinching with two other pointers

      if (dragRef.current && dragRef.current.pointerId !== e.pointerId) {
        // A second pointer went down while one was already active -> pinch.
        clearLongPress()
        const first = dragRef.current
        pinchRef.current = {
          pointerIds: [first.pointerId, e.pointerId],
          startDistPx: Math.abs(e.clientX - first.startClientX),
          centerFrac: (first.startFrac + startFrac) / 2,
          viewportAtStart: viewport,
        }
        dragRef.current = null
        return
      }

      dragRef.current = {
        mode: e.pointerType === 'touch' ? 'pending-touch' : 'pending-mouse',
        pointerId: e.pointerId,
        startClientX: e.clientX,
        startFrac,
        viewportAtStart: viewport,
      }

      if (e.pointerType === 'mouse' && e.button === 1) {
        dragRef.current.mode = 'pan'
      } else if (e.pointerType === 'touch') {
        clearLongPress()
        longPressTimerRef.current = window.setTimeout(() => {
          if (dragRef.current?.pointerId === e.pointerId && dragRef.current.mode === 'pending-touch') {
            dragRef.current.mode = 'select'
            setSelection({ startFrac, endFrac: startFrac })
          }
        }, LONG_PRESS_MS)
      }
    },
    [fracFromClientX, viewport, clearLongPress],
  )

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      // Pinch (tracked by the native listener below) owns the viewport
      // while two pointers are down; ignore drag/hover logic here.
      if (pinchRef.current?.pointerIds.includes(e.pointerId)) return

      const drag = dragRef.current
      if (!drag || drag.pointerId !== e.pointerId) {
        // Hover readout (mouse only, no active drag).
        if (e.pointerType === 'mouse' && !drag) {
          const absFrac = fracFromClientX(e.clientX)
          setHover({ absFrac, seg: segmentAtFrac(allSegments, totalS, absFrac) })
        }
        return
      }

      const dxPx = e.clientX - drag.startClientX
      if (drag.mode === 'pending-mouse' || drag.mode === 'pending-touch') {
        if (Math.abs(dxPx) > DRAG_THRESHOLD_PX) {
          clearLongPress()
          drag.mode = drag.mode === 'pending-mouse' ? 'select' : 'pan'
          if (drag.mode === 'select') setSelection({ startFrac: drag.startFrac, endFrac: drag.startFrac })
        } else {
          return
        }
      }

      if (drag.mode === 'pan') {
        const el = trackRef.current
        const rectWidth = el ? el.getBoundingClientRect().width : 1
        const width = drag.viewportAtStart.end - drag.viewportAtStart.start
        const deltaFrac = -(dxPx / Math.max(1, rectWidth)) * width
        setViewport({
          start: drag.viewportAtStart.start + deltaFrac,
          end: drag.viewportAtStart.end + deltaFrac,
        })
        return
      }

      if (drag.mode === 'select') {
        const curFrac = fracFromClientX(e.clientX)
        setSelection({ startFrac: drag.startFrac, endFrac: curFrac })
      }
    },
    [fracFromClientX, allSegments, totalS, clearLongPress, setViewport],
  )

  const endDrag = useCallback(
    (e: React.PointerEvent) => {
      clearLongPress()
      if (pinchRef.current?.pointerIds.includes(e.pointerId)) {
        pinchRef.current = null
        dragRef.current = null
        return
      }
      const drag = dragRef.current
      if (!drag || drag.pointerId !== e.pointerId) return
      dragRef.current = null

      if (drag.mode === 'pending-mouse' || drag.mode === 'pending-touch') {
        // No meaningful movement -> a click/tap: seek.
        onSeek(drag.startFrac * totalS)
        return
      }
      if (drag.mode === 'select') {
        setSelection((sel) => {
          if (sel) finishSelection(sel.startFrac, sel.endFrac)
          return null
        })
      }
    },
    [clearLongPress, onSeek, totalS, finishSelection],
  )

  // Pinch-zoom needs both active touch pointers' live X coordinates at
  // once, which React's per-pointer event callbacks don't hand us — so
  // track raw pointer positions natively and drive the zoom from there.
  useEffect(() => {
    const el = trackRef.current
    if (!el) return
    const positions = new Map<number, number>() // pointerId -> clientX

    function onNativeDown(e: PointerEvent) {
      if (e.pointerType === 'touch') positions.set(e.pointerId, e.clientX)
    }
    function onNativeMove(e: PointerEvent) {
      if (e.pointerType !== 'touch' || !positions.has(e.pointerId)) return
      positions.set(e.pointerId, e.clientX)
      const pinch = pinchRef.current
      if (!pinch || !pinch.pointerIds.includes(e.pointerId)) return
      const [idA, idB] = pinch.pointerIds
      const xA = positions.get(idA)
      const xB = positions.get(idB)
      if (xA == null || xB == null) return
      const dist = Math.abs(xA - xB)
      if (dist <= 0 || pinch.startDistPx <= 0) return
      const scale = pinch.startDistPx / dist
      const width = pinch.viewportAtStart.end - pinch.viewportAtStart.start
      const ratio = width > 0 ? (pinch.centerFrac - pinch.viewportAtStart.start) / width : 0.5
      const newWidth = Math.min(1, Math.max(0.0001, width * scale))
      const start = pinch.centerFrac - ratio * newWidth
      setViewport({ start, end: start + newWidth })
    }
    function onNativeUp(e: PointerEvent) {
      positions.delete(e.pointerId)
    }
    el.addEventListener('pointerdown', onNativeDown)
    el.addEventListener('pointermove', onNativeMove)
    el.addEventListener('pointerup', onNativeUp)
    el.addEventListener('pointercancel', onNativeUp)
    return () => {
      el.removeEventListener('pointerdown', onNativeDown)
      el.removeEventListener('pointermove', onNativeMove)
      el.removeEventListener('pointerup', onNativeUp)
      el.removeEventListener('pointercancel', onNativeUp)
    }
  }, [setViewport])

  const onDoubleClick = useCallback(() => fit(), [fit])
  const onMouseLeave = useCallback(() => setHover(null), [])

  const markerPct = totalS > 0 ? toViewportPct(currentOffsetS / totalS, viewport) : 0
  const showMarker = markerPct >= 0 && markerPct <= 100

  const ticks = useMemo(() => {
    if (!isZoomed || totalS <= 0) return []
    const vWidth = viewport.end - viewport.start
    const visibleSpanS = vWidth * totalS
    const stepS = pickTickStepS(visibleSpanS)
    const startS = viewport.start * totalS
    const endS = viewport.end * totalS
    const first = Math.ceil(startS / stepS) * stepS
    const out: { atS: number; pct: number }[] = []
    for (let t = first; t <= endS; t += stepS) {
      out.push({ atS: t, pct: toViewportPct(t / totalS, viewport) })
    }
    return out
  }, [isZoomed, totalS, viewport])

  const hoverPct = hover ? toViewportPct(hover.absFrac, viewport) : null

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center gap-1.5">
        <button
          type="button"
          onClick={() => zoomIn()}
          className="flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-text-secondary hover:border-text-muted"
        >
          <Plus className="size-3" /> Zoom in
        </button>
        <button
          type="button"
          onClick={() => zoomOut()}
          className="flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-text-secondary hover:border-text-muted"
        >
          <Minus className="size-3" /> Zoom out
        </button>
        <button
          type="button"
          onClick={fit}
          disabled={!isZoomed}
          className="flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-text-secondary disabled:opacity-40 hover:border-text-muted"
        >
          <Scan className="size-3" /> Fit
        </button>
        {hover && (
          <span className="ml-auto text-xs tabular-nums text-text-muted">
            {formatDuration(hover.absFrac * totalS)}
            {hover.seg ? ` · ${hover.seg.camera} seg ${hover.seg.seq}` : ''}
          </span>
        )}
      </div>

      <div
        ref={trackRef}
        className="relative flex touch-none select-none flex-col gap-0.5 py-1"
        style={{ cursor: 'crosshair' }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onMouseLeave={onMouseLeave}
        onDoubleClick={onDoubleClick}
      >
        <Row segments={front} totalS={totalS} maxCount={maxCount} viewport={viewport} />
        <Row segments={rear} totalS={totalS} maxCount={maxCount} viewport={viewport} />

        {ticks.map((tick) => (
          <div
            key={tick.atS}
            className="pointer-events-none absolute top-0 h-full w-px bg-surface-2"
            style={{ left: `${tick.pct}%` }}
          />
        ))}

        {selection && (
          <div
            className="pointer-events-none absolute top-0 h-full bg-sky-400/25"
            style={{
              left: `${Math.min(toViewportPct(selection.startFrac, viewport), toViewportPct(selection.endFrac, viewport))}%`,
              width: `${Math.abs(toViewportPct(selection.endFrac, viewport) - toViewportPct(selection.startFrac, viewport))}%`,
            }}
          />
        )}

        {showMarker && (
          <div
            className="pointer-events-none absolute top-0 h-full w-px bg-accent"
            style={{ left: `${markerPct}%` }}
          />
        )}
        {hoverPct != null && !selection && (
          <div
            className="pointer-events-none absolute -top-5 -translate-x-1/2 rounded bg-black/80 px-1.5 py-0.5 text-xs tabular-nums text-white"
            style={{ left: `${hoverPct}%` }}
          >
            {formatDuration((hover?.absFrac ?? 0) * totalS)}
          </div>
        )}
      </div>

      {isZoomed && ticks.length > 0 && (
        <div className="relative h-3 text-xs tabular-nums text-text-muted">
          {ticks.map((tick) => (
            <span key={tick.atS} className="absolute -translate-x-1/2" style={{ left: `${tick.pct}%` }}>
              {formatDuration(tick.atS)}
            </span>
          ))}
        </div>
      )}

      <div className="flex justify-between text-xs tabular-nums text-text-muted">
        <span>{formatDuration(viewport.start * totalS)}</span>
        <span>{formatDuration(viewport.end * totalS)}</span>
      </div>

      <TimelineOverview viewport={viewport} onPan={setViewport} />
    </div>
  )
}
