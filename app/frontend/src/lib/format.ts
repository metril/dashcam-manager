// Small formatting helpers shared across browse pages.

export function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const exp = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / 1024 ** exp
  return `${value >= 100 || exp === 0 ? Math.round(value) : value.toFixed(1)} ${units[exp]}`
}

export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds)) return '—'
  const total = Math.round(seconds)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
  return `${m}:${String(s).padStart(2, '0')}`
}

export function formatClockTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

export function formatDateLong(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric' })
}

// dateKey renders a Date as the local YYYY-MM-DD key used by the browse
// calendar/trips API (which itself groups by the UTC date of started_at —
// see internal/db/queries_browse.go — so this deliberately reads UTC
// getters, not local ones, to stay aligned with the server's grouping).
export function dateKey(d: Date): string {
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-${String(d.getUTCDate()).padStart(2, '0')}`
}

// titleCase converts a snake_case or lowercase enum-ish token (e.g. job
// states, share statuses, source kinds) into a human-readable, title-cased
// label: "queued" -> "Queued", "not_found" -> "Not Found".
export function titleCase(value: string | null | undefined): string {
  if (!value) return '—'
  return value
    .split(/[_\s]+/)
    .filter(Boolean)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ')
}

// humanizeState is titleCase with special-casing for compound statuses
// like "error: timeout" (share status), so the sentence stays readable
// instead of "Error: Timeout".
export function humanizeState(value: string | null | undefined): string {
  if (!value) return '—'
  const [head, ...rest] = value.split(':')
  const label = titleCase(head.trim())
  if (rest.length === 0) return label
  return `${label}: ${rest.join(':').trim()}`
}

// formatSegmentLabel renders a camera + sequence label consistently, e.g.
// formatSegmentLabel('Front', 12) -> "Front · Segment 12".
export function formatSegmentLabel(camera: string, seq: number | null | undefined): string {
  return `${titleCase(camera)} · Segment ${seq ?? '—'}`
}

// formatTripKey turns the raw trip_key ("recording_20260907_101412+165739",
// the front recording's directory name — see internal/scan/pair.go) into a
// human label: "Recording 2026-09-07 10:14:12". Falls back to the raw key
// verbatim if it doesn't match the expected recording_YYYYMMDD_HHMMSS
// prefix (e.g. future key formats).
export function formatTripKey(tripKey: string | null | undefined): string {
  if (!tripKey) return '—'
  const m = tripKey.match(/^recording_(\d{4})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})/)
  if (!m) return tripKey
  const [, year, month, day, hour, min, sec] = m
  return `Recording ${year}-${month}-${day} ${hour}:${min}:${sec}`
}

// pluralize appends "s" (or a supplied plural form) to `word` when `count`
// isn't exactly 1 — e.g. pluralize(3, 'segment') -> "3 segments",
// pluralize(1, 'segment') -> "1 segment".
export function pluralize(count: number, word: string, plural: string = `${word}s`): string {
  return `${count} ${count === 1 ? word : plural}`
}

export function monthLabel(year: number, month: number): string {
  return new Date(Date.UTC(year, month, 1)).toLocaleDateString(undefined, {
    month: 'long',
    year: 'numeric',
    timeZone: 'UTC',
  })
}
