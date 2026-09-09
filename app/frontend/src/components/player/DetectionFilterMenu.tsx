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

  const saveMutation = useMutation({ mutationFn: () => setDetectionFilter(filter) })

  function toggleClass(cls: string) {
    const has = filter.classes.includes(cls)
    onChange({ ...filter, classes: has ? filter.classes.filter((c) => c !== cls) : [...filter.classes, cls] })
  }

  const allClasses = filter.classes.length === 0

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className={[
          'flex items-center gap-1 rounded-md border px-2 py-1.5 text-[12px] hover:border-text-muted',
          open ? 'border-accent text-accent' : 'border-border text-text-muted',
        ].join(' ')}
      >
        <Filter className="size-3.5" /> Filter
      </button>

      {open && (
        <div className="absolute bottom-full left-0 z-20 mb-2 flex w-64 flex-col gap-3 rounded-lg border border-border bg-surface p-3 text-[12px] shadow-lg">
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
