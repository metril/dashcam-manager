// Tiny typed fetch wrapper for the local-storage plan's storage/duplicates
// API (GET/POST/PATCH/DELETE /api/storages*, GET /api/duplicates, POST
// /api/duplicates/refresh). Kept separate from lib/api.ts to avoid
// touching that file while it's under concurrent edit elsewhere; mirrors
// lib/sharesApi.ts/lib/exportsApi.ts's ApiError-throwing shape.
import type { Job } from './jobsApi'

// Storage is one persistent storage — every shares row (kind cifs|local)
// plus the synthetic "cache" pseudo-storage for the export cache
// directory (cmd/api/handlers_storages.go's storageView). The recorder's
// SSD is never included: it is not a storage (see the local-storage
// plan's design note), so this list never needs an "on_ssd" concept of
// its own.
export interface Storage {
  id: string
  kind: 'cifs' | 'local' | 'cache'
  label: string
  path: string
  status: string
  enabled: boolean
  free_bytes?: number
  total_bytes?: number
  segment_count: number
  segment_bytes: number
  export_count: number
  export_bytes: number
  hash_pending: number
}

// LocalStorageInput is the request body for creating/editing a kind=local
// storage from the Storage page — the only storage kind this page's
// add/edit form creates directly (a cifs share is still added from the
// Shares page; see cmd/api/handlers_storages.go's storageRequestBody for
// the full shape POST/PATCH /api/storages accepts, of which this is the
// local-relevant subset).
export interface LocalStorageInput {
  label: string
  path: string
  enabled?: boolean
}

export class StorageApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'StorageApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // CSRF hardening (final-fix-wave item 5): the server requires
  // Content-Type: application/json on every mutating request, so make
  // sure every non-GET call carries it even when the caller didn't pass a
  // body — callers that already set an explicit Content-Type keep it.
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
    throw new StorageApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function getStorages(): Promise<Storage[]> {
  return request('/api/storages')
}

export function createLocalStorage(input: LocalStorageInput): Promise<Storage> {
  return request('/api/storages', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ kind: 'local', ...input }),
  })
}

export function updateLocalStorage(id: string, input: LocalStorageInput): Promise<Storage> {
  return request(`/api/storages/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
}

export function deleteStorage(id: string, opts?: { purgeIndex?: boolean }): Promise<void> {
  const qs = opts?.purgeIndex ? '?purge_index=1' : ''
  return request(`/api/storages/${id}${qs}`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
  })
}

export function scanStorage(id: string): Promise<{ status: string; job?: Job }> {
  return request(`/api/storages/${id}/scan`, { method: 'POST' })
}

// hashStorages enqueues a hash_backfill job — scoped to one storage's
// share_id, or every storage when omitted (the Storage page's "Hash all"
// button).
export function hashStorages(shareId?: string): Promise<{ status: string; job: Job }> {
  const body = shareId ? { share_id: Number(shareId) } : {}
  return request('/api/storages/hash', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

// --- Duplicates (GET /api/duplicates, POST /api/duplicates/refresh) -------

export type DuplicateKind = 'sha256' | 'size_name'

// DuplicateGroupMember is one member of a DuplicateGroup — a segment's or
// export's copy on one storage (internal/db.DuplicateGroupMember).
// share_id is absent for a member on the "cache" pseudo-storage.
export interface DuplicateGroupMember {
  ref_kind: 'segment' | 'export' | string
  ref_id: number
  share_id?: number
  source_id?: number
  storage_key: string
  rel_path: string
}

// DuplicateGroup is one duplicate_groups row plus its members
// (internal/db.DuplicateGroup). kind="sha256" means every member's content
// hash matched (a verified duplicate); kind="size_name" means only
// size+filename matched (probable, pending a hash to confirm).
export interface DuplicateGroup {
  group_key: string
  kind: DuplicateKind
  copies: number
  storages: number
  bytes: number
  reclaimable: number
  sample_name: string
  refreshed_at: string
  members: DuplicateGroupMember[]
}

// DuplicatesTotals is the aggregate over the whole filtered set (not just
// the current page) — internal/db.DuplicateReportTotals.
export interface DuplicatesTotals {
  groups: number
  reclaimable_bytes: number
  bytes: number
}

export interface DuplicatesResponse {
  groups: DuplicateGroup[]
  total: number
  limit: number
  offset: number
  totals: DuplicatesTotals
}

export interface DuplicatesQuery {
  kind?: DuplicateKind
  storageId?: string
  limit?: number
  offset?: number
}

export function getDuplicates(q: DuplicatesQuery = {}): Promise<DuplicatesResponse> {
  const params = new URLSearchParams()
  if (q.kind) params.set('kind', q.kind)
  if (q.storageId) params.set('storage_id', q.storageId)
  if (q.limit != null) params.set('limit', String(q.limit))
  if (q.offset != null) params.set('offset', String(q.offset))
  const qs = params.toString()
  return request(`/api/duplicates${qs ? `?${qs}` : ''}`)
}

export function refreshDuplicates(): Promise<{ status: string; job: Job }> {
  return request('/api/duplicates/refresh', { method: 'POST' })
}
