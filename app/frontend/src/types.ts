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

// CopyRef is one persistent storage holding a copy of a segment's footage
// or an export's file (local-storage plan task 9). storage_id mirrors
// GET /api/storages' id shape: a share id as a decimal string, or the
// literal "cache" for the export cache pseudo-storage — never the SSD,
// which is not a storage.
export interface CopyRef {
  storage_id: string
  label: string
  kind: string
  rel_path: string
  verified: boolean
}

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
  // Copies/on_ssd (local-storage plan task 9): persistent storages holding
  // this segment's footage, and whether it (or a same-content segment) is
  // still on the recorder's SSD. copies is always present (possibly []).
  copies: CopyRef[]
  on_ssd: boolean
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
  // See TripSegment's identically-named fields above.
  copies: CopyRef[]
  on_ssd: boolean
}

export interface DetObject {
  class: string
  confidence: number
  bbox: [number, number, number, number]
  // track_id: det_tracks identity this detection belongs to, or null/absent
  // when untracked (pre-tracker data, or the tracker didn't link it to any
  // track). See detectionInterp.ts's buildTrackIndex for how this groups
  // detections across frames for interpolation.
  track_id?: number | null
}

export interface DetPlate {
  text: string
  confidence: number
  bbox: [number, number, number, number]
  vehicle_bbox: [number, number, number, number]
  track_id?: number | null
}

export interface DetVehicle {
  type: string
  type_confidence: number
  make?: string
  make_confidence?: number
  bbox: [number, number, number, number]
  track_id?: number | null
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
  // thumbs: cumulative thumbnail hardware-acceleration counters,
  // persisted across every "thumbs" job run (settings key
  // thumb_hw_stats) — mode is "" until the first thumbs job has run.
  thumbs: ThumbsHWStats
}

export interface ThumbsHWStats {
  mode: string
  hw_ok: number
  hw_fallbacks: number
  unreadable: number
  last_run?: string
}

// --- Maintenance tooling ----------------------------------------------------

export interface MaintenanceReport {
  ran_at: string
  integrity_check_kind: 'quick_check' | 'integrity_check'
  integrity_ok: boolean
  integrity_messages?: string[]
  missing_media_segments: number
  missing_media_segment_ids?: number[]
  // sources_unavailable / missing_media_check_incomplete: set when a
  // source's root_path isn't currently mounted/present (e.g. the SSD is
  // unplugged) — in that case missing_media_segments deliberately
  // excludes that source's segments rather than misreporting "every file
  // is gone."
  sources_unavailable?: string[]
  missing_media_check_incomplete?: boolean
  orphan_detections: number
  orphan_det_objects: number
  orphan_det_plates: number
  orphan_det_vehicles: number
  orphan_det_tracks: number
  segments_missing_detections_raw: number
  orphan_thumb_files: number
  orphan_thumb_paths?: string[]
  db_size_bytes: number
  wal_size_bytes: number
  freelist_pages: number
  page_count: number
  page_size_bytes: number
}

export interface MaintenancePruneResult {
  dry_run: boolean
  segments_removed: number
  segments_skipped_recently_modified?: number
  orphan_detections_removed: number
  orphan_det_objects_removed: number
  orphan_det_plates_removed: number
  orphan_det_vehicles_removed: number
  orphan_det_tracks_removed: number
  orphan_thumb_files_removed: number
  wal_checkpointed: boolean
  vacuumed: boolean
  vacuum_skipped_reason?: string
  segments_missing_detections_raw: number
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
  // Copies/on_ssd (local-storage plan task 9): the sample segment's copy
  // info — see TripSegment's identically-named fields above.
  copies: CopyRef[]
  on_ssd: boolean
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
