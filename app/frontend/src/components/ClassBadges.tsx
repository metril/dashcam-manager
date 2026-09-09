import { Bike, Bus, Car, IdCard, Motorbike, Truck, User } from 'lucide-react'
import type { ClassSummary } from '../types'

const classIcons: Record<string, typeof Car> = {
  car: Car,
  truck: Truck,
  bus: Bus,
  person: User,
  bicycle: Bike,
  motorcycle: Motorbike,
  plate: IdCard,
}

// classOrder pins a stable, meaningful reading order regardless of the
// object insertion order the API happens to return.
const classOrder = ['car', 'truck', 'bus', 'motorcycle', 'bicycle', 'person', 'plate']

interface Props {
  summary: ClassSummary
  className?: string
}

/** ClassBadges renders a compact row of detected-class counts, e.g. "3 car 1 person". */
export default function ClassBadges({ summary, className = '' }: Props) {
  const entries = classOrder
    .filter((cls) => summary[cls] > 0)
    .map((cls) => [cls, summary[cls]] as const)

  if (entries.length === 0) {
    return <span className={`text-xs text-neutral-400 ${className}`}>No detections</span>
  }

  return (
    <div className={`flex flex-wrap items-center gap-x-2.5 gap-y-1 ${className}`}>
      {entries.map(([cls, count]) => {
        const Icon = classIcons[cls] ?? Car
        return (
          <span key={cls} className="flex items-center gap-1 text-xs text-neutral-400">
            <Icon className="size-3" strokeWidth={2.25} />
            <span className="tabular-nums text-neutral-300">{count}</span>
          </span>
        )
      })}
    </div>
  )
}
