import { describe, expect, it } from 'vitest'
import { distanceParts, formatDistance, formatFuel, formatOdometer, formatPressure, formatRange, formatSpeed, LOW_TIRE_KPA, odometerParts, pressureParts, formatTemperature, fuelParts, speedParts, temperatureParts } from './units'

describe('speedParts / formatSpeed', () => {
  it('converts kph to mph (us)', () => {
    expect(speedParts(136, 'us')).toEqual({ value: '85', unit: 'mph' })
    expect(formatSpeed(136, 'us')).toBe('85 mph')
  })

  it('keeps kph as-is (metric)', () => {
    expect(speedParts(136, 'metric')).toEqual({ value: '136', unit: 'km/h' })
  })

  it('converts mps input via from: "mps"', () => {
    // 37.8 mps == 136.08 kph -> 84.55 mph -> rounds to 85
    expect(speedParts(37.8, 'us', 'mps')).toEqual({ value: '85', unit: 'mph' })
    expect(speedParts(10, 'metric', 'mps')).toEqual({ value: '36', unit: 'km/h' })
  })
})

describe('distanceParts / formatDistance', () => {
  it('converts meters to miles with 1 decimal (us)', () => {
    expect(distanceParts(17874.7, 'us')).toEqual({ value: '11.1', unit: 'mi' })
    expect(formatDistance(17874.7, 'us')).toBe('11.1 mi')
  })

  it('drops to feet under 0.1 mi (us)', () => {
    // 100 m = 0.0621 mi < 0.1 mi -> feet
    expect(distanceParts(100, 'us')).toEqual({ value: '328', unit: 'ft' })
  })

  it('converts meters to km with 1 decimal (metric)', () => {
    expect(distanceParts(17874.7, 'metric')).toEqual({ value: '17.9', unit: 'km' })
  })

  it('drops to meters under 0.1 km (metric)', () => {
    expect(distanceParts(42, 'metric')).toEqual({ value: '42', unit: 'm' })
  })
})

describe('fuelParts / formatFuel', () => {
  it('converts liters to gallons with 2 decimals (us)', () => {
    expect(fuelParts(1.703, 'us')).toEqual({ value: '0.45', unit: 'gal' })
    expect(formatFuel(1.703, 'us')).toBe('0.45 gal')
  })

  it('keeps liters with 1 decimal (metric)', () => {
    expect(fuelParts(1.703, 'metric')).toEqual({ value: '1.7', unit: 'L' })
  })
})

describe('temperatureParts / formatTemperature', () => {
  it('converts celsius to fahrenheit (us)', () => {
    expect(temperatureParts(91, 'us')).toEqual({ value: '196', unit: '°F' })
    expect(formatTemperature(91, 'us')).toBe('196 °F')
  })

  it('keeps celsius (metric)', () => {
    expect(temperatureParts(91, 'metric')).toEqual({ value: '91', unit: '°C' })
  })
})

describe('pressure / odometer / range', () => {
  it('pressure', () => {
    expect(pressureParts(220, 'us')).toEqual({ value: '32', unit: 'psi' })
    expect(pressureParts(220, 'metric')).toEqual({ value: '220', unit: 'kPa' })
    expect(formatPressure(220, 'us')).toBe('32 psi')
    expect(formatPressure(null, 'us')).toBe('—')
    expect(pressureParts(undefined, 'metric')).toEqual({ value: '—', unit: '' })
    expect(Math.round(LOW_TIRE_KPA * 0.1450377)).toBe(26)
  })
  it('odometer', () => {
    expect(odometerParts(160934.4, 'us')).toEqual({ value: '100,000', unit: 'mi' })
    expect(formatOdometer(123456.7, 'metric')).toBe('123,457 km')
    expect(formatOdometer(null, 'us')).toBe('—')
  })
  it('range', () => {
    expect(formatRange(341.2, 'us')).toBe('212 mi')
    expect(formatRange(341.2, 'metric')).toBe('341 km')
    expect(formatRange(null, 'metric')).toBe('—')
  })
})
