import { describe, expect, it } from 'vitest'
import { buildLoginURL, isUnauthorized } from './authApi'

describe('buildLoginURL', () => {
  it('encodes path and query into next', () => {
    expect(buildLoginURL({ pathname: '/trips/5', search: '?x=1&y=a b' })).toBe(
      '/auth/login?next=%2Ftrips%2F5%3Fx%3D1%26y%3Da%20b',
    )
  })
  it('handles an empty query', () => {
    expect(buildLoginURL({ pathname: '/', search: '' })).toBe('/auth/login?next=%2F')
  })
})

describe('isUnauthorized', () => {
  it('matches only status 401', () => {
    expect(isUnauthorized({ status: 401 })).toBe(true)
    expect(isUnauthorized({ status: 403 })).toBe(false)
    expect(isUnauthorized(new Error('x'))).toBe(false)
    expect(isUnauthorized(null)).toBe(false)
  })
})
