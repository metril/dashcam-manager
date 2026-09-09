// TimelineOverview: a thin bar spanning the whole trip with a rectangle
// showing the current zoomed viewport. Drag the rectangle (or click/drag
// anywhere on the bar) to pan the main TimelineStrip.
import { useCallback, useRef } from 'react'
import type { Viewport } from './useTimelineViewport'

export default function TimelineOverview({
  viewport,
  onPan,
}: {
  viewport: Viewport
  onPan: (v: Viewport) => void
}) {
  const trackRef = useRef<HTMLDivElement | null>(null)
  const dragRef = useRef<{ pointerId: number; grabOffsetFrac: number; width: number } | null>(null)

  const fracFromX = useCallback((clientX: number) => {
    const el = trackRef.current
    if (!el) return 0
    const rect = el.getBoundingClientRect()
    return rect.width > 0 ? Math.min(1, Math.max(0, (clientX - rect.left) / rect.width)) : 0
  }, [])

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      const width = viewport.end - viewport.start
      const clickFrac = fracFromX(e.clientX)
      const withinRect = clickFrac >= viewport.start && clickFrac <= viewport.end
      // Grabbing inside the viewport rectangle preserves the grab point;
      // clicking outside it re-centers the viewport on the click instead.
      const grabOffsetFrac = withinRect ? clickFrac - viewport.start : width / 2
      dragRef.current = { pointerId: e.pointerId, grabOffsetFrac, width }
      ;(e.target as Element).setPointerCapture(e.pointerId)
      const start = Math.min(1 - width, Math.max(0, clickFrac - grabOffsetFrac))
      onPan({ start, end: start + width })
    },
    [viewport, fracFromX, onPan],
  )

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      const drag = dragRef.current
      if (!drag || drag.pointerId !== e.pointerId) return
      const clickFrac = fracFromX(e.clientX)
      const start = Math.min(1 - drag.width, Math.max(0, clickFrac - drag.grabOffsetFrac))
      onPan({ start, end: start + drag.width })
    },
    [fracFromX, onPan],
  )

  const onPointerUp = useCallback((e: React.PointerEvent) => {
    if (dragRef.current?.pointerId === e.pointerId) dragRef.current = null
  }, [])

  return (
    <div
      ref={trackRef}
      className="relative h-2 w-full cursor-pointer rounded-sm bg-neutral-900/80 touch-none"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
      title="Drag to pan the timeline"
    >
      <div
        className="absolute top-0 h-full rounded-sm border border-amber-500/70 bg-amber-500/20"
        style={{
          left: `${viewport.start * 100}%`,
          width: `${Math.max(0.5, (viewport.end - viewport.start) * 100)}%`,
        }}
      />
    </div>
  )
}
