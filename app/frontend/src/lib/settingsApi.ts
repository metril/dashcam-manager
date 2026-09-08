// Tiny typed fetch wrapper for the settings API (GET/PUT /api/settings).
// Kept separate from lib/api.ts to avoid touching that file while it's
// under concurrent edit elsewhere; mirrors its ApiError-throwing shape
// (see lib/jobsApi.ts / lib/sharesApi.ts for the same pattern).
export interface Settings {
  ssd_path: string
  qsv_device: string
  max_ffmpeg_jobs: number
  workers: number
  timezone?: string
  hw_mode: 'auto' | 'qsv' | 'vaapi' | 'software'
}

export class SettingsApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'SettingsApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // CSRF hardening (final-fix-wave item 5): the server requires
  // Content-Type: application/json on every mutating request, so make
  // sure every non-GET call carries it even when the caller didn't pass a
  // body (e.g. a bare POST /mount or DELETE with no JSON payload) —
  // callers that already set an explicit Content-Type keep it.
  const method = (init?.method ?? 'GET').toUpperCase()
  const withCSRFHeaders =
    method === 'GET'
      ? init
      : { ...init, headers: { 'Content-Type': 'application/json', ...init?.headers } }
  const res = await fetch(path, withCSRFHeaders)
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body && typeof body.error === 'string') message = body.error
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new SettingsApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getSettings(): Promise<Settings> {
  return request('/api/settings')
}

export function updateSettings(patch: Partial<Settings>): Promise<Settings> {
  return request('/api/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  })
}
