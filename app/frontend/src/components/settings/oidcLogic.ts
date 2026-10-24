// Pure helpers for the sign-in card.
import { SettingsApiError } from '../../lib/settingsApi'

export interface TestGateInput {
  issuer: string
  discoveredIssuer: string | null // issuer string that last passed discovery
  clientId: string
  publicUrl: string
}

// Test-and-enable is only offered once the issuer currently typed has passed
// discovery and the client ID and public URL are filled in.
export function testGateReason(i: TestGateInput): string | null {
  if (!i.publicUrl.trim()) return 'Enter the public URL first.'
  if (!i.issuer.trim() || i.discoveredIssuer !== i.issuer.trim()) return 'Check the issuer first.'
  if (!i.clientId.trim()) return 'Enter the client ID first.'
  return null
}

export function splitList(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean)
}

export function oidcErrorMessage(err: unknown): string {
  if (err instanceof SettingsApiError) {
    if (err.code === 'oidc_test_required')
      return 'This change needs a successful test sign-in first. Use "Test and enable".'
    if (err.code === 'oidc_lockout')
      return 'Not saved: this change would lock out your own account. Keep your email or group allowed.'
    if (err.code === 'public_url_required') return 'Set the public URL before testing sign-in.'
    return err.message
  }
  return err instanceof Error ? err.message : 'Request failed.'
}
