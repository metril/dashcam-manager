// CopiesBadge (local-storage plan task 11): renders a segment's or
// export's copies/on_ssd state as a short pill, with a tooltip listing the
// storages holding it. The recorder's SSD is never a storage — a segment
// still only on the SSD with no persistent copy shows "not ingested".
import { CheckCircle2, TriangleAlert } from 'lucide-react'
import Tooltip from './Tooltip'
import type { CopyRef } from '../types'

export default function CopiesBadge({
  copies,
  onSsd,
  className = '',
}: {
  copies: CopyRef[]
  onSsd?: boolean
  className?: string
}) {
  if (copies.length === 0) {
    if (!onSsd) return null // no copies and never on the SSD either: nothing to report
    return (
      <Tooltip content="Still only on the recorder's SSD, no persistent copy yet.">
        <span
          className={`flex w-fit items-center gap-1 rounded-sm bg-warn/15 px-1 py-0.5 text-xs font-medium text-warn ${className}`}
        >
          <TriangleAlert className="size-3" strokeWidth={2} />
          Not ingested
        </span>
      </Tooltip>
    )
  }

  const verified = copies.some((c) => c.verified)
  const label = copies.length === 1 ? 'On 1 storage' : `On ${copies.length} storages`
  const tooltip = copies
    .map((c) => `${c.label} (${c.rel_path})${c.verified ? ' — verified' : ''}`)
    .join(' · ')

  return (
    <Tooltip content={tooltip}>
      <span
        className={`flex w-fit items-center gap-1 rounded-sm bg-surface-2/80 px-1 py-0.5 text-xs font-medium text-text-secondary ${className}`}
      >
        {verified && <CheckCircle2 className="size-3 text-success" strokeWidth={2} />}
        {label}
      </span>
    </Tooltip>
  )
}
