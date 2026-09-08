import { Camera, CameraOff, Film, Gauge, HardDrive, Route } from 'lucide-react'
import { useNavigate } from 'react-router-dom'
import type { TripCard } from '../types'
import { formatBytes, formatClockTime, formatDuration } from '../lib/format'
import ClassBadges from './ClassBadges'

interface Props {
  trip: TripCard
}

export default function TripCardRow({ trip }: Props) {
  const navigate = useNavigate()

  return (
    <button
      type="button"
      onClick={() => navigate(`/trips/${trip.id}`)}
      className="flex w-full items-center gap-4 rounded-lg border border-neutral-800 bg-neutral-900/40 px-4 py-3 text-left transition-colors hover:border-neutral-700 hover:bg-neutral-900"
    >
      <div className="flex size-14 shrink-0 items-center justify-center rounded-md bg-neutral-800/80 text-neutral-600">
        {trip.cover_thumb ? (
          <img src={trip.cover_thumb} alt="" className="size-14 rounded-md object-cover" />
        ) : (
          <Film className="size-5" strokeWidth={1.5} />
        )}
      </div>

      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <div className="flex items-baseline gap-2">
          <span className="text-[13px] font-semibold text-neutral-100 tabular-nums">
            {formatClockTime(trip.started_at)}
          </span>
          <span className="text-[11px] text-neutral-600">{formatDuration(trip.duration_s)}</span>
          <span className="ml-auto flex items-center gap-1.5 text-neutral-600">
            <span title={trip.front ? 'front camera' : 'no front camera'}>
              {trip.front ? (
                <Camera className="size-3.5 text-neutral-400" strokeWidth={2} />
              ) : (
                <CameraOff className="size-3.5" strokeWidth={2} />
              )}
            </span>
            <span title={trip.rear ? 'rear camera' : 'no rear camera'}>
              {trip.rear ? (
                <Camera className="size-3.5 text-neutral-400" strokeWidth={2} />
              ) : (
                <CameraOff className="size-3.5" strokeWidth={2} />
              )}
            </span>
          </span>
        </div>

        <ClassBadges summary={trip.summary} />

        <div className="flex items-center gap-3 text-[11px] text-neutral-600">
          <span className="flex items-center gap-1">
            <Route className="size-3" />
            {trip.segment_count} segments
          </span>
          <span className="flex items-center gap-1">
            <HardDrive className="size-3" />
            {formatBytes(trip.bytes)}
          </span>
          {trip.max_speed_mps != null && (
            <span className="flex items-center gap-1">
              <Gauge className="size-3" />
              {Math.round(trip.max_speed_mps * 3.6)} km/h max
            </span>
          )}
        </div>
      </div>
    </button>
  )
}
