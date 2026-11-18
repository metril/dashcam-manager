// Shared label/colour/icon map for VehicleEventType (schema v10 OBD
// events) — used by TimelineStrip/TimelineOverview's event-marker lane,
// TripMap's CircleMarkers, TripDetail's per-type event counts, and the
// Vehicle page's event list/chips, so every surface agrees on the same
// look for a given event type.
import {
  AlertTriangle,
  CircleDot,
  Flame,
  Gauge,
  Hourglass,
  Snowflake,
  TrendingDown,
  TrendingUp,
  Wind,
  Zap,
  type LucideIcon,
} from 'lucide-react'
import type { VehicleEventType, Wheel } from '../types'

export interface VehicleEventMeta {
  label: string
  // CSS custom-property color token (theme-aware), same convention as
  // TripMap's speedColor/resolveToken helpers.
  color: string
  icon: LucideIcon
}

export const VEHICLE_EVENT_META: Record<VehicleEventType, VehicleEventMeta> = {
  hard_brake: { label: 'Hard brake', color: '--color-danger', icon: TrendingDown },
  hard_accel: { label: 'Hard accel', color: '--color-warn', icon: TrendingUp },
  speeding: { label: 'Speeding', color: '--color-danger', icon: Gauge },
  idling: { label: 'Idling', color: '--color-text-muted', icon: Hourglass },
  overheat: { label: 'Overheat', color: '--color-danger', icon: Flame },
  charging_fault: { label: 'Charging fault', color: '--color-warn', icon: Zap },
  cold_hard: { label: 'Cold start', color: '--color-accent', icon: Snowflake },
  cornering: { label: 'Hard cornering', color: '--color-warn', icon: Wind },
  stall: { label: 'Stall', color: '--color-danger', icon: AlertTriangle },
  low_tire_pressure: { label: 'Low tire pressure', color: '--color-warn', icon: CircleDot },
}

export function vehicleEventMeta(type: string): VehicleEventMeta {
  return (VEHICLE_EVENT_META as Record<string, VehicleEventMeta>)[type] ?? {
    label: type,
    color: '--color-text-muted',
    icon: AlertTriangle,
  }
}

export const VEHICLE_EVENT_TYPES: VehicleEventType[] = Object.keys(VEHICLE_EVENT_META) as VehicleEventType[]

const WHEEL_LABELS: Record<Wheel, string> = {
  fl: 'Front left',
  fr: 'Front right',
  rl: 'Rear left',
  rr: 'Rear right',
}

export function wheelLabel(wheel: string | null | undefined): string {
  return (wheel && (WHEEL_LABELS as Record<string, string>)[wheel]) || ''
}
