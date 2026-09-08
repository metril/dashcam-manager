// Tiny typed fetch wrapper for the M7 jobs API (GET/POST/DELETE
// /api/jobs*, POST /api/sources/scan). Kept separate from lib/api.ts to
// avoid touching that file while it's under concurrent edit elsewhere;
// mirrors its ApiError-throwing shape.
export interface Job {
  id: number
  type: string
  state: 'queued' | 'running' | 'done' | 'failed' | 'cancelled'
  priority: number
  params?: unknown
  progress: number
  message?: string
  result?: unknown
  error?: string
  created_at: string
  started_at?: string | null
  finished_at?: string | null
  cancel_requested: boolean
}

export class JobsApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'JobsApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init)
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body && typeof body.error === 'string') message = body.error
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new JobsApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getJobs(state?: string): Promise<Job[]> {
  const params = new URLSearchParams()
  if (state) params.set('state', state)
  const qs = params.toString()
  return request(`/api/jobs${qs ? `?${qs}` : ''}`)
}

export function getJob(id: number): Promise<Job> {
  return request(`/api/jobs/${id}`)
}

export function cancelJob(id: number): Promise<{ status: string; id: number }> {
  return request(`/api/jobs/${id}/cancel`, { method: 'POST' })
}

export function deleteJob(id: number): Promise<void> {
  return request(`/api/jobs/${id}`, { method: 'DELETE' })
}

export function scanSource(sourceId: number, full = false): Promise<{ status: string; job?: Job }> {
  return request('/api/sources/scan', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ source_id: sourceId, full }),
  })
}
