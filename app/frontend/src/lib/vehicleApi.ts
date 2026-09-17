// Tiny typed fetch wrapper for the OBD vehicle telemetry API (GET
// /api/segments/{id}/obd, /api/trips/{id}/vehicle, /api/vehicle/*) — see
// the schema v10 plan for the exact route/response shapes this mirrors.
// Kept separate from lib/api.ts to avoid touching that file while it's
// under concurrent edit elsewhere, same rationale as lib/dashcamApi.ts;
// mirrors lib/api.ts's ApiError-throwing getJSON pattern exactly.
import { ApiError } from './api'
import type {
  SegmentOBD,
  TripVehicle,
  VehicleDTCsResponse,
  VehicleEventsResponse,
  VehicleEventType,
  VehicleStatus,
  VehicleTrendsResponse,
  VehicleTripsResponse,
} from '../types'

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

// getSegmentOBD: GET /api/segments/{id}/obd — 404s when the segment has no
// OBD sidecar at all; callers should treat that as "no telemetry" (e.g.
// check `error instanceof ApiError && error.status === 404`), not surface
// it as a fetch error.
export function getSegmentOBD(segmentId: number | string): Promise<SegmentOBD> {
  return getJSON(`/api/segments/${segmentId}/obd`)
}

export function getTripVehicle(tripId: number | string): Promise<TripVehicle> {
  return getJSON(`/api/trips/${tripId}/vehicle`)
}

export interface VehicleEventsQuery {
  type?: VehicleEventType | string
  from?: string
  to?: string
  trip?: number | string
  cursor?: string
  limit?: number
}

export function listVehicleEvents(query: VehicleEventsQuery = {}): Promise<VehicleEventsResponse> {
  const params = new URLSearchParams()
  if (query.type) params.set('type', query.type)
  if (query.from) params.set('from', query.from)
  if (query.to) params.set('to', query.to)
  if (query.trip !== undefined) params.set('trip', String(query.trip))
  if (query.cursor) params.set('cursor', query.cursor)
  if (query.limit !== undefined) params.set('limit', String(query.limit))
  const qs = params.toString()
  return getJSON(`/api/vehicle/events${qs ? `?${qs}` : ''}`)
}

export interface VehicleTripsQuery {
  from?: string
  to?: string
  cursor?: string
  limit?: number
}

export function listVehicleTrips(query: VehicleTripsQuery = {}): Promise<VehicleTripsResponse> {
  const params = new URLSearchParams()
  if (query.from) params.set('from', query.from)
  if (query.to) params.set('to', query.to)
  if (query.cursor) params.set('cursor', query.cursor)
  if (query.limit !== undefined) params.set('limit', String(query.limit))
  const qs = params.toString()
  return getJSON(`/api/vehicle/trips${qs ? `?${qs}` : ''}`)
}

export function getVehicleStatus(): Promise<VehicleStatus> {
  return getJSON('/api/vehicle/status')
}

export function getVehicleDTCs(): Promise<VehicleDTCsResponse> {
  return getJSON('/api/vehicle/dtcs')
}

export function getVehicleTrends(days?: number): Promise<VehicleTrendsResponse> {
  const qs = days !== undefined ? `?days=${days}` : ''
  return getJSON(`/api/vehicle/trends${qs}`)
}
