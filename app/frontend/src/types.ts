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
  // Count of this trip's segments with unreadable_reason set (0 if none) —
  // see TripSegment's identically-named field below.
  unreadable_count: number
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
  // Set once a re-scan found this segment's file gone from its source
  // with a verified ingest backing it (see internal/db/schema.go's
  // archived_at doc comment) — its original video is no longer available
  // for playback/thumbnails, only its detections/summary/GPS survive.
  archived_at?: string | null
  archived_to_share?: string | null
  // Set once the segment's media file was found undecodable (see
  // internal/db/schema.go's unreadable_reason doc comment) — a short
  // human-readable classification, e.g. "Missing moov atom". The player
  // and export panel both treat such a segment as unavailable.
  unreadable_reason?: string | null
  unreadable_at?: string | null
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
  // See TripSegment's identically-named fields above.
  archived_at?: string | null
  archived_to_share?: string | null
  unreadable_reason?: string | null
  unreadable_at?: string | null
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
  plates: DetPlate[]
  vehicles: DetVehicle[]
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

export interface TimelineSegment {
  id: number
  camera: 'front' | 'rear'
  seq: number
  start_time: string
  offset_s: number
  duration_s: number
  count: number
}

export interface TripTimeline {
  trip_id: number
  started_at: string
  duration_s: number
  segments: TimelineSegment[]
}

export interface StatsByMonth {
  month: string // YYYY-MM
  bytes: number
  segments: number
}

export interface StatsBySource {
  source_id: number
  label: string
  kind: 'ssd' | 'share'
  segments: number
  bytes: number
}

export interface Stats {
  segments: number
  trips: number
  bytes: number
  by_month: StatsByMonth[]
  by_camera: Record<string, number>
  by_class: Record<string, number>
  by_source: StatsBySource[]
  ingested_percent: number
  unreadable: number
  // tracks / tracks_by_kind (fix round 1): det_tracks row counts — one per
  // followed object/plate/vehicle, not per detection frame (unlike
  // by_class). 0 means "not available" (pre-tracker database, or the
  // backfill hasn't run yet), not "confirmed empty."
  tracks: number
  tracks_by_kind: Record<string, number>
}

// --- M12: search ---------------------------------------------------------

export interface SearchHit {
  pts_offset: number
  kind: 'class' | 'plate' | 'make' | 'type'
  text: string
  confidence: number
  // Count is the number of raw detection frames collapsed into this hit
  // (cmd/api's SearchHit.Count): a track's n_frames when grouped by
  // det_tracks, or 1 for an ungrouped frame. >1 means this chip represents
  // an object followed across several frames, not a single detection.
  count: number
}

export interface SearchResult {
  segment: { id: number; camera: 'front' | 'rear'; seq: number; unreadable_reason?: string | null }
  trip: { id: number; trip_key: string } | null
  start_time: string
  thumb: string
  hits: SearchHit[]
  // HitsTruncated: true when this segment had more hits than fit in the
  // list above — it's a sample, not exhaustive.
  hits_truncated?: boolean
}

export interface SearchResponse {
  results: SearchResult[]
  next_cursor: string
}

// --- WP8: catalog ----------------------------------------------------------

export type CatalogKind = 'make' | 'type' | 'plate' | 'class'

export interface CatalogItem {
  value: string
  count: number
  first_seen: string
  last_seen: string
  sample_segment_id: number
  sample_thumb: string
}

export interface CatalogResponse {
  kind: CatalogKind
  items: CatalogItem[]
  next_cursor: string
  refreshed_at: string
}

// --- M12: GPS track -------------------------------------------------------

export interface GPSTrackPoint {
  lat: number
  lon: number
  speed_mps: number | null
  t: string
}

export interface GPSTrackResponse {
  type: 'Feature'
  geometry: { type: 'LineString'; coordinates: [number, number][] }
  properties: { points: GPSTrackPoint[] }
}

// --- Unreadable segments ----------------------------------------------------

export interface UnreadableSegment {
  id: number
  trip_id: number | null
  camera: 'front' | 'rear'
  rel_path: string
  seq: number
  duration_s: number | null
  reason: string
  unreadable_at: string
  trip_started_at: string | null
}

export interface UnreadableSegmentsResponse {
  segments: UnreadableSegment[]
  next_cursor?: string
}
