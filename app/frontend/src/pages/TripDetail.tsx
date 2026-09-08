import { useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  ArrowLeft,
  Camera,
  Clock,
  Film,
  Gauge,
  HardDrive,
  Loader2,
  Route,
} from 'lucide-react'
import { useNavigate, useParams } from 'react-router-dom'
import { ApiError, getTrip } from '../lib/api'
import { formatBytes, formatDateLong, formatDuration } from '../lib/format'
import type { TripSegment } from '../types'
import ClassBadges from '../components/ClassBadges'
import DualVideoPlayer, { type JumpSignal } from '../components/player/DualVideoPlayer'
import { ExportBinProvider } from '../components/player/ExportBinContext'

function StatItem({ icon: Icon, label, value }: { icon: typeof Route; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <Icon className="size-3.5 text-neutral-600" strokeWidth={2} />
      <span className="tabular-nums text-[13px] font-semibold text-neutral-200">{value}</span>
      <span className="text-[11px] text-neutral-600">{label}</span>
    </div>
  )
}

function SegmentTile({
  segment,
  selected,
  onSelect,
}: {
  segment: TripSegment
  selected: boolean
  onSelect: () => void
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={[
        'flex w-32 shrink-0 flex-col gap-1.5 rounded-md border px-2.5 py-2 text-left transition-colors',
        selected
          ? 'border-amber-500 bg-amber-500/10'
          : 'border-neutral-800 bg-neutral-900/40 hover:border-neutral-700',
      ].join(' ')}
    >
      <div className="flex h-14 items-center justify-center rounded bg-neutral-800/80 text-neutral-600">
        <Film className="size-4" strokeWidth={1.5} />
      </div>
      <div className="text-[11px] font-medium text-neutral-300 tabular-nums">
        seg {String(segment.seq).padStart(2, '0')}
      </div>
      <div className="text-[10px] text-neutral-600">{formatDuration(segment.duration_s)}</div>
      {segment.has_detections && segment.summary && (
        <ClassBadges summary={segment.summary} className="gap-x-1.5" />
      )}
    </button>
  )
}

export default function TripDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [jumpTo, setJumpTo] = useState<JumpSignal | null>(null)
  const [jumpSegId, setJumpSegId] = useState<number | null>(null)
  const jumpNonceRef = useRef(0)

  const { data: trip, isLoading, isError, error } = useQuery({
    queryKey: ['trip', id],
    queryFn: () => getTrip(id!),
    enabled: id != null,
  })

  if (isLoading) {
    return (
      <div className="flex h-svh items-center justify-center text-neutral-600">
        <Loader2 className="size-5 animate-spin" />
      </div>
    )
  }

  if (isError || !trip) {
    const notFound = error instanceof ApiError && error.status === 404
    return (
      <div className="flex h-svh flex-col items-center justify-center gap-3 text-neutral-500">
        <span className="text-sm">
          {notFound ? 'trip not found' : error instanceof Error ? error.message : 'couldn\'t load trip'}
        </span>
        <button onClick={() => navigate('/')} className="text-[13px] text-amber-400 hover:underline">
          back to browse
        </button>
      </div>
    )
  }

  const frontSegs = trip.segments.filter((s) => s.camera === 'front')
  const rearSegs = trip.segments.filter((s) => s.camera === 'rear')

  function selectSegment(seg: TripSegment) {
    jumpNonceRef.current += 1
    setJumpSegId(seg.id)
    setJumpTo({ startTime: seg.start_time, nonce: jumpNonceRef.current })
  }

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-6 px-8 py-8">
      <button
        onClick={() => navigate('/')}
        className="flex w-fit items-center gap-1.5 text-[12px] text-neutral-500 hover:text-neutral-200"
      >
        <ArrowLeft className="size-3.5" />
        Browse
      </button>

      <header className="flex flex-col gap-3">
        <div className="flex items-baseline justify-between">
          <h1 className="text-lg font-semibold text-neutral-100">{formatDateLong(trip.started_at)}</h1>
          <span className="font-mono text-[11px] text-neutral-600">{trip.trip_key}</span>
        </div>

        <div className="flex flex-wrap items-center gap-5 rounded-lg border border-neutral-800 bg-neutral-900/50 px-4 py-2.5">
          <StatItem icon={Clock} label="duration" value={formatDuration(trip.duration_s)} />
          <StatItem icon={Route} label="segments" value={String(trip.segment_count)} />
          <StatItem icon={HardDrive} label="stored" value={formatBytes(trip.bytes)} />
          {trip.max_speed_mps != null && (
            <StatItem icon={Gauge} label="max speed" value={`${Math.round(trip.max_speed_mps * 3.6)} km/h`} />
          )}
          {trip.pair_offset_s != null && (
            <StatItem icon={Camera} label="cam offset" value={`${trip.pair_offset_s.toFixed(2)}s`} />
          )}
        </div>

        <ClassBadges summary={trip.summary} />
      </header>

      {(trip.front || trip.rear) && (
        <ExportBinProvider>
          <section className="overflow-hidden rounded-lg border border-neutral-800">
            <DualVideoPlayer trip={trip} jumpTo={jumpTo} />
          </section>
        </ExportBinProvider>
      )}

      {frontSegs.length > 0 && (
        <section>
          <h2 className="mb-2 flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-neutral-500">
            <Camera className="size-3.5" /> Front
          </h2>
          <div className="flex gap-2 overflow-x-auto pb-2">
            {frontSegs.map((seg) => (
              <SegmentTile key={seg.id} segment={seg} selected={jumpSegId === seg.id} onSelect={() => selectSegment(seg)} />
            ))}
          </div>
        </section>
      )}

      {rearSegs.length > 0 && (
        <section>
          <h2 className="mb-2 flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-neutral-500">
            <Camera className="size-3.5" /> Rear
          </h2>
          <div className="flex gap-2 overflow-x-auto pb-2">
            {rearSegs.map((seg) => (
              <SegmentTile key={seg.id} segment={seg} selected={jumpSegId === seg.id} onSelect={() => selectSegment(seg)} />
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
