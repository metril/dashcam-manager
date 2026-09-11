// JobResult: the "queued N, view job #id" feedback line shared by
// maintenance-style action buttons (Settings' maintenance card, the
// Dashcams page's pull/cleanup/reprocess actions) — every such POST
// handler returns a { status, count?, job? } or { status, job } shape.
import { Link } from 'react-router-dom'

export default function JobResult({ count, job }: { count?: number; job?: { id: number } }) {
  if (count === 0) return <p className="text-[11px] text-text-muted">Nothing to do.</p>
  return (
    <p className="text-[11px] text-text-muted">
      {count !== undefined ? `Queued ${count} segment${count === 1 ? '' : 's'}.` : 'Queued.'}{' '}
      {job && (
        <Link to="/system/jobs" className="text-accent hover:underline">
          View job #{job.id}
        </Link>
      )}
    </p>
  )
}
