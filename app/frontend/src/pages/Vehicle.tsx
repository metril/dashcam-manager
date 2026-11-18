// Vehicle page (Task 8): Footage > Vehicle — a standalone view over the
// OBD telemetry that TripDetail otherwise only shows per-trip. Live
// recorder OBD status, a cross-trip event feed (type/date filters, cursor
// paging), a DTC history table, and battery/coolant trend sparklines.
// Lazy-loaded from App.tsx like Catalog.
import { useMemo, useState } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { AlertTriangle, Battery, CircleDot, Gauge, Loader2, Thermometer, Wrench } from 'lucide-react'
import { Link } from 'react-router-dom'
import { getVehicleDTCs, getVehicleStatus, getVehicleTrends, listVehicleEvents } from '../lib/vehicleApi'
import { formatClockTime, formatDateLong, titleCase } from '../lib/format'
import {
  formatDistance,
  formatOdometer,
  formatPressure,
  formatRange,
  formatSpeed,
  formatTemperature,
  LOW_TIRE_KPA,
  pressureParts,
  temperatureUnit,
  toDisplayTemperature,
  useUnits,
  type UnitSystem,
} from '../lib/units'
import { VEHICLE_EVENT_TYPES, vehicleEventMeta, wheelLabel } from '../lib/vehicleEvents'
import Segmented from '../components/ui/Segmented'
import type { VehicleEvent, VehicleEventType, VehicleObdLive, VehicleTrend, Wheel } from '../types'

const PAGE_SIZE = 50

// --- live status card --------------------------------------------------

function readStr(obj: Record<string, unknown> | null | undefined, key: string): string | null {
  const v = obj?.[key]
  return typeof v === 'string' ? v : v != null ? String(v) : null
}
function readBool(obj: Record<string, unknown> | null | undefined, key: string): boolean | null {
  const v = obj?.[key]
  return typeof v === 'boolean' ? v : null
}


// --- extended live readings (tires / odometer / range / ambient) -----------

const TIRE_LAYOUT: { wheel: Wheel; key: keyof VehicleObdLive; label: string }[] = [
  { wheel: 'fl', key: 'tire_fl_kpa', label: 'FL' },
  { wheel: 'fr', key: 'tire_fr_kpa', label: 'FR' },
  { wheel: 'rl', key: 'tire_rl_kpa', label: 'RL' },
  { wheel: 'rr', key: 'tire_rr_kpa', label: 'RR' },
]

function num(obd: Record<string, unknown> | null | undefined, key: string): number | null {
  const v = obd?.[key]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

// Renders nothing when every extended reading is null/absent.
export function LiveReadings({ obd, units }: { obd: Record<string, unknown> | null | undefined; units: UnitSystem }) {
  const tires = TIRE_LAYOUT.map((t) => ({ ...t, kpa: num(obd, t.key) }))
  const hasTires = tires.some((t) => t.kpa != null)
  const odo = num(obd, 'odometer_km')
  const range = num(obd, 'range_km')
  const ambient = num(obd, 'ambient_c')
  if (!hasTires && odo == null && range == null && ambient == null) return null

  return (
    <div className="flex w-full flex-wrap items-center gap-x-6 gap-y-3 border-t border-border pt-3" data-testid="live-readings">
      {hasTires && (
        <div className="grid grid-cols-2 gap-1.5" role="group" aria-label="Tire pressure">
          {tires.map((t) => {
            const low = t.kpa != null && t.kpa < LOW_TIRE_KPA
            return (
              <div
                key={t.wheel}
                data-wheel={t.wheel}
                className={[
                  'flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs tabular-nums',
                  low ? 'border-warn text-warn' : 'border-border text-text',
                  t.kpa == null ? 'opacity-50' : '',
                ].join(' ')}
              >
                <span className="w-4 text-[10px] font-semibold text-text-muted">{t.label}</span>
                {t.kpa != null ? (
                  <>
                    <span>{formatPressure(t.kpa, units)}</span>
                    {low && (
                      <span className="flex items-center gap-0.5 font-medium">
                        <AlertTriangle className="size-3" aria-hidden /> Low
                      </span>
                    )}
                  </>
                ) : (
                  <span className="text-text-muted">no data</span>
                )}
              </div>
            )
          })}
        </div>
      )}
      <div className="flex flex-wrap gap-x-6 gap-y-2">
        {odo != null && <Reading label="Odometer" value={formatOdometer(odo, units)} />}
        {range != null && <Reading label="Range" value={formatRange(range, units)} />}
        {ambient != null && <Reading label="Ambient" value={formatTemperature(ambient, units)} />}
      </div>
    </div>
  )
}

function Reading({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col">
      <span className="text-[11px] uppercase tracking-wide text-text-muted">{label}</span>
      <span className="text-[13px] font-medium tabular-nums text-text">{value}</span>
    </div>
  )
}

function StatusCard() {
  const units = useUnits()
  const { data, isLoading } = useQuery({
    queryKey: ['vehicle-status'],
    queryFn: () => getVehicleStatus(),
    refetchInterval: 10000,
  })

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-border bg-surface/50 px-4 py-3 text-xs text-text-muted">
        <Loader2 className="size-3.5 animate-spin" /> Loading vehicle status…
      </div>
    )
  }

  if (data?.error_code) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-border bg-surface/50 px-4 py-3 text-xs text-text-muted">
        <AlertTriangle className="size-3.5" />
        {data.message ?? titleCase(data.error_code)}
      </div>
    )
  }

  const enabled = readBool(data?.obd, 'enabled')
  const connected = readBool(data?.obd, 'connected')
  const inTrip = data?.trip != null
  const dot = connected ? 'bg-success' : enabled ? 'bg-warn' : 'bg-text-muted'
  const label = connected ? 'Connected' : enabled ? 'Enabled, not connected' : 'OBD disabled'

  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-3 rounded-lg border border-border bg-surface/50 px-4 py-3">
      <span className="flex items-center gap-1.5 text-[13px] font-medium text-text">
        <span className={['size-2 rounded-full', dot].join(' ')} />
        {label}
      </span>
      <span className="text-xs text-text-muted">{inTrip ? 'Trip in progress' : 'No active trip'}</span>
      {readStr(data?.trip, 'distance_m') && (
        <span className="text-xs text-text-muted">
          Distance so far {formatDistance(Number(readStr(data?.trip, 'distance_m')), units)}
        </span>
      )}
      {data?.last_trip && (
        <span className="text-xs text-text-muted">
          Last trip ended {formatClockTime(readStr(data.last_trip, 'end_time'))}
        </span>
      )}
      <LiveReadings obd={data?.obd} units={units} />
    </div>
  )
}

// --- sparklines ----------------------------------------------------------

function Sparkline({
  values,
  color,
  height = 48,
  unit,
}: {
  values: (number | null)[]
  color: string
  height?: number
  unit: string
}) {
  const pts = values.map((v, i) => ({ i, v })).filter((p): p is { i: number; v: number } => p.v != null)
  if (pts.length < 2) {
    return <div className="flex h-12 items-center text-xs text-text-muted">Not enough data</div>
  }
  const width = 240
  const min = Math.min(...pts.map((p) => p.v))
  const max = Math.max(...pts.map((p) => p.v))
  const span = max - min || 1
  const x = (i: number) => (i / (values.length - 1)) * width
  const y = (v: number) => height - ((v - min) / span) * (height - 8) - 4
  const path = pts.map((p, idx) => `${idx === 0 ? 'M' : 'L'}${x(p.i).toFixed(1)},${y(p.v).toFixed(1)}`).join(' ')

  return (
    <svg viewBox={`0 0 ${width} ${height}`} className="h-12 w-full" preserveAspectRatio="none">
      <path d={path} fill="none" stroke={`var(${color})`} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
      <text x={0} y={height - 1} className="fill-text-muted" style={{ font: '9px system-ui' }}>
        {min.toFixed(1)}
        {unit}
      </text>
      <text x={width} y={9} textAnchor="end" className="fill-text-muted" style={{ font: '9px system-ui' }}>
        {max.toFixed(1)}
        {unit}
      </text>
    </svg>
  )
}


// MultiSparkline: several series on one shared y-scale. Series are told
// apart by color token, dash pattern and legend label (not color alone).
export interface Series {
  label: string
  values: (number | null)[]
  color: string
  dash?: string
}

export function MultiSparkline({ series, unit, height = 48 }: { series: Series[]; unit: string; height?: number }) {
  const n = Math.max(0, ...series.map((s) => s.values.length))
  const withData = series.filter((s) => s.values.filter((v) => v != null).length >= 2)
  if (n < 2 || withData.length === 0) {
    return <div className="flex h-12 items-center text-xs text-text-muted">Not enough data</div>
  }
  const all = withData.flatMap((s) => s.values.filter((v): v is number => v != null))
  const width = 240
  const min = Math.min(...all)
  const max = Math.max(...all)
  const span = max - min || 1
  const x = (i: number) => (i / (n - 1)) * width
  const y = (v: number) => height - ((v - min) / span) * (height - 8) - 4
  const pathOf = (vals: (number | null)[]) => {
    let d = ''
    let pen = false
    vals.forEach((v, i) => {
      if (v == null) {
        pen = false
        return
      }
      d += `${pen ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)} `
      pen = true
    })
    return d.trim()
  }

  return (
    <div className="flex flex-col gap-1">
      <svg viewBox={`0 0 ${width} ${height}`} className="h-12 w-full" preserveAspectRatio="none" role="img" aria-label={withData.map((s) => s.label).join(', ')}>
        {withData.map((s) => (
          <path
            key={s.label}
            d={pathOf(s.values)}
            fill="none"
            stroke={`var(${s.color})`}
            strokeWidth={1.5}
            strokeDasharray={s.dash}
            vectorEffect="non-scaling-stroke"
          >
            <title>{s.label}</title>
          </path>
        ))}
        <text x={0} y={height - 1} className="fill-text-muted" style={{ font: '9px system-ui' }}>
          {min.toFixed(0)}
          {unit}
        </text>
        <text x={width} y={9} textAnchor="end" className="fill-text-muted" style={{ font: '9px system-ui' }}>
          {max.toFixed(0)}
          {unit}
        </text>
      </svg>
      <ul className="flex flex-wrap gap-x-3 text-[11px] text-text-muted" aria-label="Legend">
        {withData.map((s) => (
          <li key={s.label} className="flex items-center gap-1">
            <svg width="16" height="6" aria-hidden>
              <line x1="0" y1="3" x2="16" y2="3" stroke={`var(${s.color})`} strokeWidth={2} strokeDasharray={s.dash} />
            </svg>
            {s.label}
          </li>
        ))}
      </ul>
    </div>
  )
}

const TIRE_SERIES: { label: string; color: string; dash?: string; pick: (t: VehicleTrend) => number | null | undefined }[] = [
  { label: 'FL', color: '--color-det-vehicle', pick: (t) => t.tire_fl_avg_kpa },
  { label: 'FR', color: '--color-det-person', dash: '4 2', pick: (t) => t.tire_fr_avg_kpa },
  { label: 'RL', color: '--color-det-plate', dash: '1 2', pick: (t) => t.tire_rl_avg_kpa },
  { label: 'RR', color: '--color-det-sign', dash: '6 2 1 2', pick: (t) => t.tire_rr_avg_kpa },
]

function hasPoints(vals: (number | null | undefined)[]): boolean {
  return vals.filter((v) => v != null).length >= 2
}

function TrendsCard() {
  const units = useUnits()
  const { data, isLoading } = useQuery({
    queryKey: ['vehicle-trends'],
    queryFn: () => getVehicleTrends(30),
  })
  const trips = data?.trips ?? []
  const coolantUnit = temperatureUnit(units)
  const cToDisplay = (c: number | null | undefined) => (c == null ? null : toDisplayTemperature(c, units))
  const tireSeries: Series[] = TIRE_SERIES.map((ts) => ({
    label: ts.label,
    color: ts.color,
    dash: ts.dash,
    values: trips.map((t) => {
      const kpa = ts.pick(t)
      return kpa == null ? null : Number(pressureParts(kpa, units).value)
    }),
  }))
  const showTires = tireSeries.some((ts) => hasPoints(ts.values))
  const ambientVals = trips.map((t) => cToDisplay(t.ambient_avg_c))
  const tireUnit = pressureParts(0, units).unit

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border bg-surface/50 px-4 py-3">
      <h2 className="flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-text-muted">
        <Gauge className="size-3.5" /> Last 30 days
      </h2>
      {isLoading ? (
        <span className="text-xs text-text-muted">Loading…</span>
      ) : trips.length === 0 ? (
        <span className="text-xs text-text-muted">No recorder trips yet.</span>
      ) : (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div className="flex flex-col gap-1">
            <span className="flex items-center gap-1 text-xs text-text-muted">
              <Battery className="size-3" /> Battery max (V)
            </span>
            <Sparkline values={trips.map((t) => t.batt_max_v)} color="--color-accent" unit="V" />
          </div>
          <div className="flex flex-col gap-1">
            <span className="flex items-center gap-1 text-xs text-text-muted">
              <Thermometer className="size-3" /> Coolant max ({coolantUnit})
            </span>
            <Sparkline values={trips.map((t) => cToDisplay(t.coolant_max_c))} color="--color-warn" unit={` ${coolantUnit}`} />
          </div>
          {hasPoints(ambientVals) && (
            <div className="flex flex-col gap-1">
              <span className="flex items-center gap-1 text-xs text-text-muted">
                <Thermometer className="size-3" /> Ambient ({coolantUnit})
              </span>
              <Sparkline values={ambientVals} color="--color-accent" unit={` ${coolantUnit}`} />
            </div>
          )}
          {showTires && (
            <div className="flex flex-col gap-1">
              <span className="flex items-center gap-1 text-xs text-text-muted">
                <CircleDot className="size-3" /> Tire pressure ({tireUnit})
              </span>
              <MultiSparkline series={tireSeries} unit={` ${tireUnit}`} />
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// --- DTC history ----------------------------------------------------------

function DTCTable() {
  const { data, isLoading } = useQuery({ queryKey: ['vehicle-dtcs'], queryFn: () => getVehicleDTCs() })
  const dtcs = data?.dtcs ?? []

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border bg-surface/50 px-4 py-3">
      <h2 className="flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-text-muted">
        <Wrench className="size-3.5" /> DTC history
      </h2>
      {isLoading ? (
        <span className="text-xs text-text-muted">Loading…</span>
      ) : dtcs.length === 0 ? (
        <span className="text-xs text-text-muted">No diagnostic trouble codes recorded.</span>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead>
              <tr className="text-text-muted">
                <th className="py-1 pr-3 font-medium">Code</th>
                <th className="py-1 pr-3 font-medium">First seen</th>
                <th className="py-1 pr-3 font-medium">Last seen</th>
                <th className="py-1 font-medium">Trips</th>
              </tr>
            </thead>
            <tbody>
              {dtcs.map((d) => (
                <tr key={d.code} className="border-t border-border">
                  <td className="py-1.5 pr-3 font-mono font-medium text-danger">{d.code}</td>
                  <td className="py-1.5 pr-3 text-text-secondary">{formatDateLong(d.first_seen)}</td>
                  <td className="py-1.5 pr-3 text-text-secondary">{formatDateLong(d.last_seen)}</td>
                  <td className="py-1.5 tabular-nums text-text-secondary">{d.trip_count}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

// --- event list ------------------------------------------------------------

export function EventRow({ event }: { event: VehicleEvent }) {
  const units = useUnits()
  const meta = vehicleEventMeta(event.event_type)
  const Icon = meta.icon
  // The global GET /api/vehicle/events listing's offset_s is
  // segment-relative for a segment-linked event (only GET
  // /api/trips/{id}/vehicle computes a trip-relative offset — see
  // types.ts's VehicleEvent doc comment), so this can't safely deep-link
  // with `?t=`. Use the event's own wall_time (an absolute instant) via
  // TripDetail's `?at=` wall-clock jump instead.
  const linkable = event.trip_id != null && event.wall_time != null

  const body = (
    <div
      className={[
        'flex items-center gap-3 rounded-lg border border-border bg-surface/40 px-3 py-2 text-left',
        linkable ? 'transition-colors hover:border-text-muted hover:bg-surface' : '',
      ].join(' ')}
    >
      <Icon className="size-4 shrink-0" style={{ color: `var(${meta.color})` }} strokeWidth={2} />
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="text-[13px] font-medium text-text">
          {meta.label}
          {event.event_type === 'low_tire_pressure' && event.wheel ? ` · ${wheelLabel(event.wheel)}` : ''}
          {event.event_type === 'low_tire_pressure' && event.peak != null ? ` · ${formatPressure(event.peak, units)}` : ''}
        </span>
        <span className="text-xs text-text-muted">
          {event.wall_time ? formatDateLong(event.wall_time) : 'Unknown time'}
          {event.wall_time ? ` · ${formatClockTime(event.wall_time)}` : ''}
          {event.speed_kph != null ? ` · ${formatSpeed(event.speed_kph, units)}` : ''}
        </span>
      </div>
      {!linkable && (
        <span className="shrink-0 rounded-sm bg-surface-2/80 px-1.5 py-0.5 text-xs text-text-muted">No footage</span>
      )}
    </div>
  )

  if (!linkable) return body
  return (
    <Link to={`/footage/trips/${event.trip_id}?at=${encodeURIComponent(event.wall_time!)}`} className="block">
      {body}
    </Link>
  )
}

function EventsSection() {
  const [types, setTypes] = useState<VehicleEventType[]>([])
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')

  const typeParam = types.length > 0 ? types.join(',') : undefined

  const { data, isLoading, isFetchingNextPage, fetchNextPage, hasNextPage, isError } = useInfiniteQuery({
    queryKey: ['vehicle-events', typeParam, from, to],
    queryFn: ({ pageParam }) =>
      listVehicleEvents({ type: typeParam, from: from || undefined, to: to || undefined, cursor: pageParam, limit: PAGE_SIZE }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  })

  // De-duplicate by id: the cursor is an opaque offset token, so a page
  // fetched after new rows were inserted ahead of it can repeat a row
  // already rendered from an earlier page.
  const events = useMemo(() => {
    const all = data?.pages.flatMap((p) => p.events) ?? []
    const seen = new Set<number>()
    return all.filter((ev) => (seen.has(ev.id) ? false : (seen.add(ev.id), true)))
  }, [data])

  const typeOptions = VEHICLE_EVENT_TYPES.map((t) => {
    const meta = vehicleEventMeta(t)
    const Icon = meta.icon
    return {
      value: t,
      label: (
        <>
          <Icon className="size-3" style={{ color: `var(${meta.color})` }} strokeWidth={2} />
          {meta.label}
        </>
      ),
    }
  })

  return (
    <div className="flex flex-col gap-3">
      <Segmented<VehicleEventType>
        multiple
        aria-label="Event types"
        options={typeOptions}
        value={types}
        onChange={setTypes}
      />
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-xs text-text-muted">
          From
          <input
            type="date"
            value={from}
            onChange={(e) => setFrom(e.target.value)}
            className="rounded border border-border bg-bg px-2 py-1.5 text-[12px] text-text outline-none focus:border-accent"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-text-muted">
          To
          <input
            type="date"
            value={to}
            onChange={(e) => setTo(e.target.value)}
            className="rounded border border-border bg-bg px-2 py-1.5 text-[12px] text-text outline-none focus:border-accent"
          />
        </label>
      </div>

      {isLoading && <span className="text-xs text-text-muted">Loading events…</span>}
      {isError && <span className="text-xs text-text-muted">Couldn't load events.</span>}
      {!isLoading && !isError && events.length === 0 && (
        <span className="text-xs text-text-muted">No events match these filters.</span>
      )}

      <div className="flex flex-col gap-1.5">
        {events.map((ev) => (
          <EventRow key={ev.id} event={ev} />
        ))}
      </div>

      {hasNextPage && (
        <button
          type="button"
          onClick={() => fetchNextPage()}
          disabled={isFetchingNextPage}
          className="mx-auto flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-xs font-medium text-text-secondary hover:border-text-muted disabled:opacity-50"
        >
          {isFetchingNextPage && <Loader2 className="size-3 animate-spin" />}
          Load more
        </button>
      )}
    </div>
  )
}

export default function Vehicle() {
  return (
    <div className="flex w-full flex-col gap-4 px-4 py-5 2xl:px-6">
      <h1 className="text-lg font-semibold text-text">Vehicle</h1>

      <StatusCard />

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <TrendsCard />
        <DTCTable />
      </div>

      <section className="flex flex-col gap-2">
        <h2 className="text-[12px] font-semibold uppercase tracking-wide text-text-muted">Events</h2>
        <EventsSection />
      </section>
    </div>
  )
}
