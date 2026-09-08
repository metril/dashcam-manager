// useEvents subscribes to the SSE job event stream (GET /api/events, see
// cmd/api/handlers_jobs.go + broker.go) and keeps a live map of
// job id -> latest JobEvent for as long as the component using it stays
// mounted. The browser's EventSource retries the connection itself (using
// the server's `retry: 3000` hint), so callers don't need reconnect logic.
import { useEffect, useState } from 'react'

export interface JobEvent {
  id: number
  type: string
  state: 'queued' | 'running' | 'done' | 'failed' | 'cancelled'
  progress: number
  message?: string
}

export function useEvents(): Record<number, JobEvent> {
  const [events, setEvents] = useState<Record<number, JobEvent>>({})

  useEffect(() => {
    const source = new EventSource('/api/events')

    const onJob = (e: MessageEvent<string>) => {
      try {
        const job = JSON.parse(e.data) as JobEvent
        setEvents((prev) => ({ ...prev, [job.id]: job }))
      } catch {
        // Ignore a malformed event payload rather than tearing down the
        // whole stream over it.
      }
    }

    source.addEventListener('job', onJob)
    return () => {
      source.removeEventListener('job', onJob)
      source.close()
    }
  }, [])

  return events
}
