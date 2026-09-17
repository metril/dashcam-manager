// TelemetryHUD: a small absolutely-positioned readout (speed / RPM /
// throttle) over the front video, driven by the same `currentTimeS` the
// player already tracks off `timeupdate` (~4Hz) — plenty for a numeric
// gauge, unlike DetectionOverlay's per-frame box drawing. Fetches its own
// segment's OBD points (mirrors DetectionOverlay's self-fetch-by-segmentId
// pattern) and renders nothing at all when the segment has no telemetry,
// per the "no placeholder UI" rule — including the 404 GET /api/segments/
// {id}/obd returns for a segment with no .obd.json sidecar, which is
// treated as "no telemetry" rather than an error.
import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ApiError } from '../../lib/api'
import { getSegmentOBD } from '../../lib/vehicleApi'
import { speedParts, useUnits } from '../../lib/units'
import { buildOBDIndex, sampleAt } from './obdInterp'

export default function TelemetryHUD({
  segmentId,
  currentTimeS,
  visible,
  hasObd,
}: {
  segmentId: number | null
  currentTimeS: number
  visible: boolean
  /** TimelineSegment.has_obd for this segment (Task 8 fix round 1) — false skips the query outright (a guaranteed 404); undefined still fetches. */
  hasObd?: boolean
}) {
  const units = useUnits()
  const { data, error } = useQuery({
    queryKey: ['segment-obd', segmentId],
    queryFn: () => getSegmentOBD(segmentId!),
    enabled: segmentId != null && visible && hasObd !== false,
    staleTime: Infinity,
    // A 404 means "no OBD sidecar for this segment" — not transient, so
    // don't burn retries on it; other failures still get a couple.
    retry: (failureCount, err) => !(err instanceof ApiError && err.status === 404) && failureCount < 2,
  })

  const index = useMemo(() => (data?.points ? buildOBDIndex(data.points) : null), [data])
  const sample = index ? sampleAt(index, currentTimeS) : null

  const notFound = error instanceof ApiError && error.status === 404
  // No telemetry at all for this segment, or nothing at this instant (a
  // dropout past obdInterp's MAX_GAP_S hold) — nothing to show, not a
  // placeholder of dashes.
  if (!visible || notFound || !data?.summary || !sample) return null

  // The unit label doesn't depend on the value itself, so speedParts(0,
  // ...) gives the right unit string even when there's no live speed
  // sample to convert yet, without a third hard-coded 'mph'/'km/h' pair.
  const speedUnit = speedParts(0, units).unit

  return (
    <div
      className="pointer-events-none absolute right-2 top-2 flex flex-col items-end gap-0.5 rounded-md bg-black/60 px-2.5 py-1.5 text-right text-white"
      aria-hidden="true"
    >
      <div className="flex items-baseline gap-1">
        <span className="text-2xl font-semibold leading-none tabular-nums">
          {sample.speed_kph != null ? speedParts(sample.speed_kph, units).value : '–'}
        </span>
        <span className="text-[10px] uppercase text-white/70">{speedUnit}</span>
      </div>
      <div className="flex items-center gap-2 text-xs tabular-nums text-white/80">
        <span>{sample.rpm != null ? `${Math.round(sample.rpm)} rpm` : '– rpm'}</span>
        <span>{sample.throttle_pct != null ? `${Math.round(sample.throttle_pct)}% throttle` : '–% throttle'}</span>
      </div>
    </div>
  )
}
