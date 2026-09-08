// TimelineStrip renders the whole trip as one horizontal strip: segment
// boundaries (front + rear rows) with a detection-density heat tint,
// click/drag to seek by absolute offset-from-trip-start, and a marker for
// the current position.
import { useCallback, useRef, useState } from 'react'
import type { TimelineSegment, TripTimeline } from '../../types'
import { formatDuration } from '../../lib/format'

function heatColor(count: number, max: number): string {
  if (count <= 0 || max <= 0) return 'rgba(64,64,64,0.5)'
  const t = Math.min(1, count / max)
  // amber ramp: dim at low density, saturated amber-500 at max.
  const alpha = 0.25 + t * 0.65
  return `rgba(245,158,11,${alpha.toFixed(2)})`
}

function Row({
  segments,
  totalS,
  maxCount,
}: {
  segments: TimelineSegment[]
  totalS: number
  maxCount: number
}) {
  if (segments.length === 0 || totalS <= 0) return null
  return (
    <div className="relative h-3.5 w-full overflow-hidden rounded-sm bg-neutral-900">
      {segments.map((seg) => {
        const leftPct = (seg.offset_s / totalS) * 100
        const widthPct = Math.max(0.3, (seg.duration_s / totalS) * 100)
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

export default function TimelineStrip({
  timeline,
  currentOffsetS,
  onSeek,
}: {
  timeline: TripTimeline | undefined
  currentOffsetS: number
  onSeek: (offsetS: number) => void
}) {
  const trackRef = useRef<HTMLDivElement | null>(null)
  const [hoverPct, setHoverPct] = useState<number | null>(null)

  const totalS = timeline?.duration_s ?? 0
  const front = timeline?.segments.filter((s) => s.camera === 'front') ?? []
  const rear = timeline?.segments.filter((s) => s.camera === 'rear') ?? []
  const maxCount = Math.max(1, ...(timeline?.segments.map((s) => s.count) ?? [1]))

  const pctFromEvent = useCallback((e: { clientX: number }) => {
    const el = trackRef.current
    if (!el) return null
    const rect = el.getBoundingClientRect()
    return Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width))
  }, [])

  const handleClick = useCallback(
    (e: React.MouseEvent) => {
      const pct = pctFromEvent(e)
      if (pct == null || totalS <= 0) return
      onSeek(pct * totalS)
    },
    [pctFromEvent, totalS, onSeek],
  )

  const handleMove = useCallback(
    (e: React.MouseEvent) => {
      const pct = pctFromEvent(e)
      setHoverPct(pct)
    },
    [pctFromEvent],
  )

  const markerPct = totalS > 0 ? Math.min(100, Math.max(0, (currentOffsetS / totalS) * 100)) : 0

  return (
    <div className="flex flex-col gap-1">
      <div
        ref={trackRef}
        className="relative flex cursor-pointer flex-col gap-0.5 py-1"
        onClick={handleClick}
        onMouseMove={handleMove}
        onMouseLeave={() => setHoverPct(null)}
      >
        <Row segments={front} totalS={totalS} maxCount={maxCount} />
        <Row segments={rear} totalS={totalS} maxCount={maxCount} />
        <div
          className="pointer-events-none absolute top-0 h-full w-px bg-amber-400"
          style={{ left: `${markerPct}%` }}
        />
        {hoverPct != null && (
          <div
            className="pointer-events-none absolute -top-5 -translate-x-1/2 rounded bg-black/80 px-1.5 py-0.5 text-[10px] tabular-nums text-neutral-200"
            style={{ left: `${hoverPct * 100}%` }}
          >
            {formatDuration(hoverPct * totalS)}
          </div>
        )}
      </div>
      <div className="flex justify-between text-[10px] tabular-nums text-neutral-600">
        <span>0:00</span>
        <span>{formatDuration(totalS)}</span>
      </div>
    </div>
  )
}
