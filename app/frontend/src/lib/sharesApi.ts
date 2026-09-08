// Tiny typed fetch wrapper for the M9 shares API (GET/POST/PATCH/DELETE
// /api/shares*). Kept separate from lib/api.ts to avoid touching that file
// while it's under concurrent edit elsewhere; mirrors its ApiError-throwing
// shape (see lib/jobsApi.ts for the same pattern).
export interface Share {
  id: number
  name: string
  server: string
  share: string
  subpath: string
  username: string
  domain: string
  mount_point: string
  options: string
  enabled: boolean
  status: string
  last_mounted_at?: string | null
  has_password: boolean
  source_id?: number
  free_bytes?: number
  total_bytes?: number
}

export interface ShareInput {
  name?: string
  server: string
  share: string
  subpath?: string
  username?: string
  password?: string
  domain?: string
  options?: string
  enabled?: boolean
}

export class SharesApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'SharesApiError'
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
    throw new SharesApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getShares(): Promise<Share[]> {
  return request('/api/shares')
}

export function createShare(input: ShareInput): Promise<Share> {
  return request('/api/shares', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function updateShare(id: number, input: ShareInput): Promise<Share> {
  return request(`/api/shares/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function deleteShare(id: number, opts?: { purgeIndex?: boolean }): Promise<void> {
  const qs = opts?.purgeIndex ? '?purge_index=1' : ''
  return request(`/api/shares/${id}${qs}`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
  })
}

export function testShare(id: number): Promise<{ ok: boolean; error?: string }> {
  return request(`/api/shares/${id}/test`, { method: 'POST' })
}

export function mountShare(id: number): Promise<Share> {
  return request(`/api/shares/${id}/mount`, { method: 'POST' })
}

export function unmountShare(id: number): Promise<Share> {
  return request(`/api/shares/${id}/unmount`, { method: 'POST' })
}
