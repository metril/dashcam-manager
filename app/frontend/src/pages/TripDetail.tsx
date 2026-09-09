import { useEffect, useMemo, useRef, useState } from 'react'
import type { CSSProperties, MouseEvent as ReactMouseEvent } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import {
  Archive,
  ArrowLeft,
  Camera,
  CheckCircle2,
  Clock,
  Film,
  Gauge,
  HardDrive,
  Loader2,
  Route,
  X,
} from 'lucide-react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { ApiError, getTrip } from '../lib/api'
import { formatBytes, formatDateLong, formatDuration } from '../lib/format'
import { getLastExportSettings, postExport } from '../lib/exportsApi'
import type { TripSegment } from '../types'
import ClassBadges from '../components/ClassBadges'
import DualVideoPlayer, { type JumpSignal } from '../components/player/DualVideoPlayer'
import { ExportBinProvider } from '../components/player/ExportBinContext'
import ExportPanel from '../components/ExportPanel'
import TripMap from '../components/TripMap'

function StatItem({ icon: Icon, label, value }: { icon: typeof Route; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <Icon className="size-3.5 text-neutral-400" strokeWidth={2} />
      <span className="tabular-nums text-[13px] font-semibold text-neutral-200">{value}</span>
      <span className="text-xs text-neutral-400">{label}</span>
    </div>
  )
}

// SpriteCue is one WebVTT thumbnails cue parsed from a segment's
// /sprite.vtt: a playback time range mapped to a pixel region (x,y,w,h) of
// its /sprite.jpg tile sheet.
interface SpriteCue {
  start: number
  end: number
  x: number
  y: number
  w: number
  h: number
}

// parseSpriteVTT parses the WebVTT thumbnails format media.SpriteVTT
// writes server-side: pairs of a "HH:MM:SS.mmm --> HH:MM:SS.mmm" cue-timing
// line followed by a "sprite.jpg#xywh=x,y,w,h" line.
function parseSpriteVTT(text: string): SpriteCue[] {
  const cues: SpriteCue[] = []
  const lines = text.split(/\r?\n/)
  const toSeconds = (h: string, m: string, s: string, ms: string) =>
    Number(h) * 3600 + Number(m) * 60 + Number(s) + Number(ms) / 1000

  for (let i = 0; i < lines.length; i++) {
    const timing = lines[i].match(/^(\d{2}):(\d{2}):(\d{2})\.(\d{3}) --> (\d{2}):(\d{2}):(\d{2})\.(\d{3})/)
    if (!timing) continue
    const xywh = (lines[i + 1] ?? '').match(/#xywh=(\d+),(\d+),(\d+),(\d+)/)
    if (!xywh) continue
    cues.push({
      start: toSeconds(timing[1], timing[2], timing[3], timing[4]),
      end: toSeconds(timing[5], timing[6], timing[7], timing[8]),
      x: Number(xywh[1]),
      y: Number(xywh[2]),
      w: Number(xywh[3]),
      h: Number(xywh[4]),
    })
  }
  return cues
}

// cueAt returns the cue covering time t (or the last cue, for t at/after
// the final cue's end — ffmpeg's sampled frames don't always add up to
// exactly the segment's probed duration).
function cueAt(cues: SpriteCue[], t: number): SpriteCue | undefined {
  return cues.find((c) => t >= c.start && t < c.end) ?? cues[cues.length - 1]
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
  const [thumbFailed, setThumbFailed] = useState(false)
  // null = not yet requested/loaded, [] = requested but unavailable
  // (sprite generation failed, or ffmpeg unavailable) — either way, no
  // scrub, just keep showing the static thumbnail.
  const [cues, setCues] = useState<SpriteCue[] | null>(null)
  const [hoverStyle, setHoverStyle] = useState<CSSProperties | null>(null)
  const cuesRequested = useRef(false)

  const sheetSize = useMemo(() => {
    if (!cues || cues.length === 0) return null
    return {
      w: Math.max(...cues.map((c) => c.x + c.w)),
      h: Math.max(...cues.map((c) => c.y + c.h)),
    }
  }, [cues])

  function loadSprite() {
    if (cuesRequested.current) return
    cuesRequested.current = true
    fetch(`/api/segments/${segment.id}/sprite.vtt`)
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error('sprite unavailable'))))
      .then((text) => setCues(parseSpriteVTT(text)))
      .catch(() => setCues([]))
  }

  function handleMouseMove(e: ReactMouseEvent<HTMLDivElement>) {
    if (!cues || cues.length === 0 || !sheetSize || !segment.duration_s) return
    const rect = e.currentTarget.getBoundingClientRect()
    if (rect.width === 0) return
    const frac = Math.min(Math.max((e.clientX - rect.left) / rect.width, 0), 0.999)
    const cue = cueAt(cues, frac * segment.duration_s)
    if (!cue) return
    const scale = rect.width / cue.w
    setHoverStyle({
      backgroundImage: `url(/api/segments/${segment.id}/sprite.jpg)`,
      backgroundRepeat: 'no-repeat',
      backgroundSize: `${sheetSize.w * scale}px ${sheetSize.h * scale}px`,
      backgroundPosition: `-${cue.x * scale}px -${cue.y * scale}px`,
    })
  }

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
      <div
        onMouseEnter={loadSprite}
        onMouseMove={handleMouseMove}
        onMouseLeave={() => setHoverStyle(null)}
        className="relative h-14 overflow-hidden rounded bg-neutral-800/80 text-neutral-400"
        style={hoverStyle ?? undefined}
      >
        {!hoverStyle &&
          (thumbFailed ? (
            <div className="flex h-full items-center justify-center">
              <Film className="size-4" strokeWidth={1.5} />
            </div>
          ) : (
            <img
              src={`/api/segments/${segment.id}/thumb.jpg`}
              alt=""
              loading="lazy"
              className="h-full w-full object-cover"
              onError={() => setThumbFailed(true)}
            />
          ))}
      </div>
      <div className="text-xs font-medium text-neutral-300 tabular-nums">
        seg {String(segment.seq).padStart(2, '0')}
      </div>
      <div className="text-xs text-neutral-400">{formatDuration(segment.duration_s)}</div>
      {segment.archived_at && (
        <div
          className="flex items-center gap-1 text-xs text-amber-500"
          title={
            segment.archived_to_share
              ? `Original deleted after verified ingest to ${segment.archived_to_share}`
              : 'Original deleted after verified ingest'
          }
        >
          <Archive className="size-3" strokeWidth={2} />
          Archived{segment.archived_to_share ? ` to ${segment.archived_to_share}` : ''}
        </div>
      )}
      {segment.has_detections && segment.summary && (
        <ClassBadges summary={segment.summary} className="gap-x-1.5" />
      )}
    </button>
  )
}

export default function TripDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const [jumpTo, setJumpTo] = useState<JumpSignal | null>(null)
  const [jumpSegId, setJumpSegId] = useState<number | null>(null)
  const [activeTimeMs, setActiveTimeMs] = useState<number | null>(null)
  const jumpNonceRef = useRef(0)
  const appliedDeepLink = useRef(false)

  const { data: trip, isLoading, isError, error } = useQuery({
    queryKey: ['trip', id],
    queryFn: () => getTrip(id!),
    enabled: id != null,
  })

  // One-click "Export this segment": posts a single-segment export job
  // straight away, reusing whatever camera/quality/target the user picked
  // last time, then surfaces a dismissible inline notice with a link to
  // /exports rather than a full export-bin round trip.
  const [quickExportNotice, setQuickExportNotice] = useState<{ ok: boolean; message: string } | null>(null)
  const quickExportMutation = useMutation({
    mutationFn: (segmentId: number) => {
      const settings = getLastExportSettings()
      return postExport({ items: [{ segment_id: segmentId }], camera: settings.camera, quality: settings.quality, target: settings.target, burn_in: false })
    },
    onSuccess: () => setQuickExportNotice({ ok: true, message: 'Export started.' }),
    onError: (err) =>
      setQuickExportNotice({ ok: false, message: err instanceof Error ? err.message : 'Export failed to start.' }),
  })

  // Deep-link support for Search results: /trips/{id}?seg={segId}&t={ptsOffset}
  // jumps to that segment's start plus the given offset (Search links here
  // with the segment's pts_offset of the matched detection).
  useEffect(() => {
    if (!trip || appliedDeepLink.current) return
    const segParam = searchParams.get('seg')
    if (!segParam) return
    appliedDeepLink.current = true
    const seg = trip.segments.find((s) => String(s.id) === segParam)
    if (!seg) return
    const tOffset = Number(searchParams.get('t') ?? 0)
    const target = new Date(Date.parse(seg.start_time) + (Number.isFinite(tOffset) ? tOffset : 0) * 1000)
    jumpNonceRef.current += 1
    setJumpSegId(seg.id)
    setJumpTo({ startTime: target.toISOString(), nonce: jumpNonceRef.current })
  }, [trip, searchParams])

  if (isLoading) {
    return (
      <div className="flex h-svh items-center justify-center text-neutral-400">
        <Loader2 className="size-5 animate-spin" />
      </div>
    )
  }

  if (isError || !trip) {
    const notFound = error instanceof ApiError && error.status === 404
    return (
      <div className="flex h-svh flex-col items-center justify-center gap-3 text-neutral-500">
        <span className="text-sm">
          {notFound ? 'Trip not found' : error instanceof Error ? error.message : "Couldn't load trip"}
        </span>
        <button onClick={() => navigate('/footage')} className="text-[13px] text-amber-400 hover:underline">
          Back to browse
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
        onClick={() => navigate('/footage')}
        className="flex w-fit items-center gap-1.5 text-[12px] text-neutral-500 hover:text-neutral-200"
      >
        <ArrowLeft className="size-3.5" />
        Browse
      </button>

      <header className="flex flex-col gap-3">
        <div className="flex items-baseline justify-between">
          <h1 className="text-lg font-semibold text-neutral-100">{formatDateLong(trip.started_at)}</h1>
          <span className="font-mono text-xs text-neutral-400">{trip.trip_key}</span>
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

      {quickExportNotice && (
        <div
          className={[
            'flex items-center gap-2 rounded-lg border px-3 py-2 text-[13px]',
            quickExportNotice.ok
              ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-300'
              : 'border-red-500/30 bg-red-500/10 text-red-300',
          ].join(' ')}
        >
          {quickExportNotice.ok ? <CheckCircle2 className="size-4 shrink-0" /> : null}
          <span className="flex-1">{quickExportNotice.message}</span>
          {quickExportNotice.ok && (
            <a href="/exports" className="font-medium hover:underline">
              View in Exports
            </a>
          )}
          <button
            type="button"
            onClick={() => setQuickExportNotice(null)}
            className="text-current opacity-70 hover:opacity-100"
          >
            <X className="size-3.5" />
          </button>
        </div>
      )}

      {(trip.front || trip.rear) && (
        <ExportBinProvider>
          <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_320px] xl:items-start">
            <section className="overflow-hidden rounded-lg border border-neutral-800">
              <DualVideoPlayer
                trip={trip}
                jumpTo={jumpTo}
                onActiveTimeChange={setActiveTimeMs}
                onExportSegment={(segmentId) => {
                  setQuickExportNotice(null)
                  quickExportMutation.mutate(segmentId)
                }}
              />
            </section>
            <ExportPanel trip={trip} />
          </div>
        </ExportBinProvider>
      )}

      <TripMap
        tripId={trip.id}
        activeTimeMs={activeTimeMs}
        onSeek={(isoTime) => {
          jumpNonceRef.current += 1
          setJumpSegId(null)
          setJumpTo({ startTime: isoTime, nonce: jumpNonceRef.current })
        }}
      />

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
