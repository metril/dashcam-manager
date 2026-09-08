// M7 Jobs page: lists jobs from the persistent queue (internal/jobs), with
// a state filter, per-job progress bars, cancel/dismiss actions, and live
// updates via SSE (useEvents) layered on top of a polled GET /api/jobs as
// a fallback in case an event is missed.
import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, XCircle } from 'lucide-react'
import { cancelJob, deleteJob, getJobs, type Job } from '../lib/jobsApi'
import { useEvents } from '../hooks/useEvents'

const STATE_FILTERS = ['all', 'queued', 'running', 'done', 'failed', 'cancelled'] as const
type StateFilter = (typeof STATE_FILTERS)[number]

const STATE_STYLES: Record<Job['state'], string> = {
  queued: 'bg-neutral-800 text-neutral-400',
  running: 'bg-amber-500/10 text-amber-400',
  done: 'bg-emerald-500/10 text-emerald-400',
  failed: 'bg-red-500/10 text-red-400',
  cancelled: 'bg-neutral-800 text-neutral-500',
}

const BAR_STYLES: Record<Job['state'], string> = {
  queued: 'bg-neutral-700',
  running: 'bg-amber-500',
  done: 'bg-emerald-500',
  failed: 'bg-red-500',
  cancelled: 'bg-neutral-700',
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
        <h1 className="text-sm font-semibold text-neutral-200">Jobs</h1>
        <div className="flex gap-1">
          {STATE_FILTERS.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => setFilter(s)}
              className={[
                'rounded-md px-2.5 py-1 text-xs font-medium capitalize transition-colors',
                filter === s
                  ? 'bg-amber-500/10 text-amber-400'
                  : 'text-neutral-500 hover:bg-neutral-900 hover:text-neutral-200',
              ].join(' ')}
            >
              {s}
            </button>
          ))}
        </div>
      </div>

      {isLoading && <div className="text-xs text-neutral-600">Loading…</div>}
      {isError && <div className="text-xs text-red-400">Failed to load jobs.</div>}
      {!isLoading && !isError && filtered.length === 0 && (
        <div className="text-xs text-neutral-600">
          No jobs{filter !== 'all' ? ` in state "${filter}"` : ''}.
        </div>
      )}

      <div className="flex flex-col gap-2">
        {filtered.map((job) => (
          <div key={job.id} className="rounded-lg border border-neutral-900 bg-neutral-950/60 p-3">
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2 text-xs">
                <span className="font-mono text-neutral-600">#{job.id}</span>
                <span className="font-medium text-neutral-200">{job.type}</span>
                <span
                  className={[
                    'rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide',
                    STATE_STYLES[job.state],
                  ].join(' ')}
                >
                  {job.state}
                </span>
              </div>
              <div className="flex items-center gap-1.5">
                {(job.state === 'queued' || job.state === 'running') && (
                  <button
                    type="button"
                    onClick={() => handleCancel(job.id)}
                    className="flex items-center gap-1 rounded-md px-2 py-1 text-[11px] text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-red-400"
                  >
                    <Ban className="size-3" /> Cancel
                  </button>
                )}
                {(job.state === 'done' || job.state === 'failed' || job.state === 'cancelled') && (
                  <button
                    type="button"
                    onClick={() => handleDismiss(job.id)}
                    className="flex items-center gap-1 rounded-md px-2 py-1 text-[11px] text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
                  >
                    <XCircle className="size-3" /> Dismiss
                  </button>
                )}
              </div>
            </div>

            <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-neutral-900">
              <div
                className={['h-full rounded-full transition-all', BAR_STYLES[job.state]].join(' ')}
                style={{ width: `${Math.round(job.progress * 100)}%` }}
              />
            </div>

            {(job.message || job.error) && (
              <div className={['mt-1.5 text-[11px]', job.error ? 'text-red-400' : 'text-neutral-500'].join(' ')}>
                {job.error ?? job.message}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}
