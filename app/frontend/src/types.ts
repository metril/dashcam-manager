// Typed shapes for the M5 browse read API (GET /api/calendar, /api/trips,
// /api/trips/{id}, /api/segments/{id}, /api/segments/{id}/detections,
// /api/stats). Field names/casing mirror the JSON exactly.

export interface CalendarDay {
  date: string // YYYY-MM-DD
  trips: number
  segments: number
  bytes: number
  objects: number
}

// ClassSummary is a sparse map of detected-object class -> count (only
// classes with a positive count are present), e.g. {car: 3, person: 1}.
export type ClassSummary = Record<string, number>

export interface TripCard {
  id: number
  trip_key: string
  started_at: string
  ended_at: string | null
  duration_s: number | null
  segment_count: number
  bytes: number
  distance_m: number | null
  max_speed_mps: number | null
  front: boolean
  rear: boolean
  summary: ClassSummary
  cover_thumb: string | null
}

export interface TripsResponse {
  trips: TripCard[]
  next_cursor: string
}

export interface TripSegment {
  id: number
  camera: 'front' | 'rear'
  seq: number
  start_time: string
  end_time: string | null
  duration_s: number | null
  bytes: number
  has_detections: boolean
  has_gps: boolean
  summary?: ClassSummary
}

export interface TripDetail extends TripCard {
  pair_offset_s: number | null
  segments: TripSegment[]
}

export interface SegmentMeta {
  id: number
  trip_id: number | null
  camera: 'front' | 'rear'
  rel_path: string
  seq: number
  start_time: string
  end_time: string | null
  duration_s: number | null
  bytes: number
  width: number | null
  height: number | null
  has_detections: boolean
  has_gps: boolean
  summary?: ClassSummary
}

export interface DetObject {
  class: string
  confidence: number
  bbox: [number, number, number, number]
}

export interface DetPlate {
  text: string
  confidence: number
  bbox: [number, number, number, number]
  vehicle_bbox: [number, number, number, number]
}

export interface DetVehicle {
  type: string
  type_confidence: number
  make?: string
  make_confidence?: number
  bbox: [number, number, number, number]
}

export interface DetFrame {
  timestamp: number
  wall_time: string
  frame_index: number
  pts_offset: number
  objects: DetObject[]
  plates?: DetPlate[]
  vehicles?: DetVehicle[]
  gps?: {
    lat: number
    lon: number
    speed_mps: number
    heading: number
    sats: number
    fix: number
  }
}

export interface SegmentDetections {
  segment: string
  camera: string
  frame_width: number
  frame_height: number
  frames_analyzed: number
  start_time: string
  end_time: string
  total_objects: number
  total_plates: number
  total_vehicles_classified: number
  detections: DetFrame[]
}

export interface StatsByMonth {
  month: string // YYYY-MM
  bytes: number
  segments: number
}

export interface Stats {
  segments: number
  trips: number
  bytes: number
  by_month: StatsByMonth[]
  by_camera: Record<string, number>
  by_class: Record<string, number>
}
