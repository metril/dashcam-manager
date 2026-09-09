// TripMap: a collapsible Leaflet map showing a trip's GPS track (GET
// /api/gps/track), colored by speed, with a marker that follows playback
// time (when the host page wires activeTimeMs through) and click-on-track
// seeking (via onSeek, given the wall-clock ISO timestamp of the nearest
// point — the same shape DualVideoPlayer's JumpSignal.startTime expects).
// Self-contained: the only integration point a host page needs is
// `<TripMap tripId={trip.id} />`.
import { useEffect, useMemo, useState } from 'react'
import { CircleMarker, MapContainer, Polyline, TileLayer, useMap } from 'react-leaflet'
import { useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronRight, Map as MapIcon } from 'lucide-react'
import 'leaflet/dist/leaflet.css'
import { getGPSTrack } from '../lib/api'
import type { GPSTrackPoint } from '../types'

// speedColor buckets a point's speed (m/s) into a red(slow)->green(fast)-
// ish palette that reads clearly against OSM tiles: stopped/crawling
// (<3 m/s, ~11 km/h) in red, city speeds in amber, open-road in green.
function speedColor(speedMps: number | null): string {
  if (speedMps == null) return '#6b7280' // neutral gray for unknown
  if (speedMps < 3) return '#ef4444'
  if (speedMps < 15) return '#f59e0b'
  return '#22c55e'
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
}

export default function TripMap({ tripId, activeTimeMs, onSeek, defaultOpen = false }: TripMapProps) {
  const [open, setOpen] = useState(defaultOpen)

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
  }, [points])

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
    <section className="rounded-lg border border-neutral-800">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-2 px-4 py-2.5 text-left text-[12px] font-semibold uppercase tracking-wide text-neutral-500 hover:text-neutral-300"
      >
        {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
        <MapIcon className="size-3.5" /> Map
      </button>
      {open && (
        <div className="h-80 border-t border-neutral-700">
          {isLoading && <div className="flex h-full items-center justify-center text-neutral-400 text-[12px]">Loading track…</div>}
          {isError && <div className="flex h-full items-center justify-center text-neutral-400 text-[12px]">Couldn't load GPS track</div>}
          {!isLoading && !isError && points.length === 0 && (
            <div className="flex h-full items-center justify-center text-neutral-400 text-[12px]">No GPS data for this trip</div>
          )}
          {!isLoading && !isError && points.length > 0 && (
            <MapContainer
              center={[points[0].lat, points[0].lon]}
              zoom={15}
              scrollWheelZoom
              className="h-full w-full"
              style={{ background: '#171717' }}
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
              {activePoint && (
                <CircleMarker
                  center={[activePoint.lat, activePoint.lon]}
                  radius={7}
                  pathOptions={{ color: '#f59e0b', fillColor: '#f59e0b', fillOpacity: 1, weight: 2 }}
                />
              )}
            </MapContainer>
          )}
        </div>
      )}
    </section>
  )
}
