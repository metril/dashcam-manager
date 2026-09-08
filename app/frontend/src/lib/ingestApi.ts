// Tiny typed fetch wrapper for the M10 ingest API (GET /api/ssd/status,
// POST /api/ingest, POST /api/ingest/cleanup). Kept separate from
// lib/api.ts to avoid touching that file while it's under concurrent edit
// elsewhere; mirrors its ApiError-throwing shape (see lib/jobsApi.ts /
// lib/sharesApi.ts for the same pattern).
import type { Job } from './jobsApi'

export interface IngestTripSummary {
  trip_id: number
  trip_key: string
  started_at: string
  segment_count: number
  bytes: number
  ingested_count: number
  verified_count: number
}

export interface SSDStatus {
  path: string
  present: boolean
  mounted_rw: boolean
  total_bytes?: number
  free_bytes?: number
  segments_total: number
  segments_ingested: number
  segments_not_ingested: number
  source_id?: number
  trips?: IngestTripSummary[]
}

export interface IngestInput {
  source_id: number
  target_share_id: number
  trip_ids?: number[]
  verify?: 'sha256' | 'size'
  delete_after?: boolean
}

export interface CleanupInput {
  source_id?: number
  trip_ids?: number[]
  dry_run?: boolean
}

export interface CleanupItem {
  segment_id: number
  rel_path: string
  bytes: number
}

export interface CleanupStats {
  dry_run: boolean
  items: CleanupItem[]
  segments_deleted: number
  bytes_deleted: number
  segments_skipped: number
  dirs_removed: number
}

export class IngestApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'IngestApiError'
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
    throw new IngestApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getSSDStatus(targetShareId?: number): Promise<SSDStatus> {
  const params = new URLSearchParams()
  if (targetShareId) params.set('target_share_id', String(targetShareId))
  const qs = params.toString()
  return request(`/api/ssd/status${qs ? `?${qs}` : ''}`)
}

export function startIngest(input: IngestInput): Promise<{ status: string; job?: Job }> {
  return request('/api/ingest', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function startCleanup(input: CleanupInput): Promise<CleanupStats | { status: string; job?: Job }> {
  return request('/api/ingest/cleanup', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}
