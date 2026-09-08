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

  const activeCount = useMemo(() => {
    if (!jobs) return 0
    let n = 0
    for (const job of jobs) {
      const state = liveEvents[job.id]?.state ?? job.state
      if (state === 'queued' || state === 'running') n++
    }
    return n
  }, [jobs, liveEvents])

  if (activeCount === 0) return null

  return (
    <Link
      to="/jobs"
      className="flex items-center gap-1.5 rounded-md border border-amber-500/20 bg-amber-500/10 px-2.5 py-1 text-[11px] font-medium text-amber-400 transition-colors hover:bg-amber-500/20"
    >
      <Loader2 className="size-3 animate-spin" strokeWidth={2.5} />
      {activeCount} running
    </Link>
  )
}
