// Segmented: a segmented control for single- or multi-select from a small
// fixed option set — replaces ad hoc pill/chip button groups. role="group"
// with aria-pressed buttons is the ARIA pattern for a toggle-button group
// (unlike radiogroup/radio, which implies exactly one must always be
// selected — not true for the multi-select case here). Left/Right arrow
// keys move focus between options, matching a native toolbar/tablist feel;
// selection itself is still a plain click/Space/Enter on the focused
// button, not implicit on arrow movement, since multi-select can't follow
// the "arrow selects" convention single-select radiogroups use.
import type { KeyboardEvent, ReactNode } from 'react'

export interface SegmentedOption<T extends string> {
  value: T
  label: ReactNode
}

interface SegmentedBaseProps<T extends string> {
  options: SegmentedOption<T>[]
  'aria-label': string
  className?: string
}

interface SegmentedSingleProps<T extends string> extends SegmentedBaseProps<T> {
  multiple?: false
  value: T
  onChange: (value: T) => void
}

interface SegmentedMultiProps<T extends string> extends SegmentedBaseProps<T> {
  multiple: true
  value: T[]
  onChange: (value: T[]) => void
}

export type SegmentedProps<T extends string> = SegmentedSingleProps<T> | SegmentedMultiProps<T>

export default function Segmented<T extends string>(props: SegmentedProps<T>) {
  const { options, className } = props
  const ariaLabel = props['aria-label']

  function isActive(v: T): boolean {
    return props.multiple ? props.value.includes(v) : props.value === v
  }

  function select(v: T) {
    if (props.multiple) {
      const next = props.value.includes(v) ? props.value.filter((x) => x !== v) : [...props.value, v]
      props.onChange(next)
    } else {
      props.onChange(v)
    }
  }

  function onKeyDown(e: KeyboardEvent<HTMLButtonElement>, idx: number) {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return
    e.preventDefault()
    const dir = e.key === 'ArrowRight' ? 1 : -1
    const nextIdx = (idx + dir + options.length) % options.length
    const group = e.currentTarget.closest('[role="group"]')
    const buttons = group?.querySelectorAll<HTMLButtonElement>('button')
    buttons?.[nextIdx]?.focus()
  }

  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={[
        'inline-flex flex-wrap gap-1 rounded-md border border-border bg-surface p-0.5',
        className ?? '',
      ].join(' ')}
    >
      {options.map((opt, idx) => {
        const active = isActive(opt.value)
        return (
          <button
            key={opt.value}
            type="button"
            aria-pressed={active}
            onClick={() => select(opt.value)}
            onKeyDown={(e) => onKeyDown(e, idx)}
            className={[
              'flex items-center gap-1 rounded px-2 py-1 text-xs font-medium transition-colors',
              active ? 'bg-accent/15 text-accent' : 'text-text-muted hover:text-text',
            ].join(' ')}
          >
            {opt.label}
          </button>
        )
      })}
    </div>
  )
}
