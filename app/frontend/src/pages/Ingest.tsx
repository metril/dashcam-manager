// M10 Ingest page: SSD status card, target share picker, a trip table with
// per-trip ingest/verify progress against the selected share, row
// selection, "Ingest selected" (enqueues a job, tracked live via SSE), and
// "Clean up verified from SSD" with a dry-run preview + confirmation
// dialog before anything is actually deleted.
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { HardDrive, Loader2, Trash2, UploadCloud } from 'lucide-react'
import { getSSDStatus, startCleanup, startIngest, type CleanupStats, type IngestTripSummary } from '../lib/ingestApi'
import { getShares } from '../lib/sharesApi'
import { useEvents } from '../hooks/useEvents'
import { formatBytes } from '../lib/format'
import { formatDateLong } from '../lib/format'

export default function Ingest() {
  const queryClient = useQueryClient()
  const liveEvents = useEvents()

  const [targetShareID, setTargetShareID] = useState<number | null>(null)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [activeIngestJobID, setActiveIngestJobID] = useState<number | null>(null)
  const [activeCleanupJobID, setActiveCleanupJobID] = useState<number | null>(null)
  const [preview, setPreview] = useState<CleanupStats | null>(null)
  const [confirmingCleanup, setConfirmingCleanup] = useState(false)

  const { data: shares } = useQuery({ queryKey: ['shares'], queryFn: getShares, refetchInterval: 15000 })

  const {
    data: status,
    isLoading,
    isError,
  } = useQuery({
    queryKey: ['ssd-status', targetShareID],
    queryFn: () => getSSDStatus(targetShareID ?? undefined),
    refetchInterval: 10000,
  })

  const trips = useMemo(() => status?.trips ?? [], [status])

  function toggleRow(id: number) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  function toggleAll() {
    setSelected((prev) => (prev.size === trips.length ? new Set() : new Set(trips.map((t) => t.trip_id))))
  }

  function invalidateStatus() {
    queryClient.invalidateQueries({ queryKey: ['ssd-status'] })
  }

  const ingestMutation = useMutation({
    mutationFn: () =>
      startIngest({
        source_id: status!.source_id!,
        target_share_id: targetShareID!,
        trip_ids: selected.size > 0 ? Array.from(selected) : undefined,
        verify: 'sha256',
      }),
    onSuccess: (result) => {
      if (result.job) setActiveIngestJobID(result.job.id)
      invalidateStatus()
    },
  })

  const previewMutation = useMutation({
    mutationFn: () =>
      startCleanup({ trip_ids: selected.size > 0 ? Array.from(selected) : undefined, dry_run: true }),
    onSuccess: (result) => {
      setPreview(result as CleanupStats)
      setConfirmingCleanup(true)
    },
  })

  const cleanupMutation = useMutation({
    mutationFn: () => startCleanup({ trip_ids: selected.size > 0 ? Array.from(selected) : undefined, dry_run: false }),
    onSuccess: (result) => {
      if ('job' in result && result.job) setActiveCleanupJobID(result.job.id)
      setConfirmingCleanup(false)
      setPreview(null)
      invalidateStatus()
    },
  })

  const ingestJob = activeIngestJobID != null ? liveEvents[activeIngestJobID] : undefined
  const cleanupJob = activeCleanupJobID != null ? liveEvents[activeCleanupJobID] : undefined

  const canIngest = status?.present && status.source_id != null && targetShareID != null && !ingestMutation.isPending
  const readOnlyWarning = status?.present && !status.mounted_rw

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between">
        <h1 className="text-sm font-semibold text-neutral-200">Ingest</h1>
      </div>

      {isLoading && <div className="text-xs text-neutral-600">Loading…</div>}
      {isError && <div className="text-xs text-red-400">Failed to load SSD status.</div>}

      {status && (
        <div className="rounded-lg border border-neutral-900 bg-neutral-950/60 p-3">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <HardDrive className="size-3.5 text-neutral-500" />
            <span className="font-medium text-neutral-200">{status.path}</span>
            <span
              className={[
                'rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide',
                status.present ? 'bg-emerald-500/10 text-emerald-400' : 'bg-red-500/10 text-red-400',
              ].join(' ')}
            >
              {status.present ? 'present' : 'not found'}
            </span>
            {status.present && (
              <span
                className={[
                  'rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide',
                  status.mounted_rw ? 'bg-amber-500/10 text-amber-400' : 'bg-neutral-800 text-neutral-500',
                ].join(' ')}
                title="Whether the SSD is currently writable — required for cleanup"
              >
                {status.mounted_rw ? 'read-write' : 'read-only'}
              </span>
            )}
          </div>

          <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-neutral-500">
            {status.total_bytes != null && status.free_bytes != null && (
              <span>
                {formatBytes(status.free_bytes)} free of {formatBytes(status.total_bytes)}
              </span>
            )}
            <span>{status.segments_total} segments indexed</span>
            <span>{status.segments_ingested} ingested</span>
            <span>{status.segments_unindexed} not yet ingested</span>
          </div>

          {readOnlyWarning && (
            <div className="mt-1.5 text-[11px] text-amber-400">
              SSD is mounted read-only — cleanup will fail until it's remounted rw (see README / compose.yaml).
            </div>
          )}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <label className="flex items-center gap-2 text-xs text-neutral-400">
          Target share
          <select
            value={targetShareID ?? ''}
            onChange={(e) => setTargetShareID(e.target.value ? Number(e.target.value) : null)}
            className="rounded-md border border-neutral-800 bg-neutral-900 px-2 py-1.5 text-xs text-neutral-200 outline-none focus:border-amber-500/50"
          >
            <option value="">Select a share…</option>
            {shares?.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name} ({s.status})
              </option>
            ))}
          </select>
        </label>

        <button
          type="button"
          disabled={!canIngest}
          onClick={() => ingestMutation.mutate()}
          className="flex items-center gap-1.5 rounded-md bg-amber-500/10 px-2.5 py-1.5 text-xs font-medium text-amber-400 transition-colors hover:bg-amber-500/20 disabled:opacity-40"
        >
          {ingestMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <UploadCloud className="size-3.5" />}
          Ingest {selected.size > 0 ? `${selected.size} selected` : 'all'}
        </button>

        <button
          type="button"
          disabled={previewMutation.isPending}
          onClick={() => previewMutation.mutate()}
          className="flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium text-neutral-400 transition-colors hover:bg-neutral-900 hover:text-red-400 disabled:opacity-40"
        >
          {previewMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Trash2 className="size-3.5" />}
          Clean up verified from SSD
        </button>
      </div>

      {ingestMutation.isError && (
        <div className="text-[11px] text-red-400">{(ingestMutation.error as Error).message}</div>
      )}

      {ingestJob && (
        <div className="rounded-lg border border-neutral-900 bg-neutral-950/60 p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-neutral-200">Ingest job #{ingestJob.id}</span>
            <span className="text-neutral-500">{ingestJob.state}</span>
          </div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-neutral-900">
            <div
              className="h-full rounded-full bg-amber-500 transition-all"
              style={{ width: `${Math.round(ingestJob.progress * 100)}%` }}
            />
          </div>
          {ingestJob.message && <div className="mt-1.5 text-[11px] text-neutral-500">{ingestJob.message}</div>}
        </div>
      )}

      {cleanupJob && (
        <div className="rounded-lg border border-neutral-900 bg-neutral-950/60 p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-neutral-200">Cleanup job #{cleanupJob.id}</span>
            <span className="text-neutral-500">{cleanupJob.state}</span>
          </div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-neutral-900">
            <div
              className="h-full rounded-full bg-red-500 transition-all"
              style={{ width: `${Math.round(cleanupJob.progress * 100)}%` }}
            />
          </div>
          {cleanupJob.message && <div className="mt-1.5 text-[11px] text-neutral-500">{cleanupJob.message}</div>}
        </div>
      )}

      {trips.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-neutral-900">
          <table className="w-full text-left text-xs">
            <thead className="bg-neutral-950/80 text-neutral-500">
              <tr>
                <th className="w-8 px-3 py-2">
                  <input type="checkbox" checked={selected.size === trips.length && trips.length > 0} onChange={toggleAll} />
                </th>
                <th className="px-3 py-2">Date</th>
                <th className="px-3 py-2">Segments</th>
                <th className="px-3 py-2">Bytes</th>
                <th className="px-3 py-2">Ingested</th>
                <th className="px-3 py-2">Verified</th>
              </tr>
            </thead>
            <tbody>
              {trips.map((t: IngestTripSummary) => {
                const pct = t.segment_count > 0 ? Math.round((t.ingested_count / t.segment_count) * 100) : 0
                return (
                  <tr key={t.trip_id} className="border-t border-neutral-900 text-neutral-300">
                    <td className="px-3 py-2">
                      <input type="checkbox" checked={selected.has(t.trip_id)} onChange={() => toggleRow(t.trip_id)} />
                    </td>
                    <td className="px-3 py-2">{formatDateLong(t.started_at)}</td>
                    <td className="px-3 py-2">{t.segment_count}</td>
                    <td className="px-3 py-2">{formatBytes(t.bytes)}</td>
                    <td className="px-3 py-2">{pct}%</td>
                    <td className="px-3 py-2">
                      {t.verified_count}/{t.segment_count}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      {status && status.present && targetShareID == null && (
        <div className="text-xs text-neutral-600">Select a target share above to see per-trip ingest progress.</div>
      )}

      {confirmingCleanup && preview && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-md rounded-lg border border-neutral-800 bg-neutral-950 p-4">
            <h2 className="mb-2 text-sm font-semibold text-neutral-200">Confirm cleanup</h2>
            <p className="text-xs text-neutral-400">
              This will permanently delete {preview.items.length} verified segment{preview.items.length === 1 ? '' : 's'} (
              {formatBytes(preview.bytes_deleted)}) from the SSD. Their copies on the target share are unaffected.
            </p>
            {preview.segments_skipped > 0 && (
              <p className="mt-2 text-[11px] text-amber-400">
                {preview.segments_skipped} segment(s) will be skipped (not yet verified, or their destination copy no
                longer matches).
              </p>
            )}
            <div className="mt-4 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => {
                  setConfirmingCleanup(false)
                  setPreview(null)
                }}
                className="rounded-md px-3 py-1.5 text-xs text-neutral-400 hover:bg-neutral-900"
              >
                Cancel
              </button>
              <button
                type="button"
                disabled={cleanupMutation.isPending}
                onClick={() => cleanupMutation.mutate()}
                className="flex items-center gap-1.5 rounded-md bg-red-500/10 px-3 py-1.5 text-xs font-medium text-red-400 hover:bg-red-500/20 disabled:opacity-50"
              >
                {cleanupMutation.isPending && <Loader2 className="size-3.5 animate-spin" />}
                Delete from SSD
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
