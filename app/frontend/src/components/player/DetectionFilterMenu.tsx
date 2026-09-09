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

// DETECTION_CLASSES mirrors internal/scan.DetectionClasses — the object
// classes the detection model actually emits (cmd/api's
// validDetectionClass rejects anything else in a filter's `classes` list).
const DETECTION_CLASSES = ['car', 'truck', 'bus', 'person', 'bicycle', 'motorcycle']

function titleCase(s: string): string {
  return s.length === 0 ? s : s[0].toUpperCase() + s.slice(1)
}

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

  const allClasses = filter.classes.length === 0

  // toggleClass: fix round 1 — an empty `classes` list means "every class
  // is on," so unchecking one box from that all-on state must materialize
  // the *other* classes explicitly (every class minus the one just
  // clicked), not toggle membership in the empty list itself. The old
  // `includes(cls) ? remove : add` logic treated "not present in []" as
  // "not selected," so unchecking Car while every box reads checked
  // actually *added* 'car' to classes — collapsing the filter down to
  // "only cars" instead of "everything but cars." Any subsequent toggle
  // works off the concrete (possibly all-classes) list the same way, and
  // re-selecting every class collapses back to [] so a full-again filter
  // still means "no restriction," matching what the server treats classes:
  // [] as (cmd/api/detection_filter.go's DetectionFilter.Classes doc).
  //
  // Manual check (no frontend test runner is configured in this repo —
  // see package.json's scripts — so this isn't unit-tested):
  //   1. Open the Filter menu with a fresh/default filter (classes: []).
  //   2. Uncheck "Car" — expect every *other* class to stay checked and
  //      only Car to become unchecked (not the reverse).
  //   3. Re-check "Car" — expect every class to read checked again, and
  //      the filter's classes list to be [] (not a 6-item list) so the
  //      "(all)" label in the section header comes back.
  function toggleClass(cls: string) {
    const current = allClasses ? DETECTION_CLASSES : filter.classes
    const next = current.includes(cls) ? current.filter((c) => c !== cls) : [...current, cls]
    onChange({ ...filter, classes: next.length === DETECTION_CLASSES.length ? [] : next })
  }

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
          <label className="flex flex-col gap-1 text-text-muted">
            Min confidence: {Math.round(filter.min_conf * 100)}%
            <input
              type="range"
              min={0}
              max={1}
              step={0.05}
              value={filter.min_conf}
              onChange={(e) => onChange({ ...filter, min_conf: Number(e.target.value) })}
            />
          </label>

          <div className="flex flex-col gap-1.5">
            <span className="text-text-muted">Kinds</span>
            <div className="flex flex-wrap gap-3">
              <label className="flex items-center gap-1.5 text-text-secondary">
                <input
                  type="checkbox"
                  checked={filter.objects}
                  onChange={(e) => onChange({ ...filter, objects: e.target.checked })}
                />
                Objects
              </label>
              <label className="flex items-center gap-1.5 text-text-secondary">
                <input
                  type="checkbox"
                  checked={filter.plates}
                  onChange={(e) => onChange({ ...filter, plates: e.target.checked })}
                />
                Plates
              </label>
              <label className="flex items-center gap-1.5 text-text-secondary">
                <input
                  type="checkbox"
                  checked={filter.vehicles}
                  onChange={(e) => onChange({ ...filter, vehicles: e.target.checked })}
                />
                Vehicles
              </label>
            </div>
          </div>

          <div className="flex flex-col gap-1.5">
            <span className="text-text-muted">Object classes ({allClasses ? 'all' : filter.classes.length})</span>
            <div className="grid grid-cols-2 gap-1">
              {DETECTION_CLASSES.map((cls) => (
                <label key={cls} className="flex items-center gap-1.5 text-text-secondary">
                  <input
                    type="checkbox"
                    checked={allClasses || filter.classes.includes(cls)}
                    onChange={() => toggleClass(cls)}
                  />
                  {titleCase(cls)}
                </label>
              ))}
            </div>
          </div>

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
