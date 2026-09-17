// TripMap: a collapsible Leaflet map showing a trip's GPS track (GET
// /api/gps/track), colored by speed, with a marker that follows playback
// time (when the host page wires activeTimeMs through) and click-on-track
// seeking (via onSeek, given the wall-clock ISO timestamp of the nearest
// point — the same shape DualVideoPlayer's JumpSignal.startTime expects).
// Self-contained: the only integration point a host page needs is
// `<TripMap tripId={trip.id} />`.
import { useEffect, useMemo, useState } from 'react'
import { CircleMarker, MapContainer, Polyline, Popup, TileLayer, useMap } from 'react-leaflet'
import { useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronRight, Map as MapIcon } from 'lucide-react'
import 'leaflet/dist/leaflet.css'
import { getGPSTrack } from '../lib/api'
import { THEME_CHANGE_EVENT } from '../lib/theme'
import { formatSpeed, useUnits } from '../lib/units'
import { vehicleEventMeta } from '../lib/vehicleEvents'
import type { GPSTrackPoint, VehicleEvent } from '../types'

// resolveToken reads a CSS custom property's current computed value off
// document.documentElement, the same approach DetectionOverlay uses for its
// canvas — Leaflet's pathOptions/style props take plain color strings, not
// CSS classes, so they can't pick up a theme change via a class name the
// way DOM elements do; the caller must re-read after THEME_CHANGE_EVENT.
function resolveToken(varName: string, fallback: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(varName).trim()
  return v || fallback
}

// speedColor buckets a point's speed (m/s) into a slow/medium/fast palette
// using the danger/warn/success tokens, so it tracks the active theme
// instead of a fixed hex: stopped/crawling (<3 m/s, ~11 km/h) is danger,
// city speeds are warn, open-road is success.
function speedColor(speedMps: number | null): string {
  if (speedMps == null) return resolveToken('--color-text-muted', '#6b7280')
  if (speedMps < 3) return resolveToken('--color-danger', '#ef4444')
  if (speedMps < 15) return resolveToken('--color-warn', '#f59e0b')
  return resolveToken('--color-success', '#22c55e')
}

interface TrackSegment {
  positions: [[number, number], [number, number]]
  color: string
  midT: string
}

function FitBounds({ points }: { points: GPSTrackPoint[] }) {
  const map = useMap()
  useEffect(() => {
    if (points.length === 0) return
    const lats = points.map((p) => p.lat)
    const lons = points.map((p) => p.lon)
    map.fitBounds(
      [
        [Math.min(...lats), Math.min(...lons)],
        [Math.max(...lats), Math.max(...lons)],
      ],
      { padding: [24, 24] },
    )
    // map is stable for the lifetime of this MapContainer; only points
    // (a new track after switching trips) should re-trigger the fit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [points])
  return null
}

export interface TripMapProps {
  tripId: number
  /** Wall-clock ms of the point playback is currently at, if the host page tracks it. */
  activeTimeMs?: number | null
  /** Called with the wall-clock ISO timestamp of the point nearest a track click. */
  onSeek?: (isoTime: string) => void
  defaultOpen?: boolean
  /** OBD vehicle events with a lat/lon (Task 8) — rendered as CircleMarkers on top of the track, click seeks via the same onSeek path (event's wall_time). */
  events?: VehicleEvent[]
}

export default function TripMap({ tripId, activeTimeMs, onSeek, defaultOpen = false, events }: TripMapProps) {
  const [open, setOpen] = useState(defaultOpen)
  const units = useUnits()
  // Bumped on THEME_CHANGE_EVENT so the memoized segment/marker colors
  // below (resolved once via getComputedStyle, not live CSS) recompute
  // when the user switches theme instead of keeping stale colors.
  const [themeTick, setThemeTick] = useState(0)

  useEffect(() => {
    const onThemeChange = () => setThemeTick((t) => t + 1)
    window.addEventListener(THEME_CHANGE_EVENT, onThemeChange)
    return () => window.removeEventListener(THEME_CHANGE_EVENT, onThemeChange)
  }, [])

  const { data, isLoading, isError } = useQuery({
    queryKey: ['gps-track', tripId],
    queryFn: () => getGPSTrack(tripId),
    enabled: open,
  })

  const points = data?.properties.points ?? []

  const segments = useMemo<TrackSegment[]>(() => {
    const out: TrackSegment[] = []
    for (let i = 0; i < points.length - 1; i++) {
      const a = points[i]
      const b = points[i + 1]
      out.push({
        positions: [
          [a.lat, a.lon],
          [b.lat, b.lon],
        ],
        color: speedColor(a.speed_mps),
        midT: a.t,
      })
    }
    return out
    // eslint-disable-next-line react-hooks/exhaustive-deps -- themeTick only forces a recompute of speedColor's resolved token values
  }, [points, themeTick])

  const activePoint = useMemo(() => {
    if (activeTimeMs == null || points.length === 0) return null
    let nearest = points[0]
    let best = Infinity
    for (const p of points) {
      const d = Math.abs(Date.parse(p.t) - activeTimeMs)
      if (d < best) {
        best = d
        nearest = p
      }
    }
    return nearest
  }, [activeTimeMs, points])

  return (
    <section className="rounded-lg border border-border">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-2 px-4 py-2.5 text-left text-[12px] font-semibold uppercase tracking-wide text-text-muted hover:text-text-secondary"
      >
        {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
        <MapIcon className="size-3.5" /> Map
      </button>
      {open && (
        <div className="h-80 border-t border-border">
          {isLoading && <div className="flex h-full items-center justify-center text-text-muted text-[12px]">Loading track…</div>}
          {isError && <div className="flex h-full items-center justify-center text-text-muted text-[12px]">Couldn't load GPS track</div>}
          {!isLoading && !isError && points.length === 0 && (
            <div className="flex h-full items-center justify-center text-text-muted text-[12px]">No GPS data for this trip</div>
          )}
          {!isLoading && !isError && points.length > 0 && (
            <MapContainer
              center={[points[0].lat, points[0].lon]}
              zoom={15}
              scrollWheelZoom
              className="h-full w-full"
              style={{ background: 'var(--color-surface)' }}
            >
              <TileLayer
                attribution='&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
                url="https://tile.openstreetmap.org/{z}/{x}/{y}.png"
              />
              <FitBounds points={points} />
              {segments.map((seg, i) => (
                <Polyline
                  key={i}
                  positions={seg.positions}
                  pathOptions={{ color: seg.color, weight: 4, opacity: 0.85 }}
                  eventHandlers={{ click: () => onSeek?.(seg.midT) }}
                />
              ))}
              {events
                ?.filter((ev) => ev.lat != null && ev.lon != null)
                .map((ev) => {
                  const meta = vehicleEventMeta(ev.event_type)
                  const color = resolveToken(meta.color, '#f59e0b')
                  return (
                    <CircleMarker
                      key={ev.id}
                      center={[ev.lat!, ev.lon!]}
                      radius={6}
                      pathOptions={{ color, fillColor: color, fillOpacity: 0.85, weight: 1.5 }}
                      eventHandlers={{ click: () => ev.wall_time && onSeek?.(ev.wall_time) }}
                    >
                      <Popup>
                        <span className="text-xs">
                          {meta.label}
                          {ev.speed_kph != null ? ` · ${formatSpeed(ev.speed_kph, units)}` : ''}
                        </span>
                      </Popup>
                    </CircleMarker>
                  )
                })}
              {activePoint && (
                <CircleMarker
                  center={[activePoint.lat, activePoint.lon]}
                  radius={7}
                  pathOptions={{
                    color: resolveToken('--color-accent', '#f59e0b'),
                    fillColor: resolveToken('--color-accent', '#f59e0b'),
                    fillOpacity: 1,
                    weight: 2,
                  }}
                />
              )}
            </MapContainer>
          )}
        </div>
      )}
    </section>
  )
}
