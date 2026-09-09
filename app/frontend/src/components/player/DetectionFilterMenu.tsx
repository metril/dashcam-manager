// DetectionFilterMenu: a popover (opened from a toolbar Filter button in
// DualVideoPlayer) editing the live DetectionFilter — confidence floor,
// which kinds show, and which object classes show. Edits apply live via
// the passed filter/onChange (backed by useDetectionFilter); "Save as
// default" additionally persists the current value server-side so future
// sessions/exports start from it.
import { useEffect, useRef, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Filter, RotateCcw, Save } from 'lucide-react'
import { defaultDetectionFilter, setDetectionFilter, type DetectionFilter } from '../../lib/api'
import DetectionFilterFields from '../settings/DetectionFilterFields'

export default function DetectionFilterMenu({
  filter,
  onChange,
}: {
  filter: DetectionFilter
  onChange: (filter: DetectionFilter) => void
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  // Tracks whether the popover was actually open on the previous render so
  // the focus-return effect below only fires on a real close (open ->
  // closed), never on mount or while it stays open/closed across renders.
  const wasOpenRef = useRef(false)

  useEffect(() => {
    if (!open) return
    function onDown(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(false)
    }
    window.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
  }, [open])

  // Return focus to the trigger button whenever the popover closes
  // (Escape, outside click, or Reset/Save) — otherwise focus is dropped
  // onto <body> when the popover's DOM unmounts, which is a keyboard-nav
  // dead end.
  useEffect(() => {
    if (!open && wasOpenRef.current) triggerRef.current?.focus()
    wasOpenRef.current = open
  }, [open])

  const saveMutation = useMutation({ mutationFn: () => setDetectionFilter(filter) })

  return (
    <div ref={ref} className="relative">
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="dialog"
        aria-expanded={open}
        className={[
          'flex items-center gap-1 rounded-md border px-2 py-1.5 text-[12px] hover:border-text-muted',
          open ? 'border-accent text-accent' : 'border-border text-text-muted',
        ].join(' ')}
      >
        <Filter className="size-3.5" /> Filter
      </button>

      {open && (
        <div
          role="dialog"
          aria-label="Detection filter"
          className="absolute bottom-full left-0 z-20 mb-2 flex w-64 flex-col gap-3 rounded-lg border border-border bg-surface p-3 text-[12px] shadow-lg">
          <DetectionFilterFields filter={filter} onChange={onChange} />

          <div className="flex items-center gap-2 border-t border-border pt-2">
            <button
              type="button"
              onClick={() => onChange(defaultDetectionFilter)}
              className="flex items-center gap-1 rounded-md border border-border px-2 py-1 text-text-secondary hover:border-text-muted"
            >
              <RotateCcw className="size-3" /> Reset
            </button>
            <button
              type="button"
              onClick={() => saveMutation.mutate()}
              className="ml-auto flex items-center gap-1 rounded-md border border-accent/30 bg-accent/10 px-2 py-1 text-accent hover:bg-accent/20"
            >
              <Save className="size-3" /> {saveMutation.isPending ? 'Saving…' : saveMutation.isSuccess ? 'Saved' : 'Save as default'}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
