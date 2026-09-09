// M7 Jobs page: lists jobs from the persistent queue (internal/jobs), with
// a state filter, per-job progress bars, cancel/dismiss actions, and live
// updates via SSE (useEvents) layered on top of a polled GET /api/jobs as
// a fallback in case an event is missed.
import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, XCircle } from 'lucide-react'
import { cancelJob, deleteJob, getJobs, type Job } from '../lib/jobsApi'
import { useEvents } from '../hooks/useEvents'
import { titleCase } from '../lib/format'

const STATE_FILTERS = ['all', 'queued', 'running', 'done', 'failed', 'cancelled'] as const
type StateFilter = (typeof STATE_FILTERS)[number]

const STATE_STYLES: Record<Job['state'], string> = {
  queued: 'bg-surface-2 text-text-muted',
  running: 'bg-accent/10 text-accent',
  done: 'bg-success/10 text-success',
  failed: 'bg-danger/10 text-danger',
  cancelled: 'bg-surface-2 text-text-muted',
}

const BAR_STYLES: Record<Job['state'], string> = {
  queued: 'bg-surface-2',
  running: 'bg-accent',
  done: 'bg-success',
  failed: 'bg-danger',
  cancelled: 'bg-surface-2',
}

export default function Jobs() {
  const [filter, setFilter] = useState<StateFilter>('all')
  const queryClient = useQueryClient()
  const liveEvents = useEvents()

  const {
    data: jobs,
    isLoading,
    isError,
  } = useQuery({
    queryKey: ['jobs'],
    queryFn: () => getJobs(),
    refetchInterval: 15000,
  })

  const merged = useMemo(() => {
    if (!jobs) return []
    return jobs
      .map((job) => {
        const live = liveEvents[job.id]
        if (!live) return job
        return { ...job, state: live.state, progress: live.progress, message: live.message ?? job.message }
      })
      .sort((a, b) => b.id - a.id)
  }, [jobs, liveEvents])

  const filtered = filter === 'all' ? merged : merged.filter((j) => j.state === filter)

  function invalidate() {
    queryClient.invalidateQueries({ queryKey: ['jobs'] })
  }

  async function handleCancel(id: number) {
    try {
      await cancelJob(id)
    } finally {
      invalidate()
    }
  }

  async function handleDismiss(id: number) {
    try {
      await deleteJob(id)
    } finally {
      invalidate()
    }
  }

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between">
        <h1 className="text-sm font-semibold text-text">Jobs</h1>
        <div className="flex gap-1">
          {STATE_FILTERS.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => setFilter(s)}
              className={[
                'rounded-md px-2.5 py-1 text-xs font-medium capitalize transition-colors',
                filter === s
                  ? 'bg-accent/10 text-accent'
                  : 'text-text-muted hover:bg-surface hover:text-text',
              ].join(' ')}
            >
              {s}
            </button>
          ))}
        </div>
      </div>

      {isLoading && <div className="text-xs text-text-muted">Loading…</div>}
      {isError && <div className="text-xs text-danger">Failed to load jobs.</div>}
      {!isLoading && !isError && filtered.length === 0 && (
        <div className="text-xs text-text-muted">
          No jobs{filter !== 'all' ? ` in state "${filter}"` : ''}.
        </div>
      )}

      <div className="flex flex-col gap-2">
        {filtered.map((job) => (
          <div key={job.id} className="rounded-lg border border-border bg-bg/60 p-3">
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2 text-xs">
                <span className="font-mono text-text-muted">#{job.id}</span>
                <span className="font-medium text-text">{titleCase(job.type)}</span>
                <span
                  className={[
                    'rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide',
                    STATE_STYLES[job.state],
                  ].join(' ')}
                >
                  {titleCase(job.state)}
                </span>
              </div>
              <div className="flex items-center gap-1.5">
                {(job.state === 'queued' || job.state === 'running') && (
                  <button
                    type="button"
                    onClick={() => handleCancel(job.id)}
                    className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-danger"
                  >
                    <Ban className="size-3" /> Cancel
                  </button>
                )}
                {(job.state === 'done' || job.state === 'failed' || job.state === 'cancelled') && (
                  <button
                    type="button"
                    onClick={() => handleDismiss(job.id)}
                    className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text"
                  >
                    <XCircle className="size-3" /> Dismiss
                  </button>
                )}
              </div>
            </div>

            <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface">
              <div
                className={['h-full rounded-full transition-all', BAR_STYLES[job.state]].join(' ')}
                style={{ width: `${Math.round(job.progress * 100)}%` }}
              />
            </div>

            {(job.message || job.error) && (
              <div className={['mt-1.5 text-xs', job.error ? 'text-danger' : 'text-text-muted'].join(' ')}>
                {job.error ?? job.message}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}
