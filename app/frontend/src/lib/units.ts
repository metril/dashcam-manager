// Units preference: 'us' shows imperial (mph/mi/ft/gal/°F), 'metric' shows
// the raw metric values the API already sends (km/h, km/m, L, °C). Mirrors
// lib/theme.ts exactly — localStorage-backed, a window CustomEvent so any
// mounted control re-syncs without prop drilling or a shared store, and a
// useUnits() hook for components that need to re-render on change.
import { useEffect, useState } from 'react'

export type UnitSystem = 'us' | 'metric'

export interface FormattedValue {
  value: string
  unit: string
}

const STORAGE_KEY = 'dashcam-units'
export const UNITS_CHANGE_EVENT = 'dashcam:units-change'

export function getStoredUnits(): UnitSystem {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'us' || v === 'metric') return v
  } catch {
    // localStorage unavailable (private mode, disabled) — fall back silently.
  }
  return 'us'
}

export function setUnits(units: UnitSystem) {
  try {
    localStorage.setItem(STORAGE_KEY, units)
  } catch {
    // Ignore — the choice just won't persist across reloads this session.
  }
  window.dispatchEvent(new CustomEvent(UNITS_CHANGE_EVENT))
}

// useUnits re-syncs from localStorage on UNITS_CHANGE_EVENT, the same
// pattern every THEME_CHANGE_EVENT listener in the app already hand-rolls
// (Layout's ThemeToggle, Settings' ThemeSegmented, TripMap, DetectionOverlay).
export function useUnits(): UnitSystem {
  const [units, setUnitsState] = useState<UnitSystem>(getStoredUnits)

  useEffect(() => {
    const onChange = () => setUnitsState(getStoredUnits())
    window.addEventListener(UNITS_CHANGE_EVENT, onChange)
    return () => window.removeEventListener(UNITS_CHANGE_EVENT, onChange)
  }, [])

  return units
}

// --- conversions -----------------------------------------------------------

const KM_PER_MI = 1.609344
const M_PER_MI = 1609.344
const M_PER_FT = 0.3048
const L_PER_GAL = 3.785411784

// speedParts converts a speed given in kph (or mps, via `from`) into the
// stored unit system's display speed. GPS points are stored as mps; OBD
// points are stored as kph — callers pass whichever they have.
export function speedParts(v: number, units: UnitSystem, from: 'kph' | 'mps' = 'kph'): FormattedValue {
  const kph = from === 'mps' ? v * 3.6 : v
  if (units === 'us') return { value: String(Math.round(kph / KM_PER_MI)), unit: 'mph' }
  return { value: String(Math.round(kph)), unit: 'km/h' }
}

export function formatSpeed(v: number, units: UnitSystem, from: 'kph' | 'mps' = 'kph'): string {
  const p = speedParts(v, units, from)
  return `${p.value} ${p.unit}`
}

// distanceParts converts meters into miles (1 decimal), dropping to feet
// under 0.1 mi where "0.0 mi" would otherwise be useless; metric mirrors
// the same shape with km / m.
export function distanceParts(m: number, units: UnitSystem): FormattedValue {
  if (units === 'us') {
    const mi = m / M_PER_MI
    if (mi < 0.1) return { value: String(Math.round(m / M_PER_FT)), unit: 'ft' }
    return { value: mi.toFixed(1), unit: 'mi' }
  }
  const km = m / 1000
  if (km < 0.1) return { value: String(Math.round(m)), unit: 'm' }
  return { value: km.toFixed(1), unit: 'km' }
}

export function formatDistance(m: number, units: UnitSystem): string {
  const p = distanceParts(m, units)
  return `${p.value} ${p.unit}`
}

// fuelParts converts liters into gallons (2 decimals — a small enough unit
// that 1 decimal would round away real differences); metric keeps liters
// at 1 decimal, matching the precision the UI already showed pre-units.
export function fuelParts(l: number, units: UnitSystem): FormattedValue {
  if (units === 'us') return { value: (l / L_PER_GAL).toFixed(2), unit: 'gal' }
  return { value: l.toFixed(1), unit: 'L' }
}

export function formatFuel(l: number, units: UnitSystem): string {
  const p = fuelParts(l, units)
  return `${p.value} ${p.unit}`
}

// temperatureParts converts °C into °F.
export function temperatureParts(c: number, units: UnitSystem): FormattedValue {
  if (units === 'us') return { value: String(Math.round((c * 9) / 5 + 32)), unit: '°F' }
  return { value: String(Math.round(c)), unit: '°C' }
}

export function formatTemperature(c: number, units: UnitSystem): string {
  const p = temperatureParts(c, units)
  return `${p.value} ${p.unit}`
}
