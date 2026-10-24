import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import OidcCard from './OidcCard'
import { oidcErrorMessage, splitList, testGateReason } from './oidcLogic'
import { SettingsApiError, type Settings } from '../../lib/settingsApi'

const base: Settings = {
  ssd_path: '/mnt/a', ssd_uuid: '', qsv_device: '', cuda_device: '', local_root: '/mnt/b', ffmpeg_bin: 'ffmpeg', ffprobe_bin: 'ffprobe',
  public_url: 'https://dc.example.com', max_ffmpeg_jobs: 2, workers: 2, timezone: '', hw_mode: 'auto', dashcam_poll_interval_s: 0,
  oidc_enabled: false, oidc_issuer: 'https://idp.example.com', oidc_client_id: 'abc', oidc_groups_claim: '', oidc_scopes: [],
  oidc_allowed_emails: [], oidc_allowed_groups: [], oidc_client_secret_set: false, oidc_secret_error: '',
}

describe('testGateReason', () => {
  const ok = { issuer: 'https://idp', discoveredIssuer: 'https://idp', clientId: 'c', publicUrl: 'https://x' }
  it('allows testing once discovery passed for the current issuer', () => {
    expect(testGateReason(ok)).toBeNull()
  })
  it('blocks until the issuer is checked', () => {
    expect(testGateReason({ ...ok, discoveredIssuer: null })).toMatch(/issuer/i)
  })
  it('blocks again when the issuer is edited after discovery', () => {
    expect(testGateReason({ ...ok, issuer: 'https://other' })).toMatch(/issuer/i)
  })
  it('requires public URL and client ID', () => {
    expect(testGateReason({ ...ok, publicUrl: ' ' })).toMatch(/public url/i)
    expect(testGateReason({ ...ok, clientId: '' })).toMatch(/client id/i)
  })
})

describe('OidcCard', () => {
  const html = (s: Settings) => renderToStaticMarkup(<OidcCard settings={s} meta={undefined} onSaved={() => {}} />)
  it('disables Test and enable until the issuer has been checked', () => {
    const out = html(base)
    expect(out).toMatch(/<button[^>]*disabled=""[^>]*>Test and enable/)
    expect(out).toContain('Check the issuer first.')
  })
  it('offers Test again, enabled, for an already enabled setup', () => {
    const out = html({ ...base, oidc_enabled: true })
    expect(out).toMatch(/<button type="button" class="[^"]*">Test again/)
  })
  it('shows the secret error banner', () => {
    expect(html({ ...base, oidc_secret_error: 'bad key' })).toContain('could not be decrypted')
  })
})

describe('helpers', () => {
  it('splits lists', () => expect(splitList('a@b.c, d@e.f  g')).toEqual(['a@b.c', 'd@e.f', 'g']))
  it('maps error codes to clear messages', () => {
    expect(oidcErrorMessage(new SettingsApiError(400, 'x', 'oidc_test_required'))).toMatch(/test sign-in/)
    expect(oidcErrorMessage(new SettingsApiError(409, 'x', 'oidc_lockout'))).toMatch(/lock out/)
    expect(oidcErrorMessage(new SettingsApiError(400, 'plain'))).toBe('plain')
  })
})
