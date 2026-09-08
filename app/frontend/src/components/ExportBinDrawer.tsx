// ExportBinDrawer (M11): renders the M6 export bin's staged ranges/segments
// as an orderable list with thumbnails, export options (camera, burn-in,
// quality, target), and an "Export" action that enqueues a job and tracks
// its progress inline via SSE.
import { useMemo, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronUp, Film, Loader2, Scissors, Trash2, X } from 'lucide-react'
import { useExportBin, type ExportRange } from './player/ExportBinContext'
import { buildPlaylist } from './player/playerMath'
import { formatDuration } from '../lib/format'
import { getShares } from '../lib/sharesApi'
import { postExport, type ExportItem, type ExportRequest } from '../lib/exportsApi'
import { useEvents } from '../hooks/useEvents'
import type { TripDetail } from '../types'

// rangeToItems expands one trip-relative export range into the
// front-camera segment items it spans (a range can cross more than one
// ~30s segment), each trimmed to the portion of that segment the range
// actually covers.
function rangeToItems(trip: TripDetail, range: ExportRange): ExportItem[] {
  const playlist = buildPlaylist(trip.segments, 'front')
  const tripStartMs = Date.parse(trip.started_at)
  const startAbsMs = tripStartMs + range.inS * 1000
  const endAbsMs = tripStartMs + range.outS * 1000

  const items: ExportItem[] = []
  for (const entry of playlist) {
    if (entry.absEndMs <= startAbsMs || entry.absStartMs >= endAbsMs) continue
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

function SegThumb({ segmentId }: { segmentId: number }) {
  const [broken, setBroken] = useState(false)
  if (broken) {
    return (
      <div className="flex size-10 shrink-0 items-center justify-center rounded bg-neutral-800 text-neutral-600">
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
        className="text-neutral-500 hover:text-neutral-200 disabled:opacity-25"
      >
        <ChevronUp className="size-3.5" />
      </button>
      <button
        type="button"
        disabled={index === count - 1}
        onClick={() => onMove(1)}
        className="text-neutral-500 hover:text-neutral-200 disabled:opacity-25"
      >
        <ChevronDown className="size-3.5" />
      </button>
    </div>
  )
}

export default function ExportBinDrawer({ trip }: { trip?: TripDetail }) {
  const exportBin = useExportBin()
  const { ranges, segmentIds } = exportBin.state
  const [camera, setCamera] = useState<'front' | 'rear' | 'both'>('front')
  const [burnIn, setBurnIn] = useState(false)
  const [quality, setQuality] = useState<'source' | '1080p'>('source')
  const [target, setTarget] = useState('download')
  const [name, setName] = useState('')
  const [jobId, setJobId] = useState<number | null>(null)

  const { data: shares } = useQuery({ queryKey: ['shares', 'export-target'], queryFn: getShares })
  const liveEvents = useEvents()
  const liveJob = jobId != null ? liveEvents[jobId] : undefined

  const empty = ranges.length === 0 && segmentIds.length === 0

  const exportMutation = useMutation({
    mutationFn: (req: ExportRequest) => postExport(req),
    onSuccess: (res) => setJobId(res.job.id),
  })

  const totalItems = useMemo(() => {
    if (!trip) return segmentIds.length
    return ranges.reduce((n, r) => n + (r.tripId === trip.id ? rangeToItems(trip, r).length : 0), 0) + segmentIds.length
  }, [trip, ranges, segmentIds])

  function handleExport() {
    if (!trip) return
    const items: ExportItem[] = []
    for (const r of ranges) {
      if (r.tripId !== trip.id) continue // bin can hold ranges from other trips; only the open one can be resolved here
      items.push(...rangeToItems(trip, r))
    }
    for (const id of segmentIds) items.push({ segment_id: id })
    if (items.length === 0) return

    exportMutation.mutate({
      items,
      camera,
      burn_in: burnIn,
      quality,
      target,
      name: name.trim() || undefined,
    })
  }

  if (empty && jobId == null) return null

  const running = liveJob ? liveJob.state === 'queued' || liveJob.state === 'running' : exportMutation.isPending
  const done = liveJob?.state === 'done'
  const failed = liveJob?.state === 'failed' || liveJob?.state === 'cancelled'

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-neutral-800 bg-neutral-900/60 p-3">
      <div className="flex items-center justify-between">
        <h3 className="flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-neutral-400">
          <Scissors className="size-3.5" /> Export bin ({totalItems} item{totalItems === 1 ? '' : 's'})
        </h3>
        <button
          type="button"
          onClick={exportBin.clearAll}
          className="flex items-center gap-1 text-[11px] text-neutral-600 hover:text-neutral-300"
        >
          <Trash2 className="size-3" /> clear
        </button>
      </div>

      {ranges.length > 0 && (
        <ul className="flex flex-col gap-1.5">
          {ranges.map((r, i) => (
            <li key={i} className="flex items-center gap-2 rounded border border-neutral-800 bg-black/30 px-2 py-1.5">
              <ReorderButtons index={i} count={ranges.length} onMove={(dir) => exportBin.moveRange(i, dir)} />
              <span className="flex-1 text-[12px] tabular-nums text-neutral-300">
                range {formatDuration(r.inS)} &rarr; {formatDuration(r.outS)}
                {trip && r.tripId !== trip.id && <span className="ml-1.5 text-neutral-600">(other trip)</span>}
              </span>
              <button type="button" onClick={() => exportBin.removeRange(i)} className="text-neutral-600 hover:text-red-400">
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}

      {segmentIds.length > 0 && (
        <ul className="flex flex-col gap-1.5">
          {segmentIds.map((id, i) => (
            <li key={id} className="flex items-center gap-2 rounded border border-neutral-800 bg-black/30 px-2 py-1.5">
              <ReorderButtons index={i} count={segmentIds.length} onMove={(dir) => exportBin.moveSegment(i, dir)} />
              <SegThumb segmentId={id} />
              <span className="flex-1 text-[12px] text-neutral-300">segment #{id}</span>
              <button type="button" onClick={() => exportBin.removeSegment(id)} className="text-neutral-600 hover:text-red-400">
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="flex flex-wrap items-center gap-2 border-t border-neutral-900 pt-2 text-[12px]">
        <select
          value={camera}
          onChange={(e) => setCamera(e.target.value as typeof camera)}
          className="rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-neutral-300"
        >
          <option value="front">front</option>
          <option value="rear">rear</option>
          <option value="both">both</option>
        </select>
        <label className="flex items-center gap-1.5 text-neutral-400">
          <input type="checkbox" checked={burnIn} onChange={(e) => setBurnIn(e.target.checked)} />
          burn in detections
        </label>
        <select
          value={quality}
          onChange={(e) => setQuality(e.target.value as typeof quality)}
          className="rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-neutral-300"
        >
          <option value="source">source quality</option>
          <option value="1080p">1080p</option>
        </select>
        <select
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          className="rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-neutral-300"
        >
          <option value="download">download</option>
          {(shares ?? []).map((s) => (
            <option key={s.id} value={String(s.id)}>
              share: {s.name}
            </option>
          ))}
        </select>
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="export name"
          className="w-36 rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-neutral-300 placeholder:text-neutral-700"
        />
        <button
          type="button"
          disabled={!trip || totalItems === 0 || running}
          onClick={handleExport}
          className="ml-auto flex items-center gap-1.5 rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-1.5 font-medium text-amber-400 disabled:opacity-40 hover:bg-amber-500/20"
        >
          {running && <Loader2 className="size-3.5 animate-spin" />}
          {running ? `${Math.round((liveJob?.progress ?? 0) * 100)}%` : 'Export'}
        </button>
      </div>

      {exportMutation.isError && (
        <p className="text-[11px] text-red-400">
          {exportMutation.error instanceof Error ? exportMutation.error.message : 'export failed to start'}
        </p>
      )}
      {liveJob && (
        <p className="text-[11px] text-neutral-500">
          {liveJob.message ?? liveJob.state}
          {done && (
            <>
              {' — '}
              <a href="/exports" className="text-amber-400 hover:underline">
                view in Exports
              </a>
            </>
          )}
          {failed && <span className="text-red-400"> — export failed</span>}
        </p>
      )}
    </div>
  )
}
