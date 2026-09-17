// TelemetryStrip: a small inline-SVG strip under TimelineStrip showing the
// active front segment's OBD telemetry — speed as a filled area, RPM as a
// line on its own scale — with a playhead synced to playback and
// click/drag seeking through the same `onSeek` the timeline uses. Scoped
// to just the on-screen segment (not the whole trip) for the same reason
// TimelineStrip's trackMarkers are: DualVideoPlayer already knows which
// segment is active and its trip-relative offset, so this is free to
// fetch, and a trip can have thousands of segments.
//
// segStartOffsetS/segDurationS are the segment's trip-relative bounds
// (TripTimeline's offset_s/duration_s for this segment); currentOffsetS
// and onSeek are trip-relative, matching TimelineStrip's own props — this
// component converts to/from the segment-local offsets OBD points use
// internally.
import { useCallback, useMemo, useRef } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError } from '../../lib/api'
import { getSegmentOBD } from '../../lib/vehicleApi'
import { MAX_GAP_S } from './obdInterp'
import type { OBDPoint } from '../../types'

const VIEW_W = 1000
const VIEW_H = 40
const EMPTY_POINTS: OBDPoint[] = []

// splitRuns groups a points array (already filtered to one field's
// non-null samples) into consecutive runs with no gap larger than
// MAX_GAP_S — the same threshold obdInterp's sampleAt uses to refuse
// interpolating across a dropout. Drawing straight through such a gap
// would fabricate a ramp across data that was never recorded.
function splitRuns(points: OBDPoint[]): OBDPoint[][] {
  const runs: OBDPoint[][] = []
  let current: OBDPoint[] = []
  for (const p of points) {
    if (current.length > 0 && p.offset_s - current[current.length - 1].offset_s > MAX_GAP_S) {
      runs.push(current)
      current = []
    }
    current.push(p)
  }
  if (current.length > 0) runs.push(current)
  return runs
}

export default function TelemetryStrip({
  segmentId,
  segStartOffsetS,
  segDurationS,
  currentOffsetS,
  onSeek,
}: {
  segmentId: number | null
  segStartOffsetS: number
  segDurationS: number
  currentOffsetS: number
  onSeek: (offsetS: number) => void
}) {
  const { data, error } = useQuery({
    queryKey: ['segment-obd', segmentId],
    queryFn: () => getSegmentOBD(segmentId!),
    enabled: segmentId != null,
    staleTime: Infinity,
    retry: (failureCount, err) => !(err instanceof ApiError && err.status === 404) && failureCount < 2,
  })

  const svgRef = useRef<SVGSVGElement | null>(null)

  const points = data?.points ?? EMPTY_POINTS

  const chart = useMemo(() => {
    if (points.length === 0 || segDurationS <= 0) return null
    const maxSpeed = Math.max(1, ...points.map((p) => p.speed_kph ?? 0))
    const maxRpm = Math.max(1, ...points.map((p) => p.rpm ?? 0))
    const xAt = (offsetS: number) => (Math.min(segDurationS, Math.max(0, offsetS)) / segDurationS) * VIEW_W
    const speedYAt = (v: number) => VIEW_H - (v / maxSpeed) * VIEW_H
    const rpmYAt = (v: number) => VIEW_H - (v / maxRpm) * VIEW_H

    const speedRuns = splitRuns(points.filter((p) => p.speed_kph != null))
    const speedPath = speedRuns
      .map((run) => {
        const line = run.map((p) => `${xAt(p.offset_s).toFixed(1)},${speedYAt(p.speed_kph!).toFixed(1)}`)
        const startX = xAt(run[0].offset_s).toFixed(1)
        const endX = xAt(run[run.length - 1].offset_s).toFixed(1)
        return `M${startX},${VIEW_H} L${line.join(' L')} L${endX},${VIEW_H} Z`
      })
      .join(' ')

    const rpmRuns = splitRuns(points.filter((p) => p.rpm != null))
    const rpmPath = rpmRuns
      .map((run) => `M${run.map((p) => `${xAt(p.offset_s).toFixed(1)},${rpmYAt(p.rpm!).toFixed(1)}`).join(' L')}`)
      .join(' ')

    return { speedPath, rpmPath, maxSpeed, maxRpm }
  }, [points, segDurationS])

  const seekFromClientX = useCallback(
    (clientX: number) => {
      const svg = svgRef.current
      if (!svg || segDurationS <= 0) return
      const rect = svg.getBoundingClientRect()
      const pct = rect.width > 0 ? Math.min(1, Math.max(0, (clientX - rect.left) / rect.width)) : 0
      onSeek(segStartOffsetS + pct * segDurationS)
    },
    [segDurationS, segStartOffsetS, onSeek],
  )

  const onPointerDown = useCallback(
    (e: React.PointerEvent<SVGSVGElement>) => {
      e.currentTarget.setPointerCapture(e.pointerId)
      seekFromClientX(e.clientX)
    },
    [seekFromClientX],
  )
  const onPointerMove = useCallback(
    (e: React.PointerEvent<SVGSVGElement>) => {
      if (e.buttons !== 1) return
      seekFromClientX(e.clientX)
    },
    [seekFromClientX],
  )

  const notFound = error instanceof ApiError && error.status === 404
  if (notFound || !chart) return null

  const localOffsetS = currentOffsetS - segStartOffsetS
  const showPlayhead = localOffsetS >= 0 && localOffsetS <= segDurationS
  const playheadPct = segDurationS > 0 ? Math.min(1, Math.max(0, localOffsetS / segDurationS)) * 100 : 0

  return (
    <div className="flex flex-col gap-0.5">
      <svg
        ref={svgRef}
        viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
        preserveAspectRatio="none"
        className="h-10 w-full touch-none rounded-sm bg-surface"
        style={{ cursor: 'crosshair' }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        role="slider"
        aria-label="Telemetry position"
        aria-valuemin={0}
        aria-valuemax={segDurationS}
        aria-valuenow={showPlayhead ? localOffsetS : undefined}
      >
        {chart.speedPath && <path d={chart.speedPath} fill="var(--color-accent)" fillOpacity={0.25} stroke="none" />}
        {chart.rpmPath && (
          <path
            d={chart.rpmPath}
            fill="none"
            stroke="var(--color-warn)"
            strokeWidth={1.5}
            vectorEffect="non-scaling-stroke"
          />
        )}
        {showPlayhead && (
          <line
            x1={`${playheadPct}%`}
            x2={`${playheadPct}%`}
            y1={0}
            y2={VIEW_H}
            stroke="var(--color-text)"
            strokeWidth={1}
            vectorEffect="non-scaling-stroke"
          />
        )}
      </svg>
      <div className="flex justify-between text-[10px] tabular-nums text-text-muted">
        <span className="text-accent">Speed · {Math.round(chart.maxSpeed)} km/h max</span>
        <span className="text-warn">RPM · {Math.round(chart.maxRpm)} max</span>
      </div>
    </div>
  )
}
