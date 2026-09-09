// Tiny typed fetch wrapper for the M11 export API (POST /api/export,
// GET/DELETE /api/exports*). Kept separate from lib/api.ts to avoid
// touching that file while it's under concurrent edit elsewhere; mirrors
// lib/jobsApi.ts/lib/sharesApi.ts's ApiError-throwing shape.
import type { Job } from './jobsApi'
import type { DetectionFilter } from './api'

export interface ExportItem {
  segment_id: number
  in_s?: number
  out_s?: number
}

export type ExportContainer = 'mp4' | 'mkv' | 'webm'
export type ExportCodec = 'copy' | 'h264' | 'h265' | 'av1'
export type ExportQuality = 'source' | '1080p' | 'high' | 'balanced' | 'small'

export interface ExportRequest {
  items: ExportItem[]
  camera: 'front' | 'rear' | 'both'
  burn_in: boolean
  target: string // '' or 'download', or a share id as a string
  name?: string
  quality: ExportQuality
  container?: ExportContainer
  codec?: ExportCodec
  // overlay_filter: which detection boxes get burned in when burn_in is
  // true (cmd/api's exportRequest.OverlayFilter). Omitted/undefined means
  // the server's DefaultDetectionFilter.
  overlay_filter?: DetectionFilter
}

// ExportCapabilities is GET /api/export/capabilities' response: which
// codecs this server's ffmpeg build/host can actually encode, so the UI
// can disable a combination that would otherwise 400. copy/h264 are
// always available and don't appear here — see cmd/api's
// validateExportCodecAvailable.
export interface ExportCapabilities {
  h265_available: boolean
  av1_available: boolean
}

export interface Export {
  id: number
  job_id: number | null
  name: string
  path?: string | null
  share_id?: number | null
  bytes: number
  created_at: string
  expires_at?: string | null
}

export interface ExportResponse {
  job: Job
  export: Export
}

// Last-used export settings, persisted so the one-click "Export this
// segment" button (and the export bin panel's defaults) reuse whatever
// camera/quality/target the user picked last time instead of resetting to
// generic defaults on every trip.
const LAST_SETTINGS_KEY = 'dashcam.export.lastSettings.v1'

export interface LastExportSettings {
  camera: 'front' | 'rear' | 'both'
  quality: ExportQuality
  target: string
  container: ExportContainer
  codec: ExportCodec
}

const DEFAULT_EXPORT_SETTINGS: LastExportSettings = {
  camera: 'front',
  quality: 'source',
  target: 'download',
  container: 'mp4',
  codec: 'copy',
}

const QUALITY_VALUES: ExportQuality[] = ['source', '1080p', 'high', 'balanced', 'small']
const CONTAINER_VALUES: ExportContainer[] = ['mp4', 'mkv', 'webm']
const CODEC_VALUES: ExportCodec[] = ['copy', 'h264', 'h265', 'av1']

export function getLastExportSettings(): LastExportSettings {
  try {
    const raw = localStorage.getItem(LAST_SETTINGS_KEY)
    if (!raw) return DEFAULT_EXPORT_SETTINGS
    const parsed = JSON.parse(raw)
    return {
      camera: parsed.camera === 'rear' || parsed.camera === 'both' ? parsed.camera : 'front',
      quality: QUALITY_VALUES.includes(parsed.quality) ? parsed.quality : 'source',
      target: typeof parsed.target === 'string' && parsed.target ? parsed.target : 'download',
      container: CONTAINER_VALUES.includes(parsed.container) ? parsed.container : 'mp4',
      codec: CODEC_VALUES.includes(parsed.codec) ? parsed.codec : 'copy',
    }
  } catch {
    return DEFAULT_EXPORT_SETTINGS
  }
}

export function saveLastExportSettings(settings: LastExportSettings): void {
  try {
    localStorage.setItem(LAST_SETTINGS_KEY, JSON.stringify(settings))
  } catch {
    // best-effort; a full/unavailable localStorage shouldn't block exporting
  }
}

export class ExportsApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ExportsApiError'
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
    throw new ExportsApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export function postExport(req: ExportRequest): Promise<ExportResponse> {
  return request('/api/export', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(req),
  })
}

export function getExports(): Promise<Export[]> {
  return request('/api/exports')
}

export function getExportCapabilities(): Promise<ExportCapabilities> {
  return request('/api/export/capabilities')
}

export function deleteExport(id: number): Promise<void> {
  return request(`/api/exports/${id}`, { method: 'DELETE' })
}

// exportDownloadUrl returns the streamable download URL for a finished
// download-target export — used directly as an <a href>, not fetched via
// getJSON.
export function exportDownloadUrl(id: number): string {
  return `/api/exports/${id}/download`
}
