// Tiny typed fetch wrapper for the dashcam-manager API. Every function
// throws ApiError on a non-2xx response so TanStack Query's isError/error
// surfaces a useful message.
import type {
  CalendarDay,
  CatalogKind,
  CatalogResponse,
  GPSTrackResponse,
  SearchResponse,
  SegmentDetections,
  SegmentMeta,
  Stats,
  TripDetail,
  TripsResponse,
  TripTimeline,
  UnreadableSegmentsResponse,
} from '../types'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path)
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body && typeof body.error === 'string') message = body.error
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new ApiError(res.status, message)
  }
  return res.json() as Promise<T>
}

// postJSON: the server's CSRF hardening (cmd/api/csrf.go) requires
// Content-Type: application/json on every mutating request, so every POST
// below carries it even for an empty body.
async function postJSON<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    try {
      const errBody = await res.json()
      if (errBody && typeof errBody.error === 'string') message = errBody.error
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new ApiError(res.status, message)
  }
  return res.json() as Promise<T>
}

// putJSON: like postJSON but for PUT /api/settings-style merge requests
// (also CSRF-hardened, same Content-Type requirement).
async function putJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    try {
      const errBody = await res.json()
      if (errBody && typeof errBody.error === 'string') message = errBody.error
    } catch {
      // body wasn't JSON; fall back to the status line
    }
    throw new ApiError(res.status, message)
  }
  return res.json() as Promise<T>
}

export function getCalendar(from: string, to: string): Promise<CalendarDay[]> {
  const params = new URLSearchParams({ from, to })
  return getJSON(`/api/calendar?${params}`)
}

export interface TripsQuery {
  date?: string
  from?: string
  to?: string
  cursor?: string
  limit?: number
}

export function getTrips(query: TripsQuery): Promise<TripsResponse> {
  const params = new URLSearchParams()
  if (query.date) params.set('date', query.date)
  if (query.from) params.set('from', query.from)
  if (query.to) params.set('to', query.to)
  if (query.cursor) params.set('cursor', query.cursor)
  if (query.limit) params.set('limit', String(query.limit))
  return getJSON(`/api/trips?${params}`)
}

export function getTrip(id: number | string): Promise<TripDetail> {
  return getJSON(`/api/trips/${id}`)
}

export function getSegment(id: number | string): Promise<SegmentMeta> {
  return getJSON(`/api/segments/${id}`)
}

// DetTrack is one det_tracks row (cmd/api/handlers_browse.go's
// detectionsView.Tracks): a single object/plate/vehicle followed across a
// segment's analyzed frames by the IoU tracker. Kind is "object", "plate",
// or "vehicle"; class is "" for plate rows (plates aren't classified).
// best_conf/best_text/bbox are null only for a track with no matched
// boxes (in practice BuildTracks never produces one).
export interface DetTrack {
  id: number
  kind: 'object' | 'plate' | 'vehicle'
  class: string
  first_pts: number
  last_pts: number
  n_frames: number
  best_conf: number | null
  best_text: string | null
  bbox: [number, number, number, number] | null
}

// SegmentDetectionsWithTracks is GET /api/segments/{id}/detections' full
// response shape: the base SegmentDetections plus Task 3's `tracks` array
// (served unfiltered — the caller applies a DetectionFilter live).
export type SegmentDetectionsWithTracks = SegmentDetections & { tracks: DetTrack[] }

export function getSegmentDetections(id: number | string): Promise<SegmentDetectionsWithTracks> {
  return getJSON(`/api/segments/${id}/detections`)
}

// --- Task 3: detection filter (burn-in / tracks filtering) -----------------

// DetectionFilter selects which detection boxes/tracks are "on": mirrors
// cmd/api/detection_filter.go's DetectionFilter, used both as
// exportRequest's overlay_filter and as the "detection_filter" settings
// value. An empty classes list means "all object classes".
export interface DetectionFilter {
  min_conf: number
  objects: boolean
  plates: boolean
  vehicles: boolean
  classes: string[]
}

// defaultDetectionFilter mirrors cmd/api's DefaultDetectionFilter: nothing
// filtered out, a 0.5 minimum confidence floor.
export const defaultDetectionFilter: DetectionFilter = {
  min_conf: 0.5,
  objects: true,
  plates: true,
  vehicles: true,
  classes: [],
}

// getDetectionFilter reads the persisted "detection_filter" setting (GET
// /api/settings only returns the whole settings object; there's no
// per-key GET), falling back to defaultDetectionFilter if it's never been
// set (e.g. a database created before this setting existed).
export async function getDetectionFilter(): Promise<DetectionFilter> {
  const all = await getJSON<Record<string, unknown>>('/api/settings')
  const raw = all.detection_filter
  return raw ? { ...defaultDetectionFilter, ...(raw as Partial<DetectionFilter>) } : defaultDetectionFilter
}

// setDetectionFilter persists filter as the "detection_filter" setting via
// PUT /api/settings, returning the full merged settings object (matching
// handleSettingsPut's response shape).
export function setDetectionFilter(filter: DetectionFilter): Promise<Record<string, unknown>> {
  return putJSON('/api/settings', { detection_filter: filter })
}

export function getStats(): Promise<Stats> {
  return getJSON('/api/stats')
}

export function getTripTimeline(id: number | string): Promise<TripTimeline> {
  return getJSON(`/api/trips/${id}/timeline`)
}

// segmentMediaUrl returns the streamable mp4 URL for a segment (Range
// requests supported server-side) — used directly as a <video> src, not
// fetched via getJSON.
export function segmentMediaUrl(id: number | string): string {
  return `/api/segments/${id}/media`
}

// segmentHLSMasterUrl returns the HLS (fMP4) master playlist URL for a
// segment — the fallback source for a browser whose canPlayType says it
// can't decode the source HEVC directly (see lib/hlsPlayback.ts, which
// picks between this and segmentMediaUrl and drives hls.js when needed).
// Generated on demand server-side (cmd/api/handlers_hls.go); GETting this
// URL is itself what triggers generation.
export function segmentHLSMasterUrl(id: number | string): string {
  return `/api/segments/${id}/hls/master.m3u8`
}

// --- M12: search + GPS track ----------------------------------------------

export interface SearchQuery {
  q?: string
  classes?: string[] // repeated ?class= params — a true OR multi-select
  make?: string
  type?: string
  plate?: string
  from?: string
  to?: string
  min_conf?: number
  min_speed?: number
  cursor?: string
  limit?: number
}

export function search(query: SearchQuery): Promise<SearchResponse> {
  const params = new URLSearchParams()
  for (const cls of query.classes ?? []) params.append('class', cls)
  for (const [key, value] of Object.entries(query)) {
    if (key === 'classes') continue
    if (value !== undefined && value !== '') params.set(key, String(value))
  }
  return getJSON(`/api/search?${params}`)
}

export function getGPSTrack(tripId: number | string): Promise<GPSTrackResponse> {
  return getJSON(`/api/gps/track?trip_id=${tripId}`)
}

// --- WP8: catalog ----------------------------------------------------------

export interface CatalogQuery {
  kind: CatalogKind
  minCount?: number
  q?: string
  cursor?: string
  limit?: number
}

export function getCatalog(query: CatalogQuery): Promise<CatalogResponse> {
  const params = new URLSearchParams({ kind: query.kind })
  if (query.minCount !== undefined) params.set('min_count', String(query.minCount))
  if (query.q) params.set('q', query.q)
  if (query.cursor) params.set('cursor', query.cursor)
  if (query.limit !== undefined) params.set('limit', String(query.limit))
  return getJSON(`/api/catalog?${params}`)
}

// --- Unreadable segments ----------------------------------------------------

export function getUnreadableSegments(cursor?: string): Promise<UnreadableSegmentsResponse> {
  const params = new URLSearchParams()
  if (cursor) params.set('cursor', cursor)
  const qs = params.toString()
  return getJSON(`/api/segments/unreadable${qs ? `?${qs}` : ''}`)
}

// recheckUnreadableSegments clears the unreadable flag on the given
// segment ids (or every currently-unreadable segment, if ids is omitted)
// and enqueues a "thumbs" job for them; returns the queued job's id so the
// caller can link to it.
export function recheckUnreadableSegments(ids?: number[]): Promise<{ status: string; count: number; job?: { id: number } }> {
  return postJSON('/api/segments/unreadable/recheck', ids && ids.length > 0 ? { ids } : undefined)
}

// --- Task 6: track backfill --------------------------------------------------

// backfillTracks enqueues a "tracks_backfill" job (cmd/api/handlers_tracks.go)
// that builds det_tracks rows for every segment with detections but no
// tracks yet — the Settings-page "Rebuild object tracks" trigger.
export function backfillTracks(): Promise<{ status: string; count: number; job?: { id: number } }> {
  return postJSON('/api/tracks/backfill')
}

