// Building blocks shared by the Settings page and its cards. The context
// carries server-returned hints (restart-required keys, env-differs keys,
// apply errors) so every Row can show them for its own key.
import { useContext } from 'react'
import { SettingsApiError } from '../../lib/settingsApi'
import { SettingsHintsContext } from './hints'

export const inputClass =
  'rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50'

export const buttonClass =
  'flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text hover:border-text-muted disabled:cursor-not-allowed disabled:opacity-40'

export const primaryButtonClass =
  'flex items-center gap-1.5 rounded-md border border-accent/30 bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 disabled:cursor-not-allowed disabled:opacity-40'

export function RestartChip() {
  return (
    <span
      title="Saved, but this change only takes effect after the container is restarted."
      className="rounded bg-surface px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-text-muted"
    >
      restart required
    </span>
  )
}

export function Card({
  title,
  icon,
  className,
  children,
}: {
  title: string
  icon?: React.ReactNode
  className?: string
  children: React.ReactNode
}) {
  return (
    <section className={`flex flex-col gap-3 rounded-lg border border-border bg-bg/60 p-4 ${className ?? ''}`}>
      <h2 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-text-secondary">
        {icon}
        {title}
      </h2>
      {children}
    </section>
  )
}

// Row: label-left / control-right. When settingKey is given, the row also
// shows a restart chip, an env-differs hint and an apply error if the server
// reported any for that key.
export function Row({
  label,
  settingKey,
  note,
  children,
}: {
  label: string
  settingKey?: string
  note?: string
  children: React.ReactNode
}) {
  const hints = useContext(SettingsHintsContext)
  const restart = !!settingKey && hints.restartRequired.includes(settingKey)
  const envDiffers = !!settingKey && hints.envDiffers.includes(settingKey)
  const applyError = settingKey ? hints.applyErrors[settingKey] : undefined
  return (
    <div className="flex flex-col gap-1 text-xs text-text-muted">
      <div className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-1.5">
          {label}
          {restart && <RestartChip />}
        </span>
        {children}
      </div>
      {note && <span className="text-[11px] text-text-muted">{note}</span>}
      {envDiffers && (
        <span className="text-[11px] text-text-muted">
          The environment variable for this setting has a different value. The saved value is used.
        </span>
      )}
      {applyError && <span className="text-[11px] text-danger">Saved, but not applied: {applyError}</span>}
    </div>
  )
}

export function ErrorLine({ error }: { error: unknown }) {
  if (error instanceof SettingsApiError && error.code === 'ssd_busy')
    return (
      <p className="text-[11px] text-danger">
        Not saved: a scan or ingest is using the SSD. Wait for it to finish, then change the SSD path or UUID.
      </p>
    )
  return <p className="text-[11px] text-danger">{error instanceof Error ? error.message : 'Request failed.'}</p>
}
