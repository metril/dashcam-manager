import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { EventRow, LiveReadings } from './Vehicle'
import type { VehicleEvent } from '../types'

describe('LiveReadings', () => {
  it('shows tires, odometer, range and ambient in us units', () => {
    const out = renderToStaticMarkup(
      <LiveReadings
        units="us"
        obd={{ tire_fl_kpa: 220, tire_fr_kpa: 165, tire_rl_kpa: 220, tire_rr_kpa: 220, odometer_km: 16093.44, range_km: 321.87, ambient_c: 20 }}
      />,
    )
    expect(out).toContain('32 psi')
    expect(out).toContain('24 psi')
    expect(out).toContain('Low')
    expect(out).toContain('10,000 mi')
    expect(out).toContain('200 mi')
    expect(out).toContain('68')
  })
  it('renders nothing without extended readings', () => {
    expect(renderToStaticMarkup(<LiveReadings units="us" obd={{ enabled: true, tire_fl_kpa: null }} />)).toBe('')
    expect(renderToStaticMarkup(<LiveReadings units="us" obd={undefined} />)).toBe('')
  })
})

describe('EventRow', () => {
  it('formats a low tire pressure event', () => {
    const ev = { id: 1, event_type: 'low_tire_pressure', wheel: 'fl', peak: 165, trip_id: null, wall_time: null, speed_kph: null } as unknown as VehicleEvent
    const out = renderToStaticMarkup(
      <MemoryRouter>
        <EventRow event={ev} />
      </MemoryRouter>,
    )
    expect(out).toContain('Low tire pressure · Front left · 24 psi')
  })
})
