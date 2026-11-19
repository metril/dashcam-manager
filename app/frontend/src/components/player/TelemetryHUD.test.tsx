import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { ExtReadings } from './TelemetryHUD'
import type { OBDSample } from './obdInterp'

const empty: OBDSample = {
  speed_kph: 50, rpm: 2000, throttle_pct: 10, accel_mps2: 0, coolant_c: 90, batt_v: 13.8,
  tire_fl_kpa: null, tire_fr_kpa: null, tire_rl_kpa: null, tire_rr_kpa: null,
  odometer_km: null, range_km: null, ambient_c: null,
}

describe('ExtReadings', () => {
  it('shows present values in US units, tints low tires, hides nulls', () => {
    const html = renderToStaticMarkup(
      <ExtReadings
        units="us"
        sample={{ ...empty, tire_fl_kpa: 220, tire_rl_kpa: 170, odometer_km: 16093.4, range_km: 400, ambient_c: 20 }}
      />,
    )
    expect(html).toContain('FL 32 psi')
    expect(html).toContain('RL 25 psi')
    expect(html).not.toContain('FR')
    expect(html).not.toContain('RR')
    expect(html.match(/data-low="true"/g)).toHaveLength(1)
    expect(html).toContain('10,000 mi odo')
    expect(html).toContain('249 mi range')
    expect(html).toContain('68 °F')
  })
  it('renders nothing when all null', () => {
    expect(renderToStaticMarkup(<ExtReadings units="metric" sample={empty} />)).toBe('')
  })
})
