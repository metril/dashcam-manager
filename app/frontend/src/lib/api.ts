// Tiny typed fetch wrapper for the dashcam-manager API. Every function
// throws ApiError on a non-2xx response so TanStack Query's isError/error
// surfaces a useful message.
import type {
  CalendarDay,
  GPSTrackResponse,
  SearchResponse,
  SegmentDetections,
  SegmentMeta,
  Stats,
  TripDetail,
  TripsResponse,
  TripTimeline,
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

export function getSegmentDetections(id: number | string): Promise<SegmentDetections> {
  return getJSON(`/api/segments/${id}/detections`)
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

