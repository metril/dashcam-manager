// ExportBinContext holds the M6 "export bin": an in/out point pair the
// user can set on the currently open trip, plus a list of whole segments
// added with S. Persisted to localStorage so it survives a reload; the
// actual export (rendering/downloading a clip) lands in M11 — this is
// just the picking/staging UI.
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'

const STORAGE_KEY = 'dashcam.exportBin.v1'

export interface ExportRange {
  tripId: number
  inS: number
  outS: number
}

export interface ExportBinState {
  ranges: ExportRange[]
  segmentIds: number[]
}

interface PendingInOut {
  inS: number | null
  outS: number | null
}

interface ExportBinContextValue {
  state: ExportBinState
  pendingFor: (tripId: number) => PendingInOut
  setIn: (tripId: number, atS: number) => void
  setOut: (tripId: number, atS: number) => void
  clearPending: (tripId: number) => void
  addPendingRange: (tripId: number) => void
  removeRange: (index: number) => void
  moveRange: (index: number, dir: -1 | 1) => void
  addSegment: (segmentId: number) => void
  removeSegment: (segmentId: number) => void
  moveSegment: (index: number, dir: -1 | 1) => void
  clearAll: () => void
}

// swap returns a copy of arr with the elements at i and i+dir swapped, or
// arr unchanged if that would go out of bounds — shared by moveRange/
// moveSegment's up/down reordering.
function swap<T>(arr: T[], i: number, dir: -1 | 1): T[] {
  const j = i + dir
  if (j < 0 || j >= arr.length) return arr
  const next = arr.slice()
  ;[next[i], next[j]] = [next[j], next[i]]
  return next
}

const ExportBinContext = createContext<ExportBinContextValue | null>(null)

function isExportRange(x: unknown): x is ExportRange {
  if (typeof x !== 'object' || x === null) return false
  const r = x as Record<string, unknown>
  return typeof r.tripId === 'number' && typeof r.inS === 'number' && typeof r.outS === 'number'
}

function isSegmentId(x: unknown): x is number {
  return typeof x === 'number' && Number.isFinite(x)
}

// loadInitial parses the persisted bin defensively: localStorage content is
// user/browser-controlled (a stale shape from an older build, or hand-edited
// devtools state), so every item is validated before being trusted rather
// than assuming the array elements match ExportRange/number.
function loadInitial(): ExportBinState {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return { ranges: [], segmentIds: [] }
    const parsed = JSON.parse(raw)
    return {
      ranges: Array.isArray(parsed.ranges) ? parsed.ranges.filter(isExportRange) : [],
      segmentIds: Array.isArray(parsed.segmentIds) ? parsed.segmentIds.filter(isSegmentId) : [],
    }
  } catch {
    return { ranges: [], segmentIds: [] }
  }
}

export function ExportBinProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<ExportBinState>(loadInitial)
  // Pending in/out for whichever trip is currently being marked — kept
  // separate from `state` (which is the persisted bin) since it's
  // transient scrub state, not yet a committed export range.
  const [pending, setPending] = useState<Record<number, PendingInOut>>({})

  useEffect(() => {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(state))
    } catch {
      // best-effort; a full/unavailable localStorage shouldn't break playback
    }
  }, [state])

  const pendingFor = useCallback((tripId: number) => pending[tripId] ?? { inS: null, outS: null }, [pending])

  const setIn = useCallback((tripId: number, atS: number) => {
    setPending((p) => ({ ...p, [tripId]: { inS: atS, outS: p[tripId]?.outS ?? null } }))
  }, [])

  const setOut = useCallback((tripId: number, atS: number) => {
    setPending((p) => ({ ...p, [tripId]: { inS: p[tripId]?.inS ?? null, outS: atS } }))
  }, [])

  const clearPending = useCallback((tripId: number) => {
    setPending((p) => ({ ...p, [tripId]: { inS: null, outS: null } }))
  }, [])

  const addPendingRange = useCallback(
    (tripId: number) => {
      const cur = pending[tripId]
      if (cur == null || cur.inS == null || cur.outS == null) return
      const [inS, outS] = cur.inS <= cur.outS ? [cur.inS, cur.outS] : [cur.outS, cur.inS]
      setState((s) => ({ ...s, ranges: [...s.ranges, { tripId, inS, outS }] }))
      setPending((p) => ({ ...p, [tripId]: { inS: null, outS: null } }))
    },
    [pending],
  )

  const removeRange = useCallback((index: number) => {
    setState((s) => ({ ...s, ranges: s.ranges.filter((_, i) => i !== index) }))
  }, [])

  const moveRange = useCallback((index: number, dir: -1 | 1) => {
    setState((s) => ({ ...s, ranges: swap(s.ranges, index, dir) }))
  }, [])

  const addSegment = useCallback((segmentId: number) => {
    setState((s) => (s.segmentIds.includes(segmentId) ? s : { ...s, segmentIds: [...s.segmentIds, segmentId] }))
  }, [])

  const removeSegment = useCallback((segmentId: number) => {
    setState((s) => ({ ...s, segmentIds: s.segmentIds.filter((id) => id !== segmentId) }))
  }, [])

  const moveSegment = useCallback((index: number, dir: -1 | 1) => {
    setState((s) => ({ ...s, segmentIds: swap(s.segmentIds, index, dir) }))
  }, [])

  const clearAll = useCallback(() => setState({ ranges: [], segmentIds: [] }), [])

  const value = useMemo<ExportBinContextValue>(
    () => ({
      state,
      pendingFor,
      setIn,
      setOut,
      clearPending,
      addPendingRange,
      removeRange,
      moveRange,
      addSegment,
      removeSegment,
      moveSegment,
      clearAll,
    }),
    [
      state,
      pendingFor,
      setIn,
      setOut,
      clearPending,
      addPendingRange,
      removeRange,
      moveRange,
      addSegment,
      removeSegment,
      moveSegment,
      clearAll,
    ],
  )

  return <ExportBinContext.Provider value={value}>{children}</ExportBinContext.Provider>
}

export function useExportBin() {
  const ctx = useContext(ExportBinContext)
  if (!ctx) throw new Error('useExportBin must be used within ExportBinProvider')
  return ctx
}
