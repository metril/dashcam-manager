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
import {
  formatOdometer,
  formatPressure,
  formatRange,
  formatTemperature,
  LOW_TIRE_KPA,
  speedParts,
  useUnits,
  type UnitSystem,
} from '../../lib/units'
import { buildOBDIndex, sampleAt, type OBDSample } from './obdInterp'

const TIRES = [
  { label: 'FL', key: 'tire_fl_kpa' },
  { label: 'FR', key: 'tire_fr_kpa' },
  { label: 'RL', key: 'tire_rl_kpa' },
  { label: 'RR', key: 'tire_rr_kpa' },
] as const

// ExtReadings: compact tire pressures / odometer / range / ambient rows for
// the HUD. Null values are omitted; renders nothing when all are null.
export function ExtReadings({ sample, units }: { sample: OBDSample; units: UnitSystem }) {
  const tires = TIRES.filter((t) => sample[t.key] != null)
  const other = [
    sample.odometer_km != null ? `${formatOdometer(sample.odometer_km, units)} odo` : null,
    sample.range_km != null ? `${formatRange(sample.range_km, units)} range` : null,
    sample.ambient_c != null ? formatTemperature(sample.ambient_c, units) : null,
  ].filter((s): s is string => s != null)
  if (tires.length === 0 && other.length === 0) return null
  return (
    <div className="flex flex-col items-end gap-0.5 text-[10px] tabular-nums text-white/80" data-testid="hud-ext">
      {tires.length > 0 && (
        <div className="flex gap-2">
          {tires.map((t) => {
            const kpa = sample[t.key]!
            const low = kpa < LOW_TIRE_KPA
            return (
              <span key={t.key} data-wheel={t.label.toLowerCase()} data-low={low ? 'true' : undefined} className={low ? 'text-warn' : undefined}>
                {t.label} {formatPressure(kpa, units)}
              </span>
            )
          })}
        </div>
      )}
      {other.length > 0 && <div className="flex gap-2">{other.map((s) => <span key={s}>{s}</span>)}</div>}
    </div>
  )
}

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
      <ExtReadings sample={sample} units={units} />
    </div>
  )
}
