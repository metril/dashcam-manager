// M10 Ingest page: SSD status card, target share picker, a trip table with
// per-trip ingest/verify progress against the selected share, row
// selection, "Ingest selected" (enqueues a job, tracked live via SSE), and
// "Clean up verified from SSD" with a dry-run preview + confirmation
// dialog before anything is actually deleted.
import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Eject, HardDrive, Loader2, ScanSearch, Trash2, UploadCloud } from 'lucide-react'
import {
  ejectSSD,
  getSSDStatus,
  startCleanup,
  startIngest,
  type CleanupStats,
  type IngestTripSummary,
} from '../lib/ingestApi'
import { getJobs, scanSource } from '../lib/jobsApi'
import { getShares } from '../lib/sharesApi'
import Switch from '../components/ui/Switch'
import { useEvents } from '../hooks/useEvents'
import { formatBytes, formatDateLong, humanizeState, titleCase } from '../lib/format'

// Job types that read or write the SSD's contents and therefore make a
// forced eject unsafe while one is queued or running against it — mirrors
// cmd/api/handlers_ingest.go's ejectBlockingJobTypes exactly; keep the two
// in sync.
const EJECT_BLOCKING_JOB_TYPES = new Set(['scan', 'ingest', 'cleanup', 'thumbs', 'index_detections'])

export default function Ingest() {
  const queryClient = useQueryClient()
  const liveEvents = useEvents()

  const [targetShareID, setTargetShareID] = useState<number | null>(null)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [activeIngestJobID, setActiveIngestJobID] = useState<number | null>(null)
  const [activeCleanupJobID, setActiveCleanupJobID] = useState<number | null>(null)
  const [preview, setPreview] = useState<CleanupStats | null>(null)
  const [confirmingCleanup, setConfirmingCleanup] = useState(false)
  // Bypasses the scan's empty-root safety check (same as the Shares page).
  const [forceScan, setForceScan] = useState(false)

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
    queryClient.invalidateQueries({ queryKey: ['jobs-for-eject'] })
  }

  // Polled (rather than SSE-only) so the Eject button also accounts for
  // scan/thumbs/index_detections jobs this page never enqueued itself
  // (e.g. a background re-scan) — not just the ingest/cleanup jobs it
  // tracks locally below.
  const { data: allJobs } = useQuery({
    queryKey: ['jobs-for-eject'],
    queryFn: () => getJobs(),
    refetchInterval: 5000,
  })

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
      startCleanup({
        source_id: status?.source_id,
        trip_ids: selected.size > 0 ? Array.from(selected) : undefined,
        dry_run: true,
      }),
    onSuccess: (result) => {
      setPreview(result as CleanupStats)
      setConfirmingCleanup(true)
    },
  })

  const cleanupMutation = useMutation({
    mutationFn: () =>
      startCleanup({
        source_id: status?.source_id,
        trip_ids: selected.size > 0 ? Array.from(selected) : undefined,
        dry_run: false,
      }),
    onSuccess: (result) => {
      if ('job' in result && result.job) setActiveCleanupJobID(result.job.id)
      setConfirmingCleanup(false)
      setPreview(null)
      invalidateStatus()
    },
  })

  const scanMutation = useMutation({
    mutationFn: () => scanSource(status!.source_id!, false, forceScan),
    onSuccess: () => invalidateStatus(),
  })

  const ejectMutation = useMutation({
    mutationFn: () => ejectSSD(),
    onSuccess: () => invalidateStatus(),
  })

  const ingestJob = activeIngestJobID != null ? liveEvents[activeIngestJobID] : undefined
  const cleanupJob = activeCleanupJobID != null ? liveEvents[activeCleanupJobID] : undefined

  const ssdScanJob = useMemo(() => {
    if (status?.source_id == null) return undefined
    return allJobs?.find((j) => {
      if (j.type !== 'scan' || (j.state !== 'queued' && j.state !== 'running')) return false
      return (j.params as { source_id?: number } | undefined)?.source_id === status.source_id
    })
  }, [allJobs, status])
  const ssdScanLive = ssdScanJob ? liveEvents[ssdScanJob.id] : undefined
  const scanLiveDone = ssdScanLive ? ssdScanLive.state !== 'queued' && ssdScanLive.state !== 'running' : false
  const scanning = (Boolean(ssdScanJob) && !scanLiveDone) || scanMutation.isPending

  // SSE reports the scan finishing before the 5s jobs poll does; refresh
  // immediately so the buttons and segment counts don't lag.
  useEffect(() => {
    if (scanLiveDone) {
      queryClient.invalidateQueries({ queryKey: ['jobs-for-eject'] })
      queryClient.invalidateQueries({ queryKey: ['ssd-status'] })
    }
  }, [scanLiveDone, queryClient])

  const ingestRunning =
    (ingestJob ? ingestJob.state === 'queued' || ingestJob.state === 'running' : false) ||
    Boolean(
      status?.source_id != null &&
        allJobs?.some(
          (j) =>
            j.type === 'ingest' &&
            (j.state === 'queued' || j.state === 'running') &&
            (j.params as { source_id?: number } | undefined)?.source_id === status.source_id,
        ),
    )

  const canIngest =
    status?.present && status.source_id != null && targetShareID != null && !ingestMutation.isPending && !scanning
  const readOnlyWarning = status?.present && !status.mounted_rw
  const canCleanup = status?.present && status.mounted_rw && !previewMutation.isPending
  const cleanupDisabledReason = !status?.present
    ? 'SSD not present'
    : !status?.mounted_rw
      ? "SSD is mounted read-only — remount it read-write to clean up (see README / compose.yaml)"
      : undefined

  const ingestJobActive = ingestJob ? ingestJob.state === 'queued' || ingestJob.state === 'running' : false
  const cleanupJobActive = cleanupJob ? cleanupJob.state === 'queued' || cleanupJob.state === 'running' : false

  // Any queued/running job of a blocking type targeting the SSD source —
  // not just the ingest/cleanup jobs this page itself started (ingestJob/
  // cleanupJob above, which update instantly via SSE) — disables Eject.
  // Falls back to those two SSE-tracked jobs when allJobs hasn't loaded
  // yet (or is momentarily stale) so there's no gap right after a mutation
  // succeeds.
  const blockingJob = useMemo(() => {
    if (status?.source_id == null) return undefined
    return allJobs?.find((j) => {
      if (j.state !== 'queued' && j.state !== 'running') return false
      if (!EJECT_BLOCKING_JOB_TYPES.has(j.type)) return false
      const params = j.params as { source_id?: number } | undefined
      return params?.source_id === status.source_id
    })
  }, [allJobs, status?.source_id])

  const ejectBlocked = Boolean(blockingJob) || ingestJobActive || cleanupJobActive
  const ejectBusyReason = blockingJob
    ? `Eject is unavailable while ${blockingJob.type} is running`
    : ingestJobActive
      ? 'Eject is unavailable while ingest is running'
      : cleanupJobActive
        ? 'Eject is unavailable while cleanup is running'
        : 'Safely unmount the SSD before unplugging it'

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between">
        <h1 className="text-sm font-semibold text-text">Ingest</h1>
      </div>

      {isLoading && <div className="text-xs text-text-muted">Loading…</div>}
      {isError && <div className="text-xs text-danger">Failed to load SSD status.</div>}

      {status && (
        <div className="rounded-lg border border-border bg-bg/60 p-3">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <HardDrive className="size-3.5 text-text-muted" />
            <span className="font-medium text-text">{status.path}</span>
            <span
              className={[
                'rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide',
                status.present ? 'bg-success/10 text-success' : 'bg-danger/10 text-danger',
              ].join(' ')}
            >
              {status.present ? 'Present' : 'Not found'}
            </span>
            {status.present && (
              <span
                className={[
                  'rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide',
                  status.mounted_rw ? 'bg-accent/10 text-accent' : 'bg-surface-2 text-text-muted',
                ].join(' ')}
                title="Whether the SSD is currently writable — required for cleanup"
              >
                {status.mounted_rw ? 'Read-write' : 'Read-only'}
              </span>
            )}
          </div>

          <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-text-muted">
            {status.total_bytes != null && status.free_bytes != null && (
              <span>
                {formatBytes(status.free_bytes)} free of {formatBytes(status.total_bytes)}
              </span>
            )}
            <span>{status.segments_total} segments indexed</span>
            <span>{status.segments_ingested} ingested</span>
            <span>{status.segments_not_ingested} not yet ingested</span>
            {status.mount_source && <span>Mounted from {status.mount_source}</span>}
            {status.last_mounted_at && <span>Last mounted {formatDateLong(status.last_mounted_at)}</span>}
          </div>

          {readOnlyWarning && (
            <div className="mt-1.5 text-xs text-accent">
              SSD is mounted read-only — cleanup will fail until it's remounted rw (see README / compose.yaml).
            </div>
          )}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <label className="flex items-center gap-2 text-xs text-text-muted">
          Target share
          <select
            value={targetShareID ?? ''}
            onChange={(e) => setTargetShareID(e.target.value ? Number(e.target.value) : null)}
            className="rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50"
          >
            <option value="">Select a share…</option>
            {shares?.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name} ({humanizeState(s.status)})
              </option>
            ))}
          </select>
        </label>

        <button
          type="button"
          disabled={!status?.present || status.source_id == null || scanning || ingestRunning}
          title={
            ingestRunning
              ? 'Scan is unavailable while an ingest is running'
              : 'Walk the SSD and index its segments so they can be ingested'
          }
          onClick={() => scanMutation.mutate()}
          className="flex items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/20 disabled:opacity-40"
        >
          {scanning ? <Loader2 className="size-3.5 animate-spin" /> : <ScanSearch className="size-3.5" />}
          {scanning ? 'Scanning…' : 'Scan SSD'}
        </button>

        <label
          className="flex items-center gap-2 text-xs text-text-muted"
          title="Bypasses the scan's empty-root safety check. Only needed if you deliberately emptied the SSD."
        >
          <Switch checked={forceScan} onChange={setForceScan} label="Force scan" />
          Force scan
        </label>

        <button
          type="button"
          disabled={!canIngest}
          onClick={() => ingestMutation.mutate()}
          className="flex items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/20 disabled:opacity-40"
        >
          {ingestMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <UploadCloud className="size-3.5" />}
          Ingest {selected.size > 0 ? `${selected.size} selected` : 'all'}
        </button>

        <button
          type="button"
          disabled={!canCleanup}
          title={cleanupDisabledReason}
          onClick={() => previewMutation.mutate()}
          className="flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium text-text-muted transition-colors hover:bg-surface hover:text-danger disabled:opacity-40"
        >
          {previewMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Trash2 className="size-3.5" />}
          Clean up verified from SSD
        </button>

        <button
          type="button"
          disabled={!status?.present || ejectMutation.isPending || ejectBlocked}
          title={ejectBusyReason}
          onClick={() => ejectMutation.mutate()}
          className="flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-xs font-medium text-text-muted transition-colors hover:bg-surface hover:text-text disabled:opacity-40"
        >
          {ejectMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Eject className="size-3.5" />}
          Eject SSD
        </button>
      </div>

      {scanMutation.isError && (
        <div className="text-xs text-danger">{(scanMutation.error as Error).message}</div>
      )}

      {ssdScanJob && (
        <div className="rounded-lg border border-border bg-bg/60 p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-text">Scan job #{ssdScanJob.id}</span>
            <span className="text-text-muted">{titleCase((ssdScanLive ?? ssdScanJob).state)}</span>
          </div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface">
            <div
              className="h-full rounded-full bg-accent transition-all"
              style={{ width: `${Math.round((ssdScanLive ?? ssdScanJob).progress * 100)}%` }}
            />
          </div>
          {(ssdScanLive ?? ssdScanJob).message && (
            <div className="mt-1.5 text-xs text-text-muted">{(ssdScanLive ?? ssdScanJob).message}</div>
          )}
        </div>
      )}

      {status?.present && status.segments_total === 0 && !scanning && (
        <div className="text-xs text-text-muted">No segments found yet — scan the SSD.</div>
      )}

      {ejectMutation.isError && (
        <div className="text-xs text-danger">{(ejectMutation.error as Error).message}</div>
      )}

      {ingestMutation.isError && (
        <div className="text-xs text-danger">{(ingestMutation.error as Error).message}</div>
      )}

      {ingestJob && (
        <div className="rounded-lg border border-border bg-bg/60 p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-text">Ingest job #{ingestJob.id}</span>
            <span className="text-text-muted">{titleCase(ingestJob.state)}</span>
          </div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface">
            <div
              className="h-full rounded-full bg-accent transition-all"
              style={{ width: `${Math.round(ingestJob.progress * 100)}%` }}
            />
          </div>
          {ingestJob.message && <div className="mt-1.5 text-xs text-text-muted">{ingestJob.message}</div>}
        </div>
      )}

      {cleanupJob && (
        <div className="rounded-lg border border-border bg-bg/60 p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-text">Cleanup job #{cleanupJob.id}</span>
            <span className="text-text-muted">{titleCase(cleanupJob.state)}</span>
          </div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface">
            <div
              className="h-full rounded-full bg-danger transition-all"
              style={{ width: `${Math.round(cleanupJob.progress * 100)}%` }}
            />
          </div>
          {cleanupJob.message && <div className="mt-1.5 text-xs text-text-muted">{cleanupJob.message}</div>}
        </div>
      )}

      {trips.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-border">
          <table className="w-full text-left text-xs">
            <thead className="bg-bg/80 text-text-muted">
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
                  <tr key={t.trip_id} className="border-t border-border text-text-secondary">
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
        <div className="text-xs text-text-muted">Select a target share above to see per-trip ingest progress.</div>
      )}

      {confirmingCleanup && preview && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-md rounded-lg border border-border bg-bg p-4">
            <h2 className="mb-2 text-sm font-semibold text-text">Confirm cleanup</h2>
            <p className="text-xs text-text-muted">
              This will permanently delete {preview.items.length} verified segment{preview.items.length === 1 ? '' : 's'} (
              {formatBytes(preview.bytes_deleted)}) from the SSD. Their copies on the target share are unaffected.
            </p>
            {preview.segments_skipped > 0 && (
              <p className="mt-2 text-xs text-accent">
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
                className="rounded-md px-3 py-1.5 text-xs text-text-muted hover:bg-surface"
              >
                Cancel
              </button>
              <button
                type="button"
                disabled={cleanupMutation.isPending}
                onClick={() => cleanupMutation.mutate()}
                className="flex items-center gap-1.5 rounded-md bg-danger/10 px-3 py-1.5 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
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
