// useDetectionFilter: the live DetectionFilter shared by the player's
// overlay, its track list, and the export panel's burn-in preview. Backed
// by localStorage (key `detection_filter`) for instant reads/writes across
// remounts, seeded once from the server's persisted default (GET
// /api/settings' detection_filter, via lib/api's getDetectionFilter) the
// first time this app instance runs with nothing in localStorage yet —
// after that, localStorage is the source of truth for the *live* filter,
// independent of whatever's saved as the server-side default. "Save as
// default" (DetectionFilterMenu) explicitly calls setDetectionFilter to
// push the current live value back to the server.
import { useCallback, useEffect, useState } from 'react'
import { defaultDetectionFilter, getDetectionFilter, type DetectionFilter } from '../lib/api'

const STORAGE_KEY = 'detection_filter'

// FILTER_CHANGE_EVENT lets every useDetectionFilter() instance in the tab
// stay in sync when one of them writes a new value (mirrors lib/theme.ts's
// THEME_CHANGE_EVENT pattern) — plain localStorage writes don't fire a
// `storage` event in the same document that made them.
const FILTER_CHANGE_EVENT = 'detection-filter-change'

function readStored(): DetectionFilter | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw)
    return { ...defaultDetectionFilter, ...parsed }
  } catch {
    return null
  }
}

function writeStored(filter: DetectionFilter) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(filter))
  } catch {
    // best-effort; a full/unavailable localStorage shouldn't block filtering
  }
  window.dispatchEvent(new CustomEvent(FILTER_CHANGE_EVENT))
}

export function useDetectionFilter(): [DetectionFilter, (filter: DetectionFilter) => void] {
  const [filter, setFilterState] = useState<DetectionFilter>(() => readStored() ?? defaultDetectionFilter)

  // Seed from the server default exactly once per app load, only when
  // nothing is in localStorage yet (a fresh browser/profile) — never
  // overwrites a value the user already has stored locally.
  useEffect(() => {
    if (readStored() != null) return
    let cancelled = false
    getDetectionFilter()
      .then((serverDefault) => {
        if (cancelled || readStored() != null) return
        writeStored(serverDefault)
        setFilterState(serverDefault)
      })
      .catch(() => {
        // Server unreachable/settings row missing — the in-memory
        // defaultDetectionFilter already in state is a fine fallback.
      })
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    const onChange = () => setFilterState(readStored() ?? defaultDetectionFilter)
    window.addEventListener(FILTER_CHANGE_EVENT, onChange)
    return () => window.removeEventListener(FILTER_CHANGE_EVENT, onChange)
  }, [])

  const setFilter = useCallback((next: DetectionFilter) => {
    writeStored(next)
    setFilterState(next)
  }, [])

  return [filter, setFilter]
}
