// ExportPanel (M11, WP4): renders the export bin's staged ranges/segments
// grouped by trip, with export options (camera, burn-in, quality, target)
// and an "Export" action that enqueues a job and tracks its progress
// inline via SSE. Ranges can come from trips other than the one currently
// open (the bin persists across trip navigation) — exporting resolves
// each range against its own trip, fetching trips other than the open one
// on demand.
import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { ChevronDown, ChevronUp, Film, Loader2, Scissors, Trash2, X } from 'lucide-react'
import { useExportBin, type ExportRange } from './player/ExportBinContext'
import { buildPlaylist } from './player/playerMath'
import { formatDuration, titleCase } from '../lib/format'
import { getSegment, getTrip } from '../lib/api'
import { getShares } from '../lib/sharesApi'
import {
  getExportCapabilities,
  getLastExportSettings,
  postExport,
  saveLastExportSettings,
  type ExportCodec,
  type ExportContainer,
  type ExportItem,
  type ExportQuality,
  type ExportRequest,
} from '../lib/exportsApi'
import { useEvents } from '../hooks/useEvents'
import { useDetectionFilter } from '../hooks/useDetectionFilter'
import type { TripDetail } from '../types'
import Switch from './ui/Switch'

// rangeToItems expands one trip-relative export range into the
// front-camera segment items it spans (a range can cross more than one
// ~30s segment), each trimmed to the portion of that segment the range
// actually covers. A segment whose file is unreadable is silently
// excluded (there's nothing ffmpeg could export from it) and its id is
// appended to skippedUnreadable so the caller can surface an inline
// notice instead of the export job just failing later.
function rangeToItems(trip: TripDetail, range: ExportRange, skippedUnreadable: number[]): ExportItem[] {
  const playlist = buildPlaylist(trip.segments, 'front')
  const tripStartMs = Date.parse(trip.started_at)
  const startAbsMs = tripStartMs + range.inS * 1000
  const endAbsMs = tripStartMs + range.outS * 1000

  const items: ExportItem[] = []
  for (const entry of playlist) {
    if (entry.absEndMs <= startAbsMs || entry.absStartMs >= endAbsMs) continue
    if (entry.segment.unreadable_reason) {
      skippedUnreadable.push(entry.segment.id)
      continue
    }
    const clipStartMs = Math.max(entry.absStartMs, startAbsMs)
    const clipEndMs = Math.min(entry.absEndMs, endAbsMs)
    items.push({
      segment_id: entry.segment.id,
      in_s: (clipStartMs - entry.absStartMs) / 1000,
      out_s: (clipEndMs - entry.absStartMs) / 1000,
    })
  }
  return items
}

// codecOption is one entry in the codec <select>: value plus whether it's
// currently selectable, given the chosen container and this server's
// codec capabilities (mirrors the server-side matrix in
// cmd/api/handlers_export.go's validate()/validateExportCodecAvailable,
// so a disabled option here is exactly one the POST would 400 on).
interface CodecOption {
  value: ExportCodec
  label: string
  disabled: boolean
}

function codecOptionsFor(container: ExportContainer, h265Available: boolean, av1Available: boolean): CodecOption[] {
  if (container === 'webm') {
    // webm can only carry vp8/vp9/av1 video — copy/h264/h265 are never
    // valid here, matching the server's "container webm requires codec
    // av1" rejection.
    return [{ value: 'av1', label: 'AV1', disabled: !av1Available }]
  }
  return [
    { value: 'copy', label: 'Copy (fastest, no re-encode)', disabled: false },
    { value: 'h264', label: 'H.264', disabled: false },
    { value: 'h265', label: 'H.265 / HEVC', disabled: !h265Available },
    { value: 'av1', label: 'AV1', disabled: !av1Available },
  ]
}

function SegThumb({ segmentId }: { segmentId: number }) {
  const [broken, setBroken] = useState(false)
  if (broken) {
    return (
      <div className="flex size-10 shrink-0 items-center justify-center rounded bg-surface-2 text-text-muted">
        <Film className="size-4" strokeWidth={1.5} />
      </div>
    )
  }
  return (
    // eslint-disable-next-line jsx-a11y/alt-text
    <img
      src={`/api/segments/${segmentId}/thumb.jpg`}
      onError={() => setBroken(true)}
      className="size-10 shrink-0 rounded object-cover"
    />
  )
}

function ReorderButtons({ index, count, onMove }: { index: number; count: number; onMove: (dir: -1 | 1) => void }) {
  return (
    <div className="flex flex-col">
      <button
        type="button"
        disabled={index === 0}
        onClick={() => onMove(-1)}
        className="text-text-muted hover:text-text disabled:opacity-25"
      >
        <ChevronUp className="size-3.5" />
      </button>
      <button
        type="button"
        disabled={index === count - 1}
        onClick={() => onMove(1)}
        className="text-text-muted hover:text-text disabled:opacity-25"
      >
        <ChevronDown className="size-3.5" />
      </button>
    </div>
  )
}

interface RangeGroup {
  tripId: number
  tripLabel: string
  entries: { range: ExportRange; index: number }[]
}

// groupRangesByTrip preserves each range's position in the flat `ranges`
// array (needed for moveRange/removeRange) while presenting them grouped
// under the trip they were captured from, in first-seen order.
function groupRangesByTrip(ranges: ExportRange[]): RangeGroup[] {
  const groups: RangeGroup[] = []
  const byTrip = new Map<number, RangeGroup>()
  ranges.forEach((range, index) => {
    let g = byTrip.get(range.tripId)
    if (!g) {
      g = { tripId: range.tripId, tripLabel: range.tripLabel, entries: [] }
      byTrip.set(range.tripId, g)
      groups.push(g)
    }
    g.entries.push({ range, index })
  })
  return groups
}

export default function ExportPanel({
  trip,
  onVisibilityChange,
}: {
  trip?: TripDetail
  // onVisibilityChange (fix round 1): reports whether this panel is
  // actually rendering anything, so a parent laying it out in a
  // collapsible side column (TripDetail's TripPlayerPanel) knows to keep
  // that column around — e.g. a job's live/finished status notice stays
  // visible here even after the bin that started it was cleared, which
  // the parent can't otherwise infer from the bin's own (now-empty) state.
  onVisibilityChange?: (visible: boolean) => void
}) {
  const exportBin = useExportBin()
  const { ranges, segments } = exportBin.state
  const lastSettings = useMemo(getLastExportSettings, [])
  const [camera, setCamera] = useState<'front' | 'rear' | 'both'>(lastSettings.camera)
  const [burnIn, setBurnIn] = useState(false)
  const [quality, setQuality] = useState<ExportQuality>(lastSettings.quality)
  const [target, setTarget] = useState(lastSettings.target)
  const [container, setContainer] = useState<ExportContainer>(lastSettings.container)
  const [codec, setCodec] = useState<ExportCodec>(lastSettings.codec)
  const [name, setName] = useState('')
  const [jobId, setJobId] = useState<number | null>(null)
  const [detectionFilter] = useDetectionFilter()

  const { data: capabilities } = useQuery({ queryKey: ['export-capabilities'], queryFn: getExportCapabilities })
  const h265Available = capabilities?.h265_available ?? false
  const av1Available = capabilities?.av1_available ?? false
  const codecOptions = useMemo(
    () => codecOptionsFor(container, h265Available, av1Available),
    [container, h265Available, av1Available],
  )
  // Keep codec valid for the current container/capabilities: switching to
  // webm must jump off whatever non-av1 codec was selected, and a codec
  // that just became unavailable (capabilities loaded after mount, or a
  // container switch away from webm) must fall back to the first
  // selectable option instead of silently submitting a combo the server
  // will 400 on.
  useEffect(() => {
    const current = codecOptions.find((o) => o.value === codec)
    if (current && !current.disabled) return
    const firstEnabled = codecOptions.find((o) => !o.disabled)
    if (firstEnabled) setCodec(firstEnabled.value)
  }, [codec, codecOptions])
  // Set to the count of unreadable segments silently dropped from the most
  // recent export attempt (see rangeToItems/exportMutation below) — an
  // unreadable segment can end up in the bin via a range that spans one
  // even though DualVideoPlayer refuses to *add* an already-known-
  // unreadable segment directly.
  const [unreadableSkipped, setUnreadableSkipped] = useState(0)

  const { data: shares } = useQuery({ queryKey: ['shares', 'export-target'], queryFn: getShares })
  const liveEvents = useEvents()
  const liveJob = jobId != null ? liveEvents[jobId] : undefined

  const empty = ranges.length === 0 && segments.length === 0
  useEffect(() => {
    onVisibilityChange?.(!(empty && jobId == null))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [empty, jobId])
  const groups = useMemo(() => groupRangesByTrip(ranges), [ranges])
  const totalItems = ranges.length + segments.length

  const exportMutation = useMutation({
    mutationFn: async () => {
      // Resolve every range against its own trip — the currently open one
      // is already in hand, but a range added while viewing a different
      // trip needs that trip fetched to know which segments it spans.
      const tripCache = new Map<number, TripDetail>()
      if (trip) tripCache.set(trip.id, trip)
      const skippedUnreadable: number[] = []
      const items: ExportItem[] = []
      for (const r of ranges) {
        let t = tripCache.get(r.tripId)
        if (!t) {
          t = await getTrip(r.tripId)
          tripCache.set(r.tripId, t)
        }
        items.push(...rangeToItems(t, r, skippedUnreadable))
      }
      // Whole segments were already refused at add-time (DualVideoPlayer)
      // when known unreadable then, but re-check here in case a segment
      // was marked unreadable after being staged — never send an id
      // ffmpeg can't decode. Fetched concurrently: each lookup is
      // independent (unlike the trip-cached range loop above), so there's
      // no reason to wait on them one at a time.
      const stagedMeta = await Promise.all(segments.map((seg) => getSegment(seg.id)))
      segments.forEach((seg, i) => {
        const meta = stagedMeta[i]
        if (meta.unreadable_reason) {
          skippedUnreadable.push(seg.id)
          return
        }
        items.push({ segment_id: seg.id })
      })
      setUnreadableSkipped(skippedUnreadable.length)
      if (items.length === 0) throw new Error('Nothing to export')

      saveLastExportSettings({ camera, quality, target, container, codec })
      const req: ExportRequest = {
        items,
        camera,
        burn_in: burnIn,
        quality,
        target,
        container,
        codec,
        name: name.trim() || undefined,
        overlay_filter: burnIn ? detectionFilter : undefined,
      }
      return postExport(req)
    },
    onSuccess: (res) => setJobId(res.job.id),
  })

  if (empty && jobId == null) return null

  const running = liveJob ? liveJob.state === 'queued' || liveJob.state === 'running' : exportMutation.isPending
  const done = liveJob?.state === 'done'
  const failed = liveJob?.state === 'failed' || liveJob?.state === 'cancelled'

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border bg-surface/60 p-3">
      <div className="flex items-center justify-between">
        <h3 className="flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-text-muted">
          <Scissors className="size-3.5" /> Export bin ({totalItems} item{totalItems === 1 ? '' : 's'})
        </h3>
        <button
          type="button"
          onClick={exportBin.clearAll}
          className="flex items-center gap-1 text-xs text-text-muted hover:text-text-secondary"
        >
          <Trash2 className="size-3" /> Clear
        </button>
      </div>

      {groups.map((group) => (
        <div key={group.tripId} className="flex flex-col gap-1.5">
          <Link
            to={`/footage/trips/${group.tripId}`}
            className="text-xs font-medium text-text-muted hover:text-text-secondary hover:underline"
          >
            {group.tripLabel}
            {trip && group.tripId === trip.id ? ' (this trip)' : ''}
          </Link>
          <ul className="flex flex-col gap-1.5">
            {group.entries.map(({ range: r, index: i }) => (
              <li key={i} className="flex items-center gap-2 rounded border border-border bg-surface-2 px-2 py-1.5">
                <ReorderButtons index={i} count={ranges.length} onMove={(dir) => exportBin.moveRange(i, dir)} />
                <span className="flex-1 text-[12px] tabular-nums text-text-secondary">
                  Range {formatDuration(r.inS)} &rarr; {formatDuration(r.outS)}
                </span>
                <button type="button" onClick={() => exportBin.removeRange(i)} className="text-text-muted hover:text-danger">
                  <X className="size-3.5" />
                </button>
              </li>
            ))}
          </ul>
        </div>
      ))}

      {segments.length > 0 && (
        <ul className="flex flex-col gap-1.5">
          {segments.map((seg, i) => (
            <li key={seg.id} className="flex items-center gap-2 rounded border border-border bg-surface-2 px-2 py-1.5">
              <ReorderButtons index={i} count={segments.length} onMove={(dir) => exportBin.moveSegment(i, dir)} />
              <SegThumb segmentId={seg.id} />
              <span className="flex-1 text-[12px] text-text-secondary">{seg.label}</span>
              <button type="button" onClick={() => exportBin.removeSegment(seg.id)} className="text-text-muted hover:text-danger">
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="flex flex-wrap items-center gap-2 border-t border-border pt-2 text-[12px]">
        <select
          value={camera}
          onChange={(e) => setCamera(e.target.value as typeof camera)}
          className="rounded border border-border bg-bg px-2 py-1 text-text-secondary"
        >
          <option value="front">Front</option>
          <option value="rear">Rear</option>
          <option value="both">Both</option>
        </select>
        <label className="flex items-center gap-1.5 text-text-muted">
          <Switch checked={burnIn} onChange={setBurnIn} label="Burn in detections" />
          Burn in detections
        </label>
        {burnIn && (
          <span className="text-xs text-text-muted" title="Uses the player's live detection filter (Filter button on the timeline toolbar)">
            {[
              `≥${Math.round(detectionFilter.min_conf * 100)}%`,
              [detectionFilter.objects && 'objects', detectionFilter.plates && 'plates', detectionFilter.vehicles && 'vehicles']
                .filter(Boolean)
                .join('/') || 'nothing',
              detectionFilter.classes.length > 0 ? `(${detectionFilter.classes.join(', ')})` : '(all classes)',
            ].join(' · ')}
          </span>
        )}
        <select
          value={quality}
          onChange={(e) => setQuality(e.target.value as ExportQuality)}
          className="rounded border border-border bg-bg px-2 py-1 text-text-secondary"
        >
          <option value="source">Source quality</option>
          <option value="high">High</option>
          <option value="balanced">Balanced</option>
          <option value="small">Small file</option>
          <option value="1080p">1080p (legacy)</option>
        </select>
        <select
          value={container}
          onChange={(e) => setContainer(e.target.value as ExportContainer)}
          className="rounded border border-border bg-bg px-2 py-1 text-text-secondary"
        >
          <option value="mp4">MP4</option>
          <option value="mkv">MKV</option>
          <option value="webm">WebM</option>
        </select>
        <select
          value={codec}
          onChange={(e) => setCodec(e.target.value as ExportCodec)}
          className="rounded border border-border bg-bg px-2 py-1 text-text-secondary"
        >
          {codecOptions.map((opt) => (
            <option key={opt.value} value={opt.value} disabled={opt.disabled}>
              {opt.label}
              {opt.disabled ? ' (unavailable)' : ''}
            </option>
          ))}
        </select>
        <select
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          className="rounded border border-border bg-bg px-2 py-1 text-text-secondary"
        >
          <option value="download">Download</option>
          {(shares ?? []).map((s) => (
            <option key={s.id} value={String(s.id)}>
              Share: {s.name}
            </option>
          ))}
        </select>
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Export name"
          className="w-36 rounded border border-border bg-bg px-2 py-1 text-text-secondary placeholder:text-text-muted"
        />
        <button
          type="button"
          disabled={totalItems === 0 || running || (codecOptions.find((o) => o.value === codec)?.disabled ?? false)}
          onClick={() => exportMutation.mutate()}
          className="ml-auto flex items-center gap-1.5 rounded-md border border-accent/30 bg-accent/10 px-3 py-1.5 font-medium text-accent disabled:opacity-40 hover:bg-accent/20"
        >
          {running && <Loader2 className="size-3.5 animate-spin" />}
          {running ? `${Math.round((liveJob?.progress ?? 0) * 100)}%` : 'Export'}
        </button>
      </div>

      {exportMutation.isError && (
        <p className="text-xs text-danger">
          {exportMutation.error instanceof Error ? exportMutation.error.message : 'Export failed to start'}
        </p>
      )}
      {unreadableSkipped > 0 && (
        <p className="text-xs text-danger">
          {unreadableSkipped} segment{unreadableSkipped === 1 ? '' : 's'} in this export {unreadableSkipped === 1 ? 'is' : 'are'} unreadable
          and {unreadableSkipped === 1 ? 'was' : 'were'} skipped.
        </p>
      )}
      {liveJob && (
        <p className="text-xs text-text-muted">
          {liveJob.message ?? titleCase(liveJob.state)}
          {done && (
            <>
              {' — '}
              <Link to="/exports" className="text-accent hover:underline">
                View in Exports
              </Link>
            </>
          )}
          {failed && <span className="text-danger"> — Export failed</span>}
        </p>
      )}
    </div>
  )
}
