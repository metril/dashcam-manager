// Task 5 (dashcam network pull): Dashcams page — signing key setup, a
// list of configured dashcam sources with pull/cleanup/reprocess actions
// and live status, and an add/edit dialog. Mirrors Shares.tsx's list/
// dialog structure. Each action's typed job result (pull/cleanup/
// reprocess counts — see lib/dashcamApi.ts) is rendered directly here
// rather than via the generic components/JobResult.tsx (used by
// Settings.tsx), since these results carry action-specific fields worth
// showing in full, not just a "queued, view job #id" line.
import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Copy, KeyRound, Loader2, Pencil, Plus, RotateCw, SatelliteDish, Trash2, X } from 'lucide-react'
import {
  cleanupDashcam,
  createDashcam,
  deleteDashcam,
  generateDashcamKey,
  getDashcamKey,
  getDashcams,
  getDashcamStatus,
  pullDashcam,
  reprocessDashcam,
  testDashcam,
  updateDashcam,
  type Dashcam,
  type DashcamCleanupResult,
  type DashcamInput,
  type DashcamPullResult,
  type DashcamReprocessResult,
  type DashcamStatus,
  type ReprocessParams,
} from '../lib/dashcamApi'
import { getJob, type Job } from '../lib/jobsApi'
import { getShares } from '../lib/sharesApi'
import { formatBytes } from '../lib/format'

const inputClass =
  'rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50'

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1 text-xs text-text-muted">
      {label}
      {children}
    </label>
  )
}

function ErrorLine({ error }: { error: unknown }) {
  return <p className="text-[11px] text-danger">{error instanceof Error ? error.message : 'Request failed.'}</p>
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text)
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        } catch {
          // clipboard API unavailable/denied — silently no-op, the text is
          // still fully selectable/visible for a manual copy.
        }
      }}
      className="flex items-center gap-1 rounded-md border border-border bg-surface/60 px-1.5 py-1 text-[11px] text-text-muted hover:border-text-muted hover:text-text"
    >
      <Copy className="size-3" />
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

// --- Key card ----------------------------------------------------------

function KeyCard() {
  const queryClient = useQueryClient()
  const keyQuery = useQuery({ queryKey: ['dashcam-key'], queryFn: getDashcamKey })
  const genMutation = useMutation({
    mutationFn: (force: boolean) => generateDashcamKey(force),
    onSuccess: (result) => queryClient.setQueryData(['dashcam-key'], result),
  })

  function handleGenerate() {
    const force = keyQuery.data?.configured ?? false
    if (force && !confirm('Regenerate the dashcam signing key? Every dashcam config must be re-pasted with the new key, or pulls/cleanup/reprocess will fail.')) {
      return
    }
    genMutation.mutate(force)
  }

  return (
    <section className="flex flex-col gap-3 rounded-lg border border-border bg-bg/60 p-4">
      <h2 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-text-secondary">
        <KeyRound className="size-3.5" />
        Signing key
      </h2>

      {keyQuery.isLoading && <span className="text-xs text-text-muted">Loading…</span>}
      {keyQuery.isError && <ErrorLine error={keyQuery.error} />}

      {keyQuery.data && !keyQuery.data.configured && (
        <div className="flex flex-col gap-2">
          <p className="text-xs text-text-muted">
            No signing key configured yet. Dashcam-manager signs every request it makes to a dashcam's ingest API with
            an Ed25519 key pair; generate one here, then paste the resulting public key into each dashcam's config
            under <code className="rounded bg-surface px-1 py-0.5 text-[11px]">ingest.public_keys</code>.
          </p>
          <button
            type="button"
            onClick={handleGenerate}
            disabled={genMutation.isPending}
            className="flex w-fit items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 disabled:opacity-40"
          >
            {genMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <KeyRound className="size-3.5" />}
            Generate key
          </button>
        </div>
      )}

      {(keyQuery.data?.configured || genMutation.data?.configured) && (
        <div className="flex flex-col gap-2">
          {(() => {
            const view = genMutation.data ?? keyQuery.data!
            return (
              <>
                {view.error_code === 'key_unreadable' && (
                  <p className="rounded-md border border-danger/30 bg-danger/10 px-2.5 py-1.5 text-[11px] text-danger">
                    This key can no longer be decrypted — DASHCAM_SECRET_KEY was likely changed since it was
                    generated. Pull/cleanup/reprocess jobs for every dashcam will fail until you{' '}
                    <strong>Regenerate</strong> below and re-paste the new public key into each dashcam's config.
                  </p>
                )}
                <div className="flex items-center gap-3 text-xs">
                  <span className="text-text-muted">Key ID</span>
                  <span className="font-mono text-text">{view.key_id}</span>
                </div>
                <div className="flex items-start gap-2">
                  <code className="flex-1 overflow-x-auto rounded border border-border bg-surface px-2 py-1.5 font-mono text-[11px] text-text">
                    {view.public_key_b64}
                  </code>
                  <CopyButton text={view.public_key_b64 ?? ''} />
                </div>
                <p className="text-[11px] text-text-muted">
                  Paste this snippet into the dashcam's config and enable ingest, then restart it:
                </p>
                <div className="flex items-start gap-2">
                  <pre className="flex-1 overflow-x-auto rounded border border-border bg-surface px-2 py-1.5 font-mono text-[11px] text-text">
                    {view.config_snippet}
                  </pre>
                  <CopyButton text={view.config_snippet ?? ''} />
                </div>
                <p className="text-[11px] text-text-muted">
                  This is a full top-level <code className="rounded bg-surface px-1 py-0.5">ingest:</code> block — if
                  the dashcam's config already has one, merge just the <code className="rounded bg-surface px-1 py-0.5">enabled</code> and{' '}
                  <code className="rounded bg-surface px-1 py-0.5">public_keys</code> entries into it instead of
                  pasting a second block.
                </p>
              </>
            )
          })()}
          <button
            type="button"
            onClick={handleGenerate}
            disabled={genMutation.isPending}
            className="flex w-fit items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text hover:border-text-muted disabled:opacity-40"
          >
            {genMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            Regenerate
          </button>
          {genMutation.isError && <ErrorLine error={genMutation.error} />}
        </div>
      )}
    </section>
  )
}

// --- job-result polling helper ------------------------------------------

// useJobPoll tracks a queued job id and polls GET /api/jobs/{id} every 2s
// while it's queued/running, stopping once it lands on a terminal state —
// so callers can render the job's typed result (pull/cleanup/reprocess
// counts) once it's actually available, not just "queued". onSettled fires
// exactly once per job id, the moment it first lands on 'done'/'failed' —
// used to invalidate the dashcam list/status queries once the job that
// changed them has actually finished, not just been enqueued (enqueueing
// tells you nothing new about pull counts / reachability / last_pull_at).
function useJobPoll(jobId: number | null, onSettled?: (job: Job) => void) {
  const query = useQuery({
    queryKey: ['job', jobId],
    queryFn: () => getJob(jobId!),
    enabled: jobId != null,
    refetchInterval: (query) => {
      const state = query.state.data?.state
      return state === 'queued' || state === 'running' ? 2000 : false
    },
  })
  const state = query.data?.state
  useEffect(() => {
    if (query.data && (state === 'done' || state === 'failed')) {
      onSettled?.(query.data)
    }
    // Only re-fire when this job id's terminal state is first reached —
    // onSettled is often a fresh closure every render and must not
    // re-trigger the invalidation it causes on every unrelated re-render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobId, state])
  return query
}

// --- dashcam row: actions + status --------------------------------------

function ReachabilityBadge({ status }: { status?: DashcamStatus }) {
  if (!status) return <span className="rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide bg-surface-2 text-text-muted">Unknown</span>
  if (status.reachable) {
    return <span className="rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide bg-success/10 text-success">Reachable</span>
  }
  return (
    <span
      title={status.error_code ?? undefined}
      className="rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide bg-danger/10 text-danger"
    >
      Unreachable
    </span>
  )
}

function ReprocessForm({ dashcamId, onQueued }: { dashcamId: number; onQueued: (jobId: number) => void }) {
  const [mode, setMode] = useState<'camera_date' | 'segment'>('camera_date')
  const [camera, setCamera] = useState<'front' | 'rear'>('front')
  const [date, setDate] = useState('')
  const [segment, setSegment] = useState('')
  const [onlyMissing, setOnlyMissing] = useState(true)
  const [skipFrames, setSkipFrames] = useState(false)

  const mutation = useMutation({
    mutationFn: (params: ReprocessParams) => reprocessDashcam(dashcamId, params),
    onSuccess: (result) => onQueued(result.job.id),
  })

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const params: ReprocessParams = { only_missing: onlyMissing, skip_frames: skipFrames }
    if (mode === 'camera_date') {
      params.camera = camera
      params.date = date
    } else {
      params.segment = segment
    }
    mutation.mutate(params)
  }

  const valid = mode === 'camera_date' ? date.length > 0 : segment.trim().length > 0

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-2 rounded-md border border-border bg-surface/40 p-2.5">
      <div className="flex items-center gap-3 text-[11px] text-text-muted">
        <label className="flex items-center gap-1">
          <input type="radio" checked={mode === 'camera_date'} onChange={() => setMode('camera_date')} />
          Camera + date
        </label>
        <label className="flex items-center gap-1">
          <input type="radio" checked={mode === 'segment'} onChange={() => setMode('segment')} />
          Segment path
        </label>
      </div>
      {mode === 'camera_date' ? (
        <div className="flex items-center gap-2">
          <select value={camera} onChange={(e) => setCamera(e.target.value as 'front' | 'rear')} className={inputClass}>
            <option value="front">Front</option>
            <option value="rear">Rear</option>
          </select>
          <input type="date" value={date} onChange={(e) => setDate(e.target.value)} className={inputClass} />
        </div>
      ) : (
        <input
          type="text"
          value={segment}
          onChange={(e) => setSegment(e.target.value)}
          placeholder="front/2026-09-10/12-00-00.mp4 or just the filename"
          className={inputClass}
        />
      )}
      <div className="flex items-center gap-3 text-[11px] text-text-muted">
        <label className="flex items-center gap-1.5">
          <input type="checkbox" checked={onlyMissing} onChange={(e) => setOnlyMissing(e.target.checked)} />
          Only missing detections
        </label>
        <label className="flex items-center gap-1.5">
          <input type="checkbox" checked={skipFrames} onChange={(e) => setSkipFrames(e.target.checked)} />
          Skip frame extraction
        </label>
      </div>
      <button
        type="submit"
        disabled={!valid || mutation.isPending}
        className="flex w-fit items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 disabled:cursor-not-allowed disabled:opacity-40"
      >
        {mutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : 'Start reprocess'}
      </button>
      {mutation.isError && <ErrorLine error={mutation.error} />}
    </form>
  )
}

function DashcamRow({ dashcam, onEdit }: { dashcam: Dashcam; onEdit: (d: Dashcam) => void }) {
  const queryClient = useQueryClient()
  const [showReprocess, setShowReprocess] = useState(false)
  const [pullJobId, setPullJobId] = useState<number | null>(null)
  const [cleanupPreviewJobId, setCleanupPreviewJobId] = useState<number | null>(null)
  const [cleanupApplyJobId, setCleanupApplyJobId] = useState<number | null>(null)
  const [reprocessJobId, setReprocessJobId] = useState<number | null>(null)

  const statusQuery = useQuery({
    queryKey: ['dashcam-status', dashcam.id],
    queryFn: () => getDashcamStatus(dashcam.id),
    refetchInterval: (query) => {
      const state = query.state.data?.reprocess?.state
      return state === 'starting' || state === 'running' ? 5000 : 30000
    },
  })

  const testMutation = useMutation({ mutationFn: () => testDashcam(dashcam.id) })
  const pullMutation = useMutation({
    mutationFn: (full: boolean) => pullDashcam(dashcam.id, full),
    onSuccess: (result) => setPullJobId(result.job.id),
  })
  const cleanupPreviewMutation = useMutation({
    mutationFn: () => cleanupDashcam(dashcam.id, true),
    onSuccess: (result) => setCleanupPreviewJobId(result.job.id),
  })
  const cleanupApplyMutation = useMutation({
    mutationFn: () => cleanupDashcam(dashcam.id, false),
    onSuccess: (result) => setCleanupApplyJobId(result.job.id),
  })
  const deleteMutation = useMutation({
    mutationFn: () => deleteDashcam(dashcam.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['dashcams'] }),
  })

  // invalidateDashcamData: re-fetch the list (last_pull_at, status) and
  // this row's /status (reachable, pull counts, lease/reprocess) once a
  // job that can have changed either has actually finished — enqueueing
  // tells the UI nothing new, only completion does.
  function invalidateDashcamData() {
    queryClient.invalidateQueries({ queryKey: ['dashcams'] })
    queryClient.invalidateQueries({ queryKey: ['dashcam-status', dashcam.id] })
  }

  const pullJob = useJobPoll(pullJobId, invalidateDashcamData)
  const cleanupPreviewJob = useJobPoll(cleanupPreviewJobId)
  const cleanupApplyJob = useJobPoll(cleanupApplyJobId, (job) => {
    invalidateDashcamData()
    // The dry-run candidate count that unlocked the "Delete N from
    // dashcam" button no longer reflects reality once an apply has run
    // (successfully or not) — clear it so the button doesn't linger with
    // a stale count a reader could mistake for still-accurate.
    if (job.state === 'done') setCleanupPreviewJobId(null)
  })
  const reprocessJob = useJobPoll(reprocessJobId, invalidateDashcamData)

  const cleanupCandidates = cleanupPreviewJob.data?.state === 'done'
    ? (cleanupPreviewJob.data.result as DashcamCleanupResult | undefined)?.candidates
    : undefined

  function confirmDelete() {
    if (!confirm(`Remove dashcam "${dashcam.label}"? This only forgets its configuration here — nothing on the dashcam or its pulled footage is touched.`)) return
    deleteMutation.mutate()
  }

  function handleApplyCleanup() {
    if (!confirm(`Delete ${cleanupCandidates ?? 'the'} verified segment(s) off the dashcam "${dashcam.label}"? This permanently removes them from the recorder's storage.`)) return
    cleanupApplyMutation.mutate()
  }

  const status = statusQuery.data
  const reprocessState = status?.reprocess?.state

  return (
    <div className="flex flex-col gap-2.5 rounded-lg border border-border bg-bg/60 p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-xs">
          <span className="font-medium text-text">{dashcam.label}</span>
          <ReachabilityBadge status={status} />
          <span className="font-mono text-text-muted">{dashcam.base_url}</span>
          <span className="text-text-muted">→ {dashcam.target_share_label || `share #${dashcam.target_share_id}`}</span>
        </div>
        <div className="flex items-center gap-1">
          <button
            type="button"
            onClick={() => testMutation.mutate()}
            disabled={testMutation.isPending}
            className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text disabled:opacity-50"
          >
            {testMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : 'Test'}
          </button>
          <button
            type="button"
            onClick={() => onEdit(dashcam)}
            className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text"
          >
            <Pencil className="size-3.5" />
          </button>
          <button
            type="button"
            onClick={confirmDelete}
            className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-danger"
          >
            <Trash2 className="size-3.5" />
          </button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-text-muted">
        <span>Last pull: {dashcam.last_pull_at ? new Date(dashcam.last_pull_at).toLocaleString() : 'never'}</span>
        {status && (
          <span>
            {status.pulls.verified} verified, {status.pulls.pending_cleanup} pending cleanup
          </span>
        )}
        {status?.lease?.active && (
          <span>Maintenance lease active ({Math.max(0, Math.round(status.lease.remaining_s))}s remaining)</span>
        )}
      </div>

      {testMutation.data && (
        <p className={`text-[11px] ${testMutation.data.ok ? 'text-success' : 'text-danger'}`}>
          {testMutation.data.ok
            ? `Reachable. Skew ${testMutation.data.skew_s.toFixed(2)}s.`
            : `Failed: ${testMutation.data.error_code}`}
        </p>
      )}
      {testMutation.isError && <ErrorLine error={testMutation.error} />}

      {reprocessState && (reprocessState === 'starting' || reprocessState === 'running') && (
        <div className="rounded-md border border-accent/30 bg-accent/5 px-2.5 py-1.5 text-[11px] text-text">
          Reprocessing {status?.reprocess?.camera} {status?.reprocess?.date}: {status?.reprocess?.processed ?? 0}/
          {status?.reprocess?.total_segments ?? 0} segments
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2 border-t border-border pt-2">
        <button
          type="button"
          onClick={() => pullMutation.mutate(false)}
          disabled={pullMutation.isPending}
          className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text hover:border-text-muted disabled:opacity-40"
        >
          {pullMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : 'Pull now'}
        </button>
        <button
          type="button"
          onClick={() => pullMutation.mutate(true)}
          disabled={pullMutation.isPending}
          className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text hover:border-text-muted disabled:opacity-40"
        >
          Full pull
        </button>
        <button
          type="button"
          onClick={() => cleanupPreviewMutation.mutate()}
          disabled={cleanupPreviewMutation.isPending}
          className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text hover:border-text-muted disabled:opacity-40"
        >
          {cleanupPreviewMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : 'Cleanup (dry run)'}
        </button>
        {cleanupCandidates !== undefined && cleanupCandidates > 0 && (
          <button
            type="button"
            onClick={handleApplyCleanup}
            disabled={cleanupApplyMutation.isPending}
            className="flex items-center gap-1.5 rounded-md border border-danger/40 bg-danger/10 px-2.5 py-1.5 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-40"
          >
            {cleanupApplyMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : `Delete ${cleanupCandidates} from dashcam`}
          </button>
        )}
        <button
          type="button"
          onClick={() => setShowReprocess((v) => !v)}
          className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text hover:border-text-muted"
        >
          Reprocess
        </button>
      </div>

      {pullJob.data && (
        <div className="text-[11px]">
          {pullJob.data.state === 'done' ? (
            (() => {
              const r = pullJob.data.result as DashcamPullResult | undefined
              if (!r) return <span className="text-text-muted">Done.</span>
              return (
                <span className="text-text-muted">
                  Pulled {r.pulled}, skipped {r.skipped} ({formatBytes(r.bytes)}). Cursor: {r.cursor || '—'}.{' '}
                  {r.scan_job_id > 0 && (
                    <Link to="/system/jobs" className="text-accent hover:underline">
                      View scan job #{r.scan_job_id}
                    </Link>
                  )}
                </span>
              )
            })()
          ) : pullJob.data.state === 'failed' ? (
            <span className="text-danger">Pull failed: {pullJob.data.error}</span>
          ) : (
            <span className="text-text-muted">Pull {pullJob.data.state}… {pullJob.data.message}</span>
          )}
        </div>
      )}
      {cleanupPreviewJob.data?.state === 'done' && (
        <div className="text-[11px] text-text-muted">
          {(() => {
            const r = cleanupPreviewJob.data.result as DashcamCleanupResult | undefined
            if (!r) return null
            return `${r.candidates ?? 0} candidate(s) eligible for cleanup${r.local_mismatch ? `, ${r.local_mismatch} skipped (local mismatch)` : ''}.`
          })()}
        </div>
      )}
      {cleanupPreviewJob.data?.state === 'failed' && <span className="text-[11px] text-danger">Cleanup preview failed: {cleanupPreviewJob.data.error}</span>}
      {cleanupApplyJob.data?.state === 'done' && (
        <div className="flex flex-col gap-1 text-[11px] text-success">
          {(() => {
            const r = cleanupApplyJob.data.result as DashcamCleanupResult | undefined
            return (
              <>
                <span>
                  Deleted {r?.deleted ?? 0} segment(s) off the dashcam{r?.local_mismatch ? `, ${r.local_mismatch} skipped (local mismatch)` : ''}.
                </span>
                {r?.skipped && r.skipped.length > 0 && (
                  <ul className="flex flex-col gap-0.5 text-warn">
                    {r.skipped.map((s) => (
                      <li key={s.path}>
                        <span className="font-mono">{s.path}</span>: {s.reason}
                      </li>
                    ))}
                  </ul>
                )}
              </>
            )
          })()}
        </div>
      )}
      {cleanupApplyJob.data?.state === 'failed' && <span className="text-[11px] text-danger">Cleanup failed: {cleanupApplyJob.data.error}</span>}

      <div className={showReprocess ? '' : 'hidden'}>
        <ReprocessForm dashcamId={dashcam.id} onQueued={setReprocessJobId} />
      </div>
      {reprocessJob.data?.state === 'done' && (
        <div className="text-[11px] text-success">
          {(() => {
            const r = reprocessJob.data.result as DashcamReprocessResult | undefined
            return `Reprocessed ${r?.segments_reprocessed ?? 0} segment(s), refreshed ${r?.sidecars_refreshed ?? 0} sidecar(s).`
          })()}
        </div>
      )}
      {reprocessJob.data?.state === 'failed' && <span className="text-[11px] text-danger">Reprocess failed: {reprocessJob.data.error}</span>}
    </div>
  )
}

// --- add/edit dialog -----------------------------------------------------

const emptyForm: DashcamInput = { label: '', base_url: '', target_share_id: undefined }

function DashcamDialog({
  editing,
  onClose,
}: {
  editing: Dashcam | null
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [form, setForm] = useState<DashcamInput>(
    editing ? { label: editing.label, base_url: editing.base_url, target_share_id: editing.target_share_id } : emptyForm,
  )
  const { data: shares } = useQuery({ queryKey: ['shares'], queryFn: getShares })

  const createMutation = useMutation({
    mutationFn: (input: DashcamInput) => createDashcam(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['dashcams'] })
      onClose()
    },
  })
  const updateMutation = useMutation({
    mutationFn: (input: DashcamInput) => updateDashcam(editing!.id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['dashcams'] })
      onClose()
    },
  })

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (editing) updateMutation.mutate(form)
    else createMutation.mutate(form)
  }

  const saving = createMutation.isPending || updateMutation.isPending
  const saveError = (createMutation.error ?? updateMutation.error) as Error | null

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="w-full max-w-md rounded-lg border border-border bg-bg p-4">
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold text-text">{editing ? 'Edit dashcam' : 'Add dashcam'}</h2>
          <button type="button" onClick={onClose} className="text-text-muted hover:text-text">
            <X className="size-4" />
          </button>
        </div>
        <form onSubmit={handleSubmit} className="flex flex-col gap-2.5">
          <Field label="Label">
            <input
              type="text"
              required
              value={form.label ?? ''}
              onChange={(e) => setForm((f) => ({ ...f, label: e.target.value }))}
              className={inputClass}
              placeholder="Garage OPi5"
            />
          </Field>
          <Field label="Base URL">
            <input
              type="text"
              required
              value={form.base_url ?? ''}
              onChange={(e) => setForm((f) => ({ ...f, base_url: e.target.value }))}
              className={inputClass}
              placeholder="http://dashcam.local:8080"
            />
          </Field>
          <Field label="Target share">
            <select
              required
              value={form.target_share_id ?? ''}
              onChange={(e) => setForm((f) => ({ ...f, target_share_id: Number(e.target.value) }))}
              className={inputClass}
            >
              <option value="" disabled>
                Select a share…
              </option>
              {shares?.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}
                </option>
              ))}
            </select>
          </Field>

          {saveError && <ErrorLine error={saveError} />}

          <div className="mt-2 flex justify-end gap-2">
            <button type="button" onClick={onClose} className="rounded-md px-3 py-1.5 text-xs text-text-muted hover:bg-surface">
              Cancel
            </button>
            <button
              type="submit"
              disabled={saving}
              className="rounded-md bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
            >
              {saving ? 'Saving…' : editing ? 'Save' : 'Add'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

// --- page ------------------------------------------------------------------

export default function Dashcams() {
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Dashcam | null>(null)

  const { data: dashcams, isLoading, isError } = useQuery({
    queryKey: ['dashcams'],
    queryFn: getDashcams,
    refetchInterval: 30000,
  })

  function openCreate() {
    setEditing(null)
    setDialogOpen(true)
  }

  function openEdit(d: Dashcam) {
    setEditing(d)
    setDialogOpen(true)
  }

  function closeDialog() {
    setDialogOpen(false)
    setEditing(null)
  }

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between">
        <h1 className="flex items-center gap-1.5 text-sm font-semibold text-text">
          <SatelliteDish className="size-4" />
          Dashcams
        </h1>
        <button
          type="button"
          onClick={openCreate}
          className="flex items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/20"
        >
          <Plus className="size-3.5" /> Add dashcam
        </button>
      </div>

      <KeyCard />

      {isLoading && <div className="text-xs text-text-muted">Loading…</div>}
      {isError && <div className="text-xs text-danger">Failed to load dashcams.</div>}
      {!isLoading && !isError && (dashcams?.length ?? 0) === 0 && (
        <div className="text-xs text-text-muted">No dashcams configured yet.</div>
      )}

      <div className="flex flex-col gap-2">
        {dashcams?.map((d) => (
          <DashcamRow key={d.id} dashcam={d} onEdit={openEdit} />
        ))}
      </div>

      {dialogOpen && <DashcamDialog editing={editing} onClose={closeDialog} />}
    </div>
  )
}
