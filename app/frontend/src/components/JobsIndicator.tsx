// Global top-bar indicator (M7): shows a small live count of queued+running
// jobs, driven by a polled GET /api/jobs (refetchInterval as a fallback)
// patched live by SSE job events (useEvents) so it updates immediately
// rather than waiting for the next poll. Hidden entirely when nothing is
// active, so it doesn't add visual noise to every other page.
import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Loader2 } from 'lucide-react'
import { Link } from 'react-router-dom'
import { getJobs } from '../lib/jobsApi'
import { useEvents } from '../hooks/useEvents'

export default function JobsIndicator() {
  const liveEvents = useEvents()

  const { data: jobs } = useQuery({
    queryKey: ['jobs', 'indicator'],
    queryFn: () => getJobs(),
    refetchInterval: 15000,
  })

  // Union the polled list with live event ids: a job enqueued after the
  // last poll (or one whose completion hasn't been polled yet) only shows
  // up in liveEvents until the next refetch, and must still count.
  const activeCount = useMemo(() => {
    const ids = new Set<number>()
    for (const job of jobs ?? []) ids.add(job.id)
    for (const id of Object.keys(liveEvents)) ids.add(Number(id))

    let n = 0
    for (const id of ids) {
      const fetchedState = jobs?.find((j) => j.id === id)?.state
      const state = liveEvents[id]?.state ?? fetchedState
      if (state === 'queued' || state === 'running') n++
    }
    return n
  }, [jobs, liveEvents])

  if (activeCount === 0) return null

  return (
    <Link
      to="/system/jobs"
      className="flex items-center gap-1.5 rounded-md border border-accent/20 bg-accent/10 px-2.5 py-1 text-xs font-medium text-accent transition-colors hover:bg-accent/20"
    >
      <Loader2 className="size-3 animate-spin" strokeWidth={2.5} />
      {activeCount} running
    </Link>
  )
}
