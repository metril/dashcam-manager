// Accessible tooltip (WP5): shows a short label on hover or keyboard focus,
// replacing bare title= attributes on icon buttons (title= never fires for
// keyboard users and offers no styling). Wraps a single focusable child and
// attaches aria-describedby so screen readers announce the label too.
import { cloneElement, useId, useState, type KeyboardEvent, type ReactElement } from 'react'

interface TooltipProps {
  content: string
  children: ReactElement<Record<string, unknown>>
  side?: 'top' | 'bottom' | 'left' | 'right'
}

const SIDE_CLASSES: Record<NonNullable<TooltipProps['side']>, string> = {
  top: 'bottom-full left-1/2 mb-1.5 -translate-x-1/2',
  bottom: 'top-full left-1/2 mt-1.5 -translate-x-1/2',
  left: 'right-full top-1/2 mr-1.5 -translate-y-1/2',
  right: 'left-full top-1/2 ml-1.5 -translate-y-1/2',
}

export default function Tooltip({ content, children, side = 'top' }: TooltipProps) {
  const id = useId()
  const [visible, setVisible] = useState(false)

  function onKeyDown(e: KeyboardEvent<HTMLSpanElement>) {
    if (e.key === 'Escape') setVisible(false)
  }

  return (
    <span
      className="relative inline-flex"
      onMouseEnter={() => setVisible(true)}
      onMouseLeave={() => setVisible(false)}
      onFocus={() => setVisible(true)}
      onBlur={() => setVisible(false)}
      onKeyDown={onKeyDown}
    >
      {cloneElement(children, { 'aria-describedby': id })}
      <span
        role="tooltip"
        id={id}
        className={[
          'pointer-events-none absolute z-50 whitespace-nowrap rounded-md border border-neutral-800 bg-neutral-900 px-2 py-1 text-xs font-medium text-neutral-200 shadow-lg transition-opacity duration-100',
          visible ? 'opacity-100' : 'opacity-0',
          SIDE_CLASSES[side],
        ].join(' ')}
      >
        {content}
      </span>
    </span>
  )
}
