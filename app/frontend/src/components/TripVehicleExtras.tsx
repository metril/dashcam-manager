import { Gauge, Route, Thermometer, TriangleAlert } from 'lucide-react'
import type { TripVehicleTotals } from '../types'
import { formatOdometer, formatRange, formatTemperature, LOW_TIRE_KPA, pressureParts, type UnitSystem } from '../lib/units'

function Item({ icon: Icon, label, value }: { icon: typeof Route; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <Icon className="size-3.5 text-text-muted" strokeWidth={2} />
      <span className="tabular-nums text-[13px] font-semibold text-text">{value}</span>
      <span className="text-xs text-text-muted">{label}</span>
    </div>
  )
}

const span = (a: string, b: string, sep: string) => (a === b ? a : `${a}${sep}${b}`)

const WHEELS = [
  ['fl', 'FL'],
  ['fr', 'FR'],
  ['rl', 'RL'],
  ['rr', 'RR'],
] as const

// TripVehicleExtras renders odometer, range, ambient temperature and the tire
// block of a trip's vehicle totals; each piece only when its data is present.
export default function TripVehicleExtras({ totals, units }: { totals: TripVehicleTotals; units: UnitSystem }) {
  const t = totals
  const odoEnd = t.odometer_end_km
  const odoStart = t.odometer_start_km
  const odo = odoEnd ?? odoStart
  const rangeEnd = t.range_end_km ?? t.range_start_km
  const rangeStart = t.range_end_km != null ? t.range_start_km : null
  const ambMin = t.ambient_min_c ?? t.ambient_max_c
  const ambMax = t.ambient_max_c ?? t.ambient_min_c

  const tires = WHEELS.map(([k, label]) => ({
    k,
    label,
    avg: t[`tire_${k}_avg_kpa`],
    min: t[`tire_${k}_min_kpa`],
    max: t[`tire_${k}_max_kpa`],
  }))
  const hasTires = tires.some((w) => w.avg != null || w.min != null || w.max != null)

  return (
    <>
      {odo != null && (
        <Item
          icon={Route}
          label="Odometer"
          value={odoStart != null && odoEnd != null ? `${formatOdometer(odoStart, units)} → ${formatOdometer(odoEnd, units)}` : formatOdometer(odo, units)}
        />
      )}
      {rangeEnd != null && (
        <Item
          icon={Gauge}
          label="Range"
          value={rangeStart != null ? `${formatRange(rangeStart, units)} → ${formatRange(rangeEnd, units)}` : formatRange(rangeEnd, units)}
        />
      )}
      {ambMin != null && ambMax != null && (
        <Item
          icon={Thermometer}
          label="Outside temp"
          value={span(formatTemperature(ambMin, units), formatTemperature(ambMax, units), '–')}
        />
      )}
      {hasTires && (
        <div className="grid grid-cols-2 gap-1.5" data-testid="tire-block" aria-label="Tire pressure">
          {tires.map((w) => {
            const shown = w.avg ?? w.min ?? w.max
            if (shown == null) return <div key={w.k} />
            const low = w.min != null && w.min < LOW_TIRE_KPA
            const p = pressureParts(shown, units)
            const lo = pressureParts(w.min ?? shown, units).value
            const hi = pressureParts(w.max ?? shown, units).value
            return (
              <div
                key={w.k}
                data-wheel={w.k}
                data-low={low ? 'true' : undefined}
                title={low ? `${w.label} low tire pressure` : undefined}
                className={[
                  'flex min-w-[4.5rem] items-baseline gap-1.5 rounded-sm border px-1.5 py-0.5',
                  low ? 'border-warn text-warn' : 'border-border text-text',
                ].join(' ')}
              >
                <span className="text-[10px] font-medium text-text-muted">{w.label}</span>
                {low && <TriangleAlert className="size-3 self-center" strokeWidth={2} aria-label="Low" />}
                <span className="tabular-nums text-[13px] font-semibold">{p.value}</span>
                <span className="tabular-nums text-[10px] text-text-muted">
                  {span(lo, hi, '–')} {p.unit}
                </span>
              </div>
            )
          })}
        </div>
      )}
    </>
  )
}
