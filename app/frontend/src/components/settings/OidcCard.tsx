// Sign-in card: configure OIDC login. Enabling goes through a test-before-
// save flow: "Test and enable" stages the form server-side, opens the IdP
// login in a popup (full redirect if blocked) and the server only turns
// sign-in on after that test login succeeds.
import { useEffect, useRef, useState } from 'react'
import { Check, Copy, Loader2, LogIn } from 'lucide-react'
import {
  discoverOIDC,
  getSettings,
  testOIDC,
  updateSettings,
  type DiscoverResult,
  type Settings,
  type SettingsMeta,
  type SettingsPatch,
  type SettingsUpdateResult,
} from '../../lib/settingsApi'
import { fetchMe } from '../../lib/authApi'
import Switch from '../ui/Switch'
import { Card, ErrorLine, Row, buttonClass, inputClass, primaryButtonClass } from './shared'
import { oidcErrorMessage, splitList, testGateReason } from './oidcLogic'

interface Props {
  settings: Settings
  meta: SettingsMeta | undefined
  // Called after any server-side change (save, disable, successful test).
  onSaved: (result?: SettingsUpdateResult) => void
}

const wide = `${inputClass} w-56`

export default function OidcCard({ settings, meta, onSaved }: Props) {
  const [publicUrl, setPublicUrl] = useState(settings.public_url)
  const [issuer, setIssuer] = useState(settings.oidc_issuer)
  const [clientId, setClientId] = useState(settings.oidc_client_id)
  // null = untouched (keep stored secret); '' = clear; otherwise new value.
  const [secret, setSecret] = useState<string | null>(null)
  const [scopes, setScopes] = useState(settings.oidc_scopes.join(' '))
  const [emails, setEmails] = useState(settings.oidc_allowed_emails.join(', '))
  const [groups, setGroups] = useState(settings.oidc_allowed_groups.join(', '))
  const [groupsClaim, setGroupsClaim] = useState(settings.oidc_groups_claim)

  const [discovery, setDiscovery] = useState<DiscoverResult | null>(null)
  const [discovering, setDiscovering] = useState(false)
  // Issuer string that last passed discovery; an already enabled setup is
  // known good for its stored issuer.
  const [discoveredIssuer, setDiscoveredIssuer] = useState<string | null>(
    settings.oidc_enabled ? settings.oidc_issuer : null,
  )

  const [busy, setBusy] = useState<'save' | 'test' | 'disable' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [info, setInfo] = useState<string | null>(null)
  const [confirmDisable, setConfirmDisable] = useState(false)
  const [copied, setCopied] = useState(false)
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  useEffect(() => () => stopPolling(), [])

  function stopPolling() {
    if (pollRef.current) clearInterval(pollRef.current)
    pollRef.current = null
  }

  const redirectUri = meta?.redirect_uri || (publicUrl.trim() ? `${publicUrl.trim().replace(/\/+$/, '')}/auth/callback` : '')
  const gate = testGateReason({ issuer, discoveredIssuer, clientId, publicUrl })

  async function checkIssuer() {
    setDiscovering(true)
    setError(null)
    try {
      const res = await discoverOIDC(issuer.trim())
      setDiscovery(res)
      setDiscoveredIssuer(res.ok ? issuer.trim() : null)
    } catch (e) {
      setDiscovery({ ok: false, error: oidcErrorMessage(e) })
      setDiscoveredIssuer(null)
    } finally {
      setDiscovering(false)
    }
  }

  function buildPatch(): SettingsPatch {
    const patch: SettingsPatch = {}
    if (publicUrl.trim() !== settings.public_url) patch.public_url = publicUrl.trim()
    if (issuer.trim() !== settings.oidc_issuer) patch.oidc_issuer = issuer.trim()
    if (clientId.trim() !== settings.oidc_client_id) patch.oidc_client_id = clientId.trim()
    if (secret !== null) patch.oidc_client_secret = secret
    if (groupsClaim.trim() !== settings.oidc_groups_claim) patch.oidc_groups_claim = groupsClaim.trim()
    if (splitList(scopes).join(' ') !== settings.oidc_scopes.join(' ')) patch.oidc_scopes = splitList(scopes)
    if (splitList(emails).join(' ') !== settings.oidc_allowed_emails.join(' '))
      patch.oidc_allowed_emails = splitList(emails)
    if (splitList(groups).join(' ') !== settings.oidc_allowed_groups.join(' '))
      patch.oidc_allowed_groups = splitList(groups)
    return patch
  }
  const patch = buildPatch()
  const dirty = Object.keys(patch).length > 0

  async function save() {
    setBusy('save')
    setError(null)
    setInfo(null)
    try {
      const res = await updateSettings(patch)
      setSecret(null)
      setInfo('Sign-in settings saved.')
      onSaved(res)
    } catch (e) {
      setError(oidcErrorMessage(e))
    } finally {
      setBusy(null)
    }
  }

  async function disable() {
    setBusy('disable')
    setError(null)
    setInfo(null)
    try {
      const res = await updateSettings({ oidc_enabled: false })
      setConfirmDisable(false)
      setInfo('Sign-in is turned off. Anyone who can reach this server has full access.')
      onSaved(res)
    } catch (e) {
      setError(oidcErrorMessage(e))
    } finally {
      setBusy(null)
    }
  }

  async function testAndEnable() {
    if (gate) {
      setError(gate)
      return
    }
    setBusy('test')
    setError(null)
    setInfo(null)
    try {
      const { login_url } = await testOIDC({
        issuer: issuer.trim(),
        client_id: clientId.trim(),
        ...(secret !== null ? { client_secret: secret } : {}),
        scopes: splitList(scopes),
        allowed_emails: splitList(emails),
        allowed_groups: splitList(groups),
        groups_claim: groupsClaim.trim(),
        public_url: publicUrl.trim(),
      })
      const popup = window.open(login_url, 'dashcam-oidc-test', 'width=520,height=720')
      if (!popup) {
        // Popup blocked: fall back to a full-page redirect. The server
        // sends us back to /settings?oidc=ok on success.
        window.location.assign(login_url)
        return
      }
      watchPopup(popup)
    } catch (e) {
      setError(oidcErrorMessage(e))
      setBusy(null)
    }
  }

  // Poll until the test login succeeds (/api/me returns a user) or the popup
  // is closed without finishing.
  function watchPopup(popup: Window) {
    stopPolling()
    const finish = (ok: boolean, msg?: string) => {
      stopPolling()
      setBusy(null)
      if (ok) {
        setInfo('Sign-in test passed. Sign-in is now enabled.')
        onSaved()
      } else if (msg) {
        setError(msg)
      }
    }
    pollRef.current = setInterval(async () => {
      const closed = popup.closed
      try {
        const me = await fetchMe()
        if (me.user) {
          if (!popup.closed) popup.close()
          finish(true)
          return
        }
      } catch {
        // 401 until the test login completes
      }
      if (closed) {
        try {
          const s = await getSettings()
          if (s.oidc_enabled) return finish(true)
        } catch {
          // fall through to the failure message
        }
        finish(false, 'The test sign-in was not completed, so nothing was changed.')
      }
    }, 1500)
  }

  async function copyRedirect() {
    try {
      await navigator.clipboard.writeText(redirectUri)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setError('Could not copy. Select the address and copy it by hand.')
    }
  }

  return (
    <Card title="Sign-in" icon={<LogIn className="size-3.5" />}>
      {settings.oidc_secret_error && (
        <p role="alert" className="rounded border border-danger/40 bg-danger/10 px-2 py-1.5 text-[11px] text-danger">
          The saved client secret could not be decrypted, so sign-in was turned off. Enter the secret again and run the
          test. ({settings.oidc_secret_error})
        </p>
      )}

      <Row
        label="Require sign-in"
        note={
          settings.oidc_enabled
            ? 'Visitors must sign in with your identity provider.'
            : 'Off: anyone who can reach this server has full access.'
        }
      >
        <Switch
          checked={settings.oidc_enabled}
          label="Require sign-in"
          onChange={(on) => {
            if (on) void testAndEnable()
            else setConfirmDisable(true)
          }}
        />
      </Row>

      {confirmDisable && (
        <div role="alertdialog" aria-label="Confirm turning off sign-in" className="flex flex-col gap-2 rounded border border-danger/40 bg-danger/10 p-2 text-[11px] text-danger">
          <span>Turn off sign-in? Anyone who can reach this server will have full access.</span>
          <div className="flex gap-2">
            <button type="button" onClick={disable} disabled={busy === 'disable'} className={buttonClass}>
              {busy === 'disable' ? 'Turning off…' : 'Turn off sign-in'}
            </button>
            <button type="button" onClick={() => setConfirmDisable(false)} className={buttonClass}>
              Cancel
            </button>
          </div>
        </div>
      )}

      <Row label="Public URL" settingKey="public_url" note="The address people use to reach this app, for example https://dashcam.example.com">
        <input
          type="text"
          value={publicUrl}
          onChange={(e) => setPublicUrl(e.target.value)}
          className={wide}
          placeholder="https://dashcam.example.com"
        />
      </Row>

      <div className="flex flex-col gap-1 text-xs text-text-muted">
        <span>Redirect URI</span>
        {redirectUri ? (
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 truncate rounded border border-border bg-surface px-2 py-1 text-[11px] text-text-secondary" title={redirectUri}>
              {redirectUri}
            </code>
            <button type="button" onClick={copyRedirect} className={buttonClass} aria-label="Copy redirect URI">
              {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
        ) : (
          <span className="text-[11px]">Set the public URL to get the redirect URI to register with your provider.</span>
        )}
      </div>

      <Row label="Issuer">
        <div className="flex items-center gap-2">
          <input
            type="text"
            value={issuer}
            onChange={(e) => {
              setIssuer(e.target.value)
              setDiscovery(null)
            }}
            className={`${inputClass} w-44`}
            placeholder="https://auth.example.com"
          />
          <button type="button" onClick={checkIssuer} disabled={discovering || !issuer.trim()} className={buttonClass}>
            {discovering && <Loader2 className="size-3.5 animate-spin" />}
            Check
          </button>
        </div>
      </Row>
      {discovery?.ok && (
        <p className="text-[11px] text-success">
          Found the provider. Sign-in page: {discovery.authorization_endpoint}.{' '}
          {discovery.end_session ? 'Sign-out at the provider is supported.' : 'Sign-out at the provider is not supported.'}
        </p>
      )}
      {discovery && !discovery.ok && <p className="text-[11px] text-danger">Check failed: {discovery.error}</p>}

      <Row label="Client ID">
        <input type="text" value={clientId} onChange={(e) => setClientId(e.target.value)} className={wide} />
      </Row>
      <Row label="Client secret" note={settings.oidc_client_secret_set ? 'A secret is saved. Type to replace it.' : 'Leave empty for a public client.'}>
        <div className="flex items-center gap-2">
          <input
            type="password"
            autoComplete="new-password"
            value={secret ?? ''}
            onChange={(e) => setSecret(e.target.value)}
            className={`${inputClass} w-44`}
            placeholder={settings.oidc_client_secret_set && secret === null ? 'Saved' : 'Not set'}
          />
          {settings.oidc_client_secret_set && secret !== '' && (
            <button type="button" onClick={() => setSecret('')} className={buttonClass}>
              Clear
            </button>
          )}
        </div>
      </Row>
      <Row label="Scopes" note="Space separated. Must include openid.">
        <input type="text" value={scopes} onChange={(e) => setScopes(e.target.value)} className={wide} placeholder="openid email profile" />
      </Row>
      <Row label="Allowed emails" note="Comma separated. Leave both lists empty to allow any account from the provider.">
        <input type="text" value={emails} onChange={(e) => setEmails(e.target.value)} className={wide} placeholder="me@example.com" />
      </Row>
      <Row label="Allowed groups">
        <input type="text" value={groups} onChange={(e) => setGroups(e.target.value)} className={wide} placeholder="Family" />
      </Row>
      <Row label="Groups claim">
        <input type="text" value={groupsClaim} onChange={(e) => setGroupsClaim(e.target.value)} className={wide} placeholder="groups" />
      </Row>

      <div className="flex flex-wrap items-center gap-2 border-t border-border pt-2">
        <button type="button" onClick={testAndEnable} disabled={busy !== null || gate !== null} className={primaryButtonClass}>
          {busy === 'test' && <Loader2 className="size-3.5 animate-spin" />}
          {busy === 'test' ? 'Waiting for sign-in…' : settings.oidc_enabled ? 'Test again' : 'Test and enable'}
        </button>
        <button type="button" onClick={save} disabled={busy !== null || !dirty} className={buttonClass}>
          {busy === 'save' ? 'Saving…' : 'Save sign-in settings'}
        </button>
        {gate && <span className="text-[11px] text-text-muted">{gate}</span>}
      </div>
      <p className="text-[11px] text-text-muted">
        Testing opens your provider in a new window. Sign-in is only turned on if that test sign-in succeeds, and your
        account must be allowed by the lists above.
      </p>
      {info && <p className="text-[11px] text-success">{info}</p>}
      {error && <ErrorLine error={new Error(error)} />}
    </Card>
  )
}
