import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import TripVehicleExtras from './TripVehicleExtras'
import type { TripVehicleTotals } from '../types'

const base = { dtcs: [], mil: false } as unknown as TripVehicleTotals

describe('TripVehicleExtras', () => {
  it('renders odometer, range, temp and flags a low wheel in US units', () => {
    const html = renderToStaticMarkup(
      <TripVehicleExtras
        units="us"
        totals={{
          ...base,
          odometer_start_km: 16093.4, odometer_end_km: 16109.5,
          range_start_km: 400, range_end_km: 380,
          ambient_min_c: 20, ambient_max_c: 30,
          tire_fl_avg_kpa: 220, tire_fl_min_kpa: 215, tire_fl_max_kpa: 225,
          tire_fr_avg_kpa: 220, tire_fr_min_kpa: 218, tire_fr_max_kpa: 222,
          tire_rl_avg_kpa: 170, tire_rl_min_kpa: 160, tire_rl_max_kpa: 180,
          tire_rr_avg_kpa: 220, tire_rr_min_kpa: 218, tire_rr_max_kpa: 222,
        }}
      />,
    )
    expect(html).toContain('10,000 mi → 10,010 mi')
    expect(html).toContain('249 mi → 236 mi')
    expect(html).toContain('68')
    expect(html).toContain('86')
    expect(html).toContain('psi')
    expect(html).toContain('Outside temp')
    expect(html.match(/data-low="true"/g)).toHaveLength(1)
    expect(html).toContain('data-wheel="rl" data-low="true"')
  })

  it('renders nothing without the new fields', () => {
    expect(renderToStaticMarkup(<TripVehicleExtras units="us" totals={base} />)).toBe('')
  })
})
