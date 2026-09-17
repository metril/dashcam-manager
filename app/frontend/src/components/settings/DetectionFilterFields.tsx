// DetectionFilterFields: the min-confidence slider, kind toggles, and
// class-toggle grid shared between DetectionFilterMenu's popover (player
// toolbar) and the Settings page's "Detections" card. Pure controlled
// fields — no popover chrome, no persistence — callers own filter/onChange
// and any Save/Reset actions around it.
import type { DetectionFilter } from '../../lib/api'
import Switch from '../ui/Switch'

// DETECTION_CLASSES mirrors internal/scan.DetectionClasses — the object
// classes the detection model actually emits (cmd/api's
// validDetectionClass rejects anything else in a filter's `classes` list).
export const DETECTION_CLASSES = ['car', 'truck', 'bus', 'person', 'bicycle', 'motorcycle']

function titleCase(s: string): string {
  return s.length === 0 ? s : s[0].toUpperCase() + s.slice(1)
}

export default function DetectionFilterFields({
  filter,
  onChange,
}: {
  filter: DetectionFilter
  onChange: (filter: DetectionFilter) => void
}) {
  const allClasses = filter.classes.length === 0

  // toggleClass: an empty `classes` list means "every class is on," so
  // unchecking one box from that all-on state must materialize the other
  // classes explicitly (see DetectionFilterMenu's original fix-round-1
  // note for the bug this avoids). Re-selecting every class collapses
  // back to [] so a full-again filter still means "no restriction,"
  // matching the server's DetectionFilter.Classes doc.
  function toggleClass(cls: string) {
    const current = allClasses ? DETECTION_CLASSES : filter.classes
    const next = current.includes(cls) ? current.filter((c) => c !== cls) : [...current, cls]
    onChange({ ...filter, classes: next.length === DETECTION_CLASSES.length ? [] : next })
  }

  return (
    <div className="flex flex-col gap-3 text-[12px]">
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
            <Switch
              checked={filter.objects}
              onChange={(checked) => onChange({ ...filter, objects: checked })}
              label="Objects"
            />
            Objects
          </label>
          <label className="flex items-center gap-1.5 text-text-secondary">
            <Switch
              checked={filter.plates}
              onChange={(checked) => onChange({ ...filter, plates: checked })}
              label="Plates"
            />
            Plates
          </label>
          <label className="flex items-center gap-1.5 text-text-secondary">
            <Switch
              checked={filter.vehicles}
              onChange={(checked) => onChange({ ...filter, vehicles: checked })}
              label="Vehicles"
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
              <Switch
                checked={allClasses || filter.classes.includes(cls)}
                onChange={() => toggleClass(cls)}
                label={titleCase(cls)}
              />
              {titleCase(cls)}
            </label>
          ))}
        </div>
      </div>
    </div>
  )
}
