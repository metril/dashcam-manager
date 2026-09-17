// Switch: an accessible toggle replacing native <input type="checkbox">
// boolean controls app-wide (Settings, ExportPanel, DetectionFilterFields,
// Dashcams' ingest form). role="switch" + aria-checked is the ARIA pattern
// for a two-state on/off control (distinct from role="checkbox", which
// implies a tri-state/indeterminate option this app never uses); Space and
// Enter both toggle, matching native checkbox/button key handling.
import type { KeyboardEvent } from 'react'

interface SwitchProps {
  checked: boolean
  onChange: (checked: boolean) => void
  label?: string
  disabled?: boolean
  className?: string
}

export default function Switch({ checked, onChange, label, disabled, className }: SwitchProps) {
  function toggle() {
    if (!disabled) onChange(!checked)
  }

  function onKeyDown(e: KeyboardEvent<HTMLButtonElement>) {
    if (e.key === ' ' || e.key === 'Enter') {
      e.preventDefault()
      toggle()
    }
  }

  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={toggle}
      onKeyDown={onKeyDown}
      className={[
        'relative inline-flex h-4 w-8 shrink-0 items-center rounded-full border transition-colors',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent',
        checked ? 'border-accent bg-accent' : 'border-border bg-surface-2',
        disabled ? 'cursor-not-allowed opacity-40' : 'cursor-pointer',
        className ?? '',
      ].join(' ')}
    >
      <span
        className="inline-block size-3 rounded-full bg-white shadow-sm transition-transform"
        style={{ transform: `translateX(${checked ? 16 : 2}px)` }}
      />
    </button>
  )
}
