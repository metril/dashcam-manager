import { afterEach, describe, expect, it, vi } from 'vitest'
import { SettingsApiError, getSettingsMeta, testOIDC, updateSettings } from './settingsApi'

function mockFetch(status: number, body: unknown) {
  const fn = vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }))
  vi.stubGlobal('fetch', fn)
  return fn
}

afterEach(() => vi.unstubAllGlobals())

describe('updateSettings', () => {
  it('returns the wrapped result and sends JSON', async () => {
    const fn = mockFetch(200, {
      settings: { workers: 4 },
      restart_required: ['hw_mode'],
      applied: ['workers'],
      apply_errors: { ssd_path: 'busy' },
    })
    const res = await updateSettings({ workers: 4 })
    expect(res.restart_required).toEqual(['hw_mode'])
    expect(res.apply_errors).toEqual({ ssd_path: 'busy' })
    const init = (fn.mock.calls[0] as unknown as [string, RequestInit])[1]
    expect(init.method).toBe('PUT')
    expect((init.headers as Record<string, string>)['Content-Type']).toBe('application/json')
  })

  it('carries the error code and message', async () => {
    mockFetch(400, { error: 'test sign-in required', code: 'oidc_test_required' })
    const err = await updateSettings({ oidc_enabled: true }).catch((e) => e)
    expect(err).toBeInstanceOf(SettingsApiError)
    expect(err.status).toBe(400)
    expect(err.code).toBe('oidc_test_required')
    expect(err.message).toBe('test sign-in required')
  })

  it('leaves code undefined for plain errors', async () => {
    mockFetch(409, { error: 'nope' })
    const err = await updateSettings({ workers: 1 }).catch((e) => e)
    expect(err.code).toBeUndefined()
  })
})

describe('meta and test endpoints', () => {
  it('parses meta', async () => {
    mockFetch(200, { bootstrap: { db: '/d', cache: '/c', addr: ':8080' }, secret_key_source: 'file', auth_disabled: false, env_differs: ['workers'], restart_required: [], redirect_uri: '' })
    const m = await getSettingsMeta()
    expect(m.env_differs).toEqual(['workers'])
    expect(m.secret_key_source).toBe('file')
  })
  it('returns the login url', async () => {
    mockFetch(200, { login_url: '/auth/test-login' })
    expect((await testOIDC({ issuer: 'https://i', client_id: 'c' })).login_url).toBe('/auth/test-login')
  })
})
