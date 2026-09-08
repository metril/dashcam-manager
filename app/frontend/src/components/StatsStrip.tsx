import { useQuery } from '@tanstack/react-query'
import { Database, Film, HardDrive, Route } from 'lucide-react'
import { getStats } from '../lib/api'
import { formatBytes } from '../lib/format'

/** StatsStrip is the always-visible library-wide summary in the Browse header. */
export default function StatsStrip() {
  const { data } = useQuery({ queryKey: ['stats'], queryFn: getStats })

  const items = [
    { icon: Route, label: 'trips', value: data ? data.trips.toLocaleString() : '—' },
    { icon: Film, label: 'segments', value: data ? data.segments.toLocaleString() : '—' },
    { icon: HardDrive, label: 'stored', value: data ? formatBytes(data.bytes) : '—' },
    {
      icon: Database,
      label: 'objects',
      // Matches the backend's /api/calendar "objects" definition: vehicles/
      // people, not plate reads (by_class also carries a "plate" key).
      value: data
        ? Object.entries(data.by_class)
            .filter(([cls]) => cls !== 'plate')
            .reduce((sum, [, count]) => sum + count, 0)
            .toLocaleString()
        : '—',
    },
  ]

  return (
    <div className="flex items-center gap-5 rounded-lg border border-neutral-800 bg-neutral-900/50 px-4 py-2.5">
      {items.map((item) => (
        <div key={item.label} className="flex items-center gap-2">
          <item.icon className="size-3.5 text-neutral-600" strokeWidth={2} />
          <span className="tabular-nums text-[13px] font-semibold text-neutral-200">{item.value}</span>
          <span className="text-[11px] text-neutral-600">{item.label}</span>
        </div>
      ))}
    </div>
  )
}
