import { useMemo, useRef, useState } from 'react'
import type { CSSProperties, MouseEvent as ReactMouseEvent } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import {
  Archive,
  ArrowLeft,
  Battery,
  Bike,
  Bus,
  Camera,
  Car,
  CheckCircle2,
  Clock,
  Film,
  Fuel,
  Gauge,
  HardDrive,
  IdCard,
  Loader2,
  Motorbike,
  Route,
  Thermometer,
  Truck,
  TriangleAlert,
  User,
  X,
} from 'lucide-react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { ApiError, getSegmentDetections, getTrip, type DetTrack } from '../lib/api'
import { formatBytes, formatDateLong, formatDuration, formatTripKey } from '../lib/format'
import { getLastExportSettings, postExport } from '../lib/exportsApi'
import { getTripVehicle } from '../lib/vehicleApi'
import { vehicleEventMeta } from '../lib/vehicleEvents'
import { useDetectionFilter } from '../hooks/useDetectionFilter'
import type { TripDetail as TripDetailType, TripSegment } from '../types'
import ClassBadges from '../components/ClassBadges'
import CopiesBadge from '../components/CopiesBadge'
import DualVideoPlayer, { type JumpSignal } from '../components/player/DualVideoPlayer'
import { type EventMarker } from '../components/player/TimelineStrip'
import { ExportBinProvider, useExportBin } from '../components/player/ExportBinContext'
import ExportPanel from '../components/ExportPanel'
import Tooltip from '../components/Tooltip'
import TripMap from '../components/TripMap'

// TRACK_ICON/TRACK_COLOR mirror ClassBadges/DetectionOverlay's class ->
// icon/color mapping, plus a plate/vehicle entry for det_tracks rows that
// have no object class (kind 'plate') or a vehicle-classifier type instead
// of a detector class (kind 'vehicle').
const TRACK_ICON: Record<string, typeof Car> = {
  car: Car,
  truck: Truck,
  bus: Bus,
  person: User,
  bicycle: Bike,
  motorcycle: Motorbike,
  plate: IdCard,
  vehicle: Car,
}
const TRACK_COLOR: Record<string, string> = {
  car: 'text-det-vehicle',
  truck: 'text-det-vehicle',
  bus: 'text-det-vehicle',
  motorcycle: 'text-det-vehicle',
  bicycle: 'text-det-vehicle',
  person: 'text-det-person',
  plate: 'text-det-plate',
  vehicle: 'text-det-vehicle',
}

// trackPasses applies the live DetectionFilter to one det_tracks row: kind
// gate (objects/plates/vehicles), confidence floor, and (for object-kind
// tracks only) the class whitelist.
function trackPasses(track: DetTrack, filter: ReturnType<typeof useDetectionFilter>[0]): boolean {
  if (track.kind === 'object' && !filter.objects) return false
  if (track.kind === 'plate' && !filter.plates) return false
  if (track.kind === 'vehicle' && !filter.vehicles) return false
  if ((track.best_conf ?? 0) < filter.min_conf) return false
  if (track.kind === 'object' && filter.classes.length > 0 && !filter.classes.includes(track.class)) return false
  return true
}

// DetectionsList: the trip player's side-panel track list (Task 6) — every
// det_tracks row for the currently-visible front segment, grouped by
// object/plate/vehicle and filtered live by the same DetectionFilter the
// overlay/export use. Clicking a row seeks the player to that track's
// first appearance (first_pts, converted to an absolute wall-clock jump
// signal the same way Search's deep-link does). Purely presentational —
// fix round 1 lifted the getSegmentDetections query up to TripPlayerPanel
// (the ExportBinProvider-scoped component below) so the same query result
// can also drive whether the side column has anything to show at all,
// without a second network round trip.
function DetectionsList({
  segment,
  tracks,
  isLoading,
  onSeek,
}: {
  segment: TripSegment | undefined
  tracks: DetTrack[]
  isLoading: boolean
  onSeek: (isoTime: string) => void
}) {
  if (!segment || !segment.has_detections) return null

  return (
    <div className="flex flex-col gap-2 rounded-lg border border-border bg-surface/60 p-3">
      <h3 className="text-[12px] font-semibold uppercase tracking-wide text-text-muted">
        Detections {tracks.length > 0 ? `(${tracks.length})` : ''}
      </h3>
      {isLoading && <span className="text-xs text-text-muted">Loading…</span>}
      {!isLoading && tracks.length === 0 && (
        <span className="text-xs text-text-muted">Nothing matches the current filter in this segment.</span>
      )}
      {tracks.length > 0 && (
        <ul className="flex max-h-64 flex-col gap-1 overflow-y-auto">
          {tracks.map((t) => {
            const key = t.kind === 'plate' ? 'plate' : t.kind === 'vehicle' ? 'vehicle' : t.class
            const Icon = TRACK_ICON[key] ?? Car
            const color = TRACK_COLOR[key] ?? 'text-det-other'
            return (
              <li key={t.id}>
                <button
                  type="button"
                  onClick={() => onSeek(new Date(Date.parse(segment.start_time) + t.first_pts * 1000).toISOString())}
                  className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[12px] hover:bg-surface-2"
                >
                  <Icon className={`size-3.5 shrink-0 ${color}`} strokeWidth={2} />
                  <span className="min-w-0 flex-1 truncate text-text-secondary">
                    {t.best_text || t.class || t.kind}
                  </span>
                  {t.best_conf != null && (
                    <span className="shrink-0 tabular-nums text-text-muted">{Math.round(t.best_conf * 100)}%</span>
                  )}
                  <span className="shrink-0 tabular-nums text-text-muted">
                    {formatDuration(t.first_pts)}&ndash;{formatDuration(t.last_pts)}
                  </span>
                </button>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}

function StatItem({ icon: Icon, label, value }: { icon: typeof Route; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <Icon className="size-3.5 text-text-muted" strokeWidth={2} />
      <span className="tabular-nums text-[13px] font-semibold text-text">{value}</span>
      <span className="text-xs text-text-muted">{label}</span>
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
          ? 'border-accent bg-accent/10'
          : 'border-border bg-surface/40 hover:border-text-muted',
      ].join(' ')}
    >
      <div
        onMouseEnter={loadSprite}
        onMouseMove={handleMouseMove}
        onMouseLeave={() => setHoverStyle(null)}
        className="relative h-14 overflow-hidden rounded bg-surface-2/80 text-text-muted"
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
      <div className="text-xs font-medium text-text-secondary tabular-nums">
        Segment {String(segment.seq).padStart(2, '0')}
      </div>
      <div className="text-xs text-text-muted">{formatDuration(segment.duration_s)}</div>
      {segment.archived_at && (
        <div
          className="flex items-center gap-1 text-xs text-accent"
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
      {segment.unreadable_reason && (
        <Tooltip content={segment.unreadable_reason} side="bottom">
          <div className="flex w-fit items-center gap-1 rounded-sm bg-danger/15 px-1 py-0.5 text-xs font-medium text-danger">
            <TriangleAlert className="size-3" strokeWidth={2} />
            Unreadable
          </div>
        </Tooltip>
      )}
      {segment.has_detections && segment.summary && (
        <ClassBadges summary={segment.summary} className="gap-x-1.5" />
      )}
      {/* mt-auto pins the copies badge to the tile's bottom edge so it lines up
          across tiles even when the class-badge row above wraps to a different
          number of lines (tiles stretch to equal height in the flex row). The
          wrapper is needed because Tooltip adds its own inline-flex span. */}
      <div className="mt-auto">
        <CopiesBadge copies={segment.copies} onSsd={segment.on_ssd} />
      </div>
    </button>
  )
}

// TripPlayerPanel: the player + its side column, rendered inside
// ExportBinProvider so it can read the export bin's live state. Fix round
// 1 — the side column used to always reserve `360px`/`420px` of grid
// track even when both ExportPanel and DetectionsList had nothing to
// render (an empty bin + a segment with no detections), which visibly
// starved the video player of width (it topped out under 900px on a
// 1920px-wide screen). The grid template now switches to a single column
// (player at full width) whenever there's nothing for the side column to
// show, and back to the two-column layout the moment there is.
function TripPlayerPanel({
  trip,
  jumpTo,
  onActiveTimeChange,
  onActiveSegmentChange,
  onExportSegment,
  activeSegment,
  tracks,
  tracksLoading,
  onSeekTrack,
  eventMarkers,
}: {
  trip: TripDetailType
  jumpTo?: JumpSignal | null
  onActiveTimeChange: (absMs: number) => void
  onActiveSegmentChange: (segmentId: number | null) => void
  onExportSegment: (segmentId: number) => void
  activeSegment: TripSegment | undefined
  tracks: DetTrack[]
  tracksLoading: boolean
  onSeekTrack: (isoTime: string) => void
  eventMarkers?: EventMarker[]
}) {
  const exportBin = useExportBin()
  const binHasContent = exportBin.state.ranges.length > 0 || exportBin.state.segments.length > 0
  // exportPanelVisible additionally covers the case ExportPanel itself
  // knows about but this component can't see directly: a job it started
  // is still showing a live/finished status notice even after the bin
  // that triggered it was cleared. Defaults to binHasContent so the panel
  // (and thus the two-column layout) doesn't flash away for one render
  // before ExportPanel's own effect reports in.
  const [exportPanelVisible, setExportPanelVisible] = useState(binHasContent)
  // tracksLoading counts as "has content" so the column doesn't
  // pop collapsed -> expanded -> collapsed while the detections query for
  // a newly-active segment is in flight; it settles once the query
  // resolves (to either some tracks, or genuinely none).
  const hasSideContent = exportPanelVisible || binHasContent || tracks.length > 0 || tracksLoading

  return (
    <div
      className={
        hasSideContent
          ? 'grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_360px] xl:items-start 2xl:grid-cols-[minmax(0,1fr)_420px]'
          : 'grid grid-cols-1 gap-4'
      }
    >
      <section className="overflow-hidden rounded-lg border border-border">
        <DualVideoPlayer
          trip={trip}
          jumpTo={jumpTo}
          onActiveTimeChange={onActiveTimeChange}
          onActiveSegmentChange={onActiveSegmentChange}
          onExportSegment={onExportSegment}
          eventMarkers={eventMarkers}
        />
      </section>
      {hasSideContent && (
        <div className="flex flex-col gap-4">
          <ExportPanel trip={trip} onVisibilityChange={setExportPanelVisible} />
          <DetectionsList segment={activeSegment} tracks={tracks} isLoading={tracksLoading} onSeek={onSeekTrack} />
        </div>
      )}
    </div>
  )
}

export default function TripDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const [jumpTo, setJumpTo] = useState<JumpSignal | null>(null)
  const [jumpSegId, setJumpSegId] = useState<number | null>(null)
  const [activeTimeMs, setActiveTimeMs] = useState<number | null>(null)
  const [activeFrontSegmentId, setActiveFrontSegmentId] = useState<number | null>(null)
  const [detectionFilter] = useDetectionFilter()
  const jumpNonceRef = useRef(0)
  // deepLinkApplied latches once the ?seg=/?t=/?at= deep link (below) has
  // been applied, so it only ever fires once per mount — a state flag,
  // not a ref, so the one-shot jump computed once `trip` loads can be
  // set directly during render (see the `if (trip && !deepLinkApplied)`
  // block below) rather than from a useEffect, which a synchronous
  // setState inside would flag (react(set-state-in-effect), fix round 3
  // item 8): this is React's own sanctioned "adjust state during
  // render" pattern for reacting to newly-available data.
  const [deepLinkApplied, setDeepLinkApplied] = useState(false)

  const { data: trip, isLoading, isError, error } = useQuery({
    queryKey: ['trip', id],
    queryFn: () => getTrip(id!),
    enabled: id != null,
  })

  // Task 8: OBD vehicle telemetry for the whole trip — 404s (no linked
  // recorder data at all) are treated as "no telemetry", same convention
  // as vehicleApi's other 404-tolerant callers.
  const { data: vehicle } = useQuery({
    queryKey: ['trip-vehicle', id],
    queryFn: () => getTripVehicle(id!),
    enabled: id != null,
    retry: (failureCount, err) => !(err instanceof ApiError && err.status === 404) && failureCount < 2,
  })

  const eventMarkers = useMemo<EventMarker[] | undefined>(() => {
    if (!vehicle?.events?.length) return undefined
    return vehicle.events.map((ev) => ({ offsetS: ev.offset_s, type: ev.event_type, id: ev.id }))
  }, [vehicle])

  const activeSegment = trip?.segments.find((s) => s.id === activeFrontSegmentId)

  // Lifted up from DetectionsList (fix round 1) so the same query result
  // can also decide whether TripPlayerPanel's side column has anything to
  // show — a second, separate query for the same segment would just be a
  // redundant network round trip (staleTime: Infinity means TanStack Query
  // would dedupe the cache entry anyway, but the loading/enabled state
  // needs to live at this level regardless).
  const { data: activeSegmentDetections, isLoading: tracksLoading } = useQuery({
    queryKey: ['segment-detections', activeSegment?.id ?? null],
    queryFn: () => getSegmentDetections(activeSegment!.id),
    enabled: activeSegment != null && activeSegment.has_detections,
    staleTime: Infinity,
  })
  const activeTracks = useMemo(
    () =>
      (activeSegmentDetections?.tracks ?? [])
        .filter((t) => trackPasses(t, detectionFilter))
        .sort((a, b) => a.first_pts - b.first_pts),
    [activeSegmentDetections, detectionFilter],
  )

  // One-click "Export this segment": posts a single-segment export job
  // straight away, reusing whatever camera/quality/target the user picked
  // last time, then surfaces a dismissible inline notice with a link to
  // /exports rather than a full export-bin round trip.
  const [quickExportNotice, setQuickExportNotice] = useState<{ ok: boolean; message: string } | null>(null)
  const quickExportMutation = useMutation({
    mutationFn: (segmentId: number) => {
      const settings = getLastExportSettings()
      return postExport({
        items: [{ segment_id: segmentId }],
        camera: settings.camera,
        quality: settings.quality,
        target: settings.target,
        container: settings.container,
        codec: settings.codec,
        burn_in: false,
      })
    },
    onSuccess: () => setQuickExportNotice({ ok: true, message: 'Export started.' }),
    onError: (err) =>
      setQuickExportNotice({ ok: false, message: err instanceof Error ? err.message : 'Export failed to start.' }),
  })

  // Deep-link support for Search results: /trips/{id}?seg={segId}&t={ptsOffset}
  // jumps to that segment's start plus the given offset (Search links here
  // with the segment's pts_offset of the matched detection).
  //
  // A bare `?t={tripOffsetS}` (no `seg`) is a trip-relative-offset deep
  // link (offset from trip.started_at) — kept for any other caller that
  // already knows a trip-relative second, but NOT used by the Vehicle
  // page's event rows (see `?at=` below): the global GET /api/vehicle/events
  // listing's offset_s is segment-relative for a segment-linked event, only
  // GET /api/trips/{id}/vehicle computes a trip-relative offset (see
  // types.ts's VehicleEvent doc comment), so a bare trip-second isn't safe
  // to derive from that endpoint's rows.
  //
  // `?at={isoWallTime}` (Task 8 fix round 1) jumps straight to an absolute
  // wall-clock instant — used by the Vehicle page's event rows, which only
  // have each event's own wall_time, not a trip-relative offset.
  if (trip && !deepLinkApplied) {
    const segParam = searchParams.get('seg')
    const tParam = searchParams.get('t')
    const atParam = searchParams.get('at')
    if (segParam || tParam || atParam) {
      setDeepLinkApplied(true)

      // The deep-link jump's own nonce is a fixed sentinel (-1), not
      // jumpNonceRef (mutating/reading a ref during render is itself
      // lint-flagged, react(refs) — refs are for effects/handlers, not
      // render): it only ever fires once per mount (deepLinkApplied
      // above), so it can never collide with jumpNonceRef's own
      // increasing sequence (which starts at 1, from event handlers).
      if (segParam) {
        const seg = trip.segments.find((s) => String(s.id) === segParam)
        if (seg) {
          const tOffset = Number(tParam ?? 0)
          const target = new Date(Date.parse(seg.start_time) + (Number.isFinite(tOffset) ? tOffset : 0) * 1000)
          setJumpSegId(seg.id)
          setJumpTo({ startTime: target.toISOString(), nonce: -1 })
        }
      } else if (atParam) {
        const atMs = Date.parse(atParam)
        if (!Number.isNaN(atMs)) {
          setJumpSegId(null)
          setJumpTo({ startTime: new Date(atMs).toISOString(), nonce: -1 })
        }
      } else {
        const tripOffsetS = Number(tParam)
        if (Number.isFinite(tripOffsetS)) {
          const target = new Date(Date.parse(trip.started_at) + tripOffsetS * 1000)
          setJumpSegId(null)
          setJumpTo({ startTime: target.toISOString(), nonce: -1 })
        }
      }
    }
  }

  if (isLoading) {
    return (
      <div className="flex h-svh items-center justify-center text-text-muted">
        <Loader2 className="size-5 animate-spin" />
      </div>
    )
  }

  if (isError || !trip) {
    const notFound = error instanceof ApiError && error.status === 404
    return (
      <div className="flex h-svh flex-col items-center justify-center gap-3 text-text-muted">
        <span className="text-sm">
          {notFound ? 'Trip not found' : error instanceof Error ? error.message : "Couldn't load trip"}
        </span>
        <button onClick={() => navigate('/footage')} className="text-[13px] text-accent hover:underline">
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
    <div className="flex w-full flex-col gap-4 px-4 py-5 2xl:px-6">
      <button
        onClick={() => navigate('/footage')}
        className="flex w-fit items-center gap-1.5 text-[12px] text-text-muted hover:text-text"
      >
        <ArrowLeft className="size-3.5" />
        Browse
      </button>

      <header className="flex flex-col gap-3">
        <div className="flex items-baseline justify-between">
          <h1 className="text-lg font-semibold text-text">{formatDateLong(trip.started_at)}</h1>
          <Tooltip content={trip.trip_key}>
            <span className="font-mono text-xs text-text-muted">{formatTripKey(trip.trip_key)}</span>
          </Tooltip>
        </div>

        <div className="flex flex-wrap items-center gap-5 rounded-lg border border-border bg-surface/50 px-4 py-2.5">
          <StatItem icon={Clock} label="Duration" value={formatDuration(trip.duration_s)} />
          <StatItem icon={Route} label="Segments" value={String(trip.segment_count)} />
          <StatItem icon={HardDrive} label="Stored" value={formatBytes(trip.bytes)} />
          {trip.max_speed_mps != null && (
            <StatItem icon={Gauge} label="Max speed" value={`${Math.round(trip.max_speed_mps * 3.6)} km/h`} />
          )}
          {trip.pair_offset_s != null && (
            <StatItem icon={Camera} label="Camera offset" value={`${trip.pair_offset_s.toFixed(2)}s`} />
          )}
        </div>

        <ClassBadges summary={trip.summary} />

        {vehicle && (
          <div className="flex flex-col gap-2 rounded-lg border border-border bg-surface/50 px-4 py-2.5">
            <div className="flex flex-wrap items-center gap-5">
              {vehicle.totals.distance_m != null && (
                <StatItem icon={Route} label="OBD distance" value={`${(vehicle.totals.distance_m / 1000).toFixed(1)} km`} />
              )}
              {vehicle.totals.fuel_l != null && (
                <StatItem icon={Fuel} label="Fuel" value={`${vehicle.totals.fuel_l.toFixed(1)} L`} />
              )}
              {vehicle.totals.max_speed_kph != null && (
                <StatItem icon={Gauge} label="Max speed" value={`${Math.round(vehicle.totals.max_speed_kph)} km/h`} />
              )}
              {(() => {
                // A single recorder trip's own avg_speed_kph is exact;
                // with several overlapping recorder trips there's no
                // single "average" to report, so fall back to
                // totals.distance_m / this recording trip's duration —
                // a trip-wide mean speed, not a mean of means.
                if (vehicle.recorder_trips.length === 1) {
                  const avg = vehicle.recorder_trips[0].avg_speed_kph
                  if (avg == null) return null
                  return <StatItem icon={Gauge} label="Avg speed" value={`${Math.round(avg)} km/h`} />
                }
                if (vehicle.recorder_trips.length > 1 && vehicle.totals.distance_m != null && trip.duration_s) {
                  const avg = vehicle.totals.distance_m / 1000 / (trip.duration_s / 3600)
                  return <StatItem icon={Gauge} label="Avg speed (trip)" value={`${Math.round(avg)} km/h`} />
                }
                return null
              })()}
              {(vehicle.totals.batt_min_v != null || vehicle.totals.batt_max_v != null) && (
                <StatItem
                  icon={Battery}
                  label="Battery"
                  value={`${vehicle.totals.batt_min_v?.toFixed(1) ?? '—'}–${vehicle.totals.batt_max_v?.toFixed(1) ?? '—'} V`}
                />
              )}
              {vehicle.totals.coolant_max_c != null && (
                <StatItem icon={Thermometer} label="Coolant max" value={`${Math.round(vehicle.totals.coolant_max_c)}°C`} />
              )}
            </div>
            {(vehicle.totals.mil || vehicle.totals.dtcs.length > 0) && (
              <div className="flex flex-wrap items-center gap-1.5">
                {vehicle.totals.mil && (
                  <span className="flex items-center gap-1 rounded-sm bg-danger/15 px-1.5 py-0.5 text-xs font-medium text-danger">
                    <TriangleAlert className="size-3" strokeWidth={2} /> MIL on
                  </span>
                )}
                {vehicle.totals.dtcs.map((code) => (
                  <span key={code} className="rounded-sm bg-danger/15 px-1.5 py-0.5 font-mono text-xs font-medium text-danger">
                    {code}
                  </span>
                ))}
              </div>
            )}
            {Object.keys(vehicle.event_counts).length > 0 && (
              <div className="flex flex-wrap items-center gap-3">
                {Object.entries(vehicle.event_counts).map(([type, count]) => {
                  const meta = vehicleEventMeta(type)
                  const Icon = meta.icon
                  return (
                    <span key={type} className="flex items-center gap-1 text-xs text-text-muted">
                      <Icon className="size-3" style={{ color: `var(${meta.color})` }} strokeWidth={2} />
                      {count} {meta.label.toLowerCase()}
                    </span>
                  )
                })}
              </div>
            )}
          </div>
        )}
      </header>

      {quickExportNotice && (
        <div
          className={[
            'flex items-center gap-2 rounded-lg border px-3 py-2 text-[13px]',
            quickExportNotice.ok
              ? 'border-success/30 bg-success/10 text-success'
              : 'border-danger/30 bg-danger/10 text-danger',
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
          <TripPlayerPanel
            trip={trip}
            jumpTo={jumpTo}
            onActiveTimeChange={setActiveTimeMs}
            onActiveSegmentChange={setActiveFrontSegmentId}
            onExportSegment={(segmentId) => {
              setQuickExportNotice(null)
              quickExportMutation.mutate(segmentId)
            }}
            activeSegment={activeSegment}
            tracks={activeTracks}
            tracksLoading={tracksLoading}
            onSeekTrack={(isoTime) => {
              jumpNonceRef.current += 1
              setJumpSegId(activeFrontSegmentId)
              setJumpTo({ startTime: isoTime, nonce: jumpNonceRef.current })
            }}
            eventMarkers={eventMarkers}
          />
        </ExportBinProvider>
      )}

      <TripMap
        tripId={trip.id}
        activeTimeMs={activeTimeMs}
        events={vehicle?.events}
        onSeek={(isoTime) => {
          jumpNonceRef.current += 1
          setJumpSegId(null)
          setJumpTo({ startTime: isoTime, nonce: jumpNonceRef.current })
        }}
      />

      {frontSegs.length > 0 && (
        <section>
          <h2 className="mb-2 flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-text-muted">
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
          <h2 className="mb-2 flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-text-muted">
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
