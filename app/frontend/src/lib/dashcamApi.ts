// Tiny typed fetch wrapper for the dashcam API (GET/POST/PATCH/DELETE
// /api/dashcams*) — see cmd/api/handlers_dashcam.go for the exact
// route/response shapes this mirrors. Kept separate from lib/api.ts to
// avoid touching that file while it's under concurrent edit elsewhere;
// mirrors its ApiError-throwing shape (see lib/jobsApi.ts / lib/sharesApi.ts
// for the same pattern).
import type { Job } from './jobsApi'

export interface Dashcam {
  id: number
  label: string
  base_url: string
  target_share_id: number
  target_share_label: string
  pull_cursor: string | null
  last_pull_at: string | null
  status: string
}

export interface DashcamInput {
  label?: string
  base_url?: string
  target_share_id?: number
}

// LeaseStatus mirrors internal/dashcam.LeaseStatus — the recorder's
// current maintenance-lease state, reported by both /test and /status.
export interface LeaseStatus {
  active: boolean
  lease_id: string
  expires_at: string
  remaining_s: number
  reason: string
}

// DashcamTestErrorCode: stable machine codes /test and /status report on
// failure ("" when ok/reachable). Kept as a plain string type (rather than
// a union) since the server may add codes over time and an unrecognized
// one should still render rather than fail a type check.
export type DashcamTestErrorCode = 'unreachable' | 'clock_skew' | 'key_not_configured' | 'not_found' | string

export interface DashcamTestResult {
  ok: boolean
  error_code: DashcamTestErrorCode
  server_time?: string
  skew_s: number
  lease?: LeaseStatus
}

export interface DashcamReprocessStatus {
  state: 'idle' | 'starting' | 'running' | 'done' | 'error'
  camera?: string
  date?: string
  processed?: number
  total_segments?: number
  error?: string
}

export interface DashcamPullCounts {
  verified: number
  pending_cleanup: number
}

export interface DashcamStatus {
  reachable: boolean
  // error_code/lease/reprocess are always present keys on the response,
  // just possibly null (empty error_code / no lease-or-reprocess data
  // available, e.g. the dashcam is unreachable).
  error_code: DashcamTestErrorCode | null
  lease: LeaseStatus | null
  reprocess: DashcamReprocessStatus | null
  last_pull_at: string | null
  pull_cursor: string | null
  pulls: DashcamPullCounts
}

export interface DashcamKey {
  configured: boolean
  key_id?: string
  public_key_b64?: string
  config_snippet?: string
  // error_code is "key_unreadable" when a key is configured (key_id/
  // public_key_b64/config_snippet above are still populated) but its
  // private half can't be decrypted under the server's current
  // DASHCAM_SECRET_KEY — see cmd/api/handlers_dashcam.go's
  // dashcamKeyUnreadableResponse. Absent/"" otherwise.
  error_code?: 'key_unreadable' | string
}

// SkippedPath mirrors internal/dashcam.SkippedPath — one path the
// dashcam's own delete endpoint refused to remove, with why.
export interface SkippedPath {
  path: string
  reason: string
}

export interface DashcamPullResult {
  pulled: number
  skipped: number
  bytes: number
  cursor: string
  scan_job_id: number
}

export interface DashcamCleanupResult {
  candidates?: number
  bytes?: number
  local_mismatch: number
  deleted?: number
  skipped?: SkippedPath[]
}

export interface DashcamReprocessResult {
  segments_reprocessed: number
  sidecars_refreshed: number
  scan_job_id: number
  total_objects: number
  total_plates: number
  total_vehicles: number
  total_track_events: number
}

export interface ReprocessParams {
  camera?: string
  date?: string
  path?: string
  segment?: string
  only_missing?: boolean
  skip_frames?: boolean
}

export class DashcamApiError extends Error {
  status: number
  // code is the error body's machine-readable "code" field (e.g.
  // "secret_key_missing" — see cmd/api/handlers_settings.go's
  // writeJSONErrorCode), when the server included one. undefined for an
  // error body that only carries "error" (the older, still-common shape).
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.name = 'DashcamApiError'
    this.status = status
    this.code = code
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // CSRF hardening (final-fix-wave item 5): the server requires
  // Content-Type: application/json on every mutating request, so make
  // sure every non-GET call carries it even when the caller didn't pass a
  // body (e.g. a bare POST with no JSON payload) — callers that already
  // set an explicit Content-Type keep it.
  const method = (init?.method ?? 'GET').toUpperCase()
  const withCSRFHeaders =
    method === 'GET'
      ? init
      : { ...init, headers: { 'Content-Type': 'application/json', ...init?.headers } }
  const res = await fetch(path, withCSRFHeaders)
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    let code: string | undefined
    try {
      const body = await res.json()
      if (body && typeof body.error === 'string') message = body.error
      if (body && typeof body.code === 'string') code = body.code
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new DashcamApiError(res.status, message, code)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getDashcams(): Promise<Dashcam[]> {
  return request('/api/dashcams')
}

export function createDashcam(input: DashcamInput): Promise<Dashcam> {
  return request('/api/dashcams', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function updateDashcam(id: number, input: DashcamInput): Promise<Dashcam> {
  return request(`/api/dashcams/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function deleteDashcam(id: number): Promise<void> {
  return request(`/api/dashcams/${id}`, { method: 'DELETE' })
}

// testDashcam sends an empty JSON object body: csrfProtect (cmd/api/csrf.go)
// requires Content-Type: application/json on every mutating request
// regardless of whether the handler itself reads a body, so every POST
// here — even ones with nothing to say — carries one explicitly rather
// than relying on the request() wrapper's automatic header alone.
export function testDashcam(id: number): Promise<DashcamTestResult> {
  return request(`/api/dashcams/${id}/test`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({}),
  })
}

export function pullDashcam(id: number, full = false): Promise<{ status: string; job: Job }> {
  return request(`/api/dashcams/${id}/pull`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ full }),
  })
}

export function cleanupDashcam(id: number, dryRun: boolean): Promise<{ status: string; job: Job }> {
  return request(`/api/dashcams/${id}/cleanup`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ dry_run: dryRun }),
  })
}

export function reprocessDashcam(id: number, params: ReprocessParams): Promise<{ status: string; job: Job }> {
  return request(`/api/dashcams/${id}/reprocess`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(params),
  })
}

export function getDashcamStatus(id: number): Promise<DashcamStatus> {
  return request(`/api/dashcams/${id}/status`)
}

export function getDashcamKey(): Promise<DashcamKey> {
  return request('/api/dashcams/key')
}

export function generateDashcamKey(force = false): Promise<DashcamKey> {
  return request('/api/dashcams/key/generate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ force }),
  })
}
