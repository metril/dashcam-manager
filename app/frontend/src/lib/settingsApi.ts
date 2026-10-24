// Tiny typed fetch wrapper for the settings API (GET/PUT /api/settings).
// Kept separate from lib/api.ts to avoid touching that file while it's
// under concurrent edit elsewhere; mirrors its ApiError-throwing shape
// (see lib/jobsApi.ts / lib/sharesApi.ts for the same pattern).
export type HwMode = 'auto' | 'nvenc' | 'qsv' | 'vaapi' | 'software'

export interface Settings {
  ssd_path: string
  ssd_uuid: string
  qsv_device: string
  cuda_device: string
  local_root: string
  ffmpeg_bin: string
  ffprobe_bin: string
  public_url: string
  max_ffmpeg_jobs: number
  workers: number
  timezone: string
  hw_mode: HwMode
  // dashcam_poll_interval_s: how often (seconds) the server auto-pulls
  // every configured dashcam; 0 disables the poll loop entirely.
  dashcam_poll_interval_s: number
  oidc_enabled: boolean
  oidc_issuer: string
  oidc_client_id: string
  oidc_groups_claim: string
  oidc_scopes: string[]
  oidc_allowed_emails: string[]
  oidc_allowed_groups: string[]
  // Read-only: the secret itself is write-only (oidc_client_secret on PUT).
  oidc_client_secret_set: boolean
  // Non-empty when the stored secret could not be decrypted; OIDC is forced off.
  oidc_secret_error: string
}

// Write-only key accepted by PUT but never returned.
export type SettingsPatch = Partial<Omit<Settings, 'oidc_client_secret_set' | 'oidc_secret_error'>> & {
  oidc_client_secret?: string
}

export interface SettingsUpdateResult {
  settings: Settings
  restart_required: string[]
  applied: string[]
  apply_errors: Record<string, string>
}

export interface SettingsMeta {
  bootstrap: { db: string; cache: string; addr: string }
  secret_key_source: 'env' | 'file' | 'generated'
  auth_disabled: boolean
  env_differs: string[]
  restart_required: string[]
  redirect_uri: string
}

export type DiscoverResult =
  | { ok: true; issuer: string; authorization_endpoint: string; end_session: boolean }
  | { ok: false; error: string }

export interface OIDCTestRequest {
  issuer: string
  client_id: string
  client_secret?: string
  scopes?: string[]
  allowed_emails?: string[]
  allowed_groups?: string[]
  groups_claim?: string
  public_url?: string
}

export class SettingsApiError extends Error {
  status: number
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.name = 'SettingsApiError'
    this.status = status
    this.code = code
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
    let code: string | undefined
    try {
      const body = await res.json()
      if (body && typeof body.error === 'string') message = body.error
      if (body && typeof body.code === 'string') code = body.code
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new SettingsApiError(res.status, message, code)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getSettings(): Promise<Settings> {
  return request('/api/settings')
}

export function updateSettings(patch: SettingsPatch): Promise<SettingsUpdateResult> {
  return request('/api/settings', { method: 'PUT', body: JSON.stringify(patch) })
}

export function getSettingsMeta(): Promise<SettingsMeta> {
  return request('/api/settings/meta')
}

export function discoverOIDC(issuer: string): Promise<DiscoverResult> {
  return request('/api/settings/oidc/discover', { method: 'POST', body: JSON.stringify({ issuer }) })
}

// testOIDC stages the pending config and returns the URL to open (popup or
// full redirect) to run the test sign-in.
export function testOIDC(req: OIDCTestRequest): Promise<{ login_url: string }> {
  return request('/api/settings/oidc/test', { method: 'POST', body: JSON.stringify(req) })
}
