// M12 Stats dashboard: storage per source/share, bytes-by-month bar chart,
// segments by camera, object class histogram, ingest coverage — all as
// small inline SVG bars (no chart library).
import { useQuery } from '@tanstack/react-query'
import { Camera, Database, Gauge, HardDrive } from 'lucide-react'
import { getStats } from '../lib/api'
import { formatBytes, titleCase } from '../lib/format'

function BarRow({ label, value, max, formatValue }: { label: string; value: number; max: number; formatValue: (v: number) => string }) {
  const pct = max > 0 ? Math.max((value / max) * 100, value > 0 ? 2 : 0) : 0
  return (
    <div className="flex items-center gap-3">
      <span className="w-20 shrink-0 truncate text-xs text-neutral-500">{label}</span>
      <div className="h-4 flex-1 overflow-hidden rounded bg-neutral-900">
        <div className="h-full rounded bg-amber-500/70" style={{ width: `${pct}%` }} />
      </div>
      <span className="w-16 shrink-0 text-right text-xs tabular-nums text-neutral-400">{formatValue(value)}</span>
    </div>
  )
}

function Panel({ title, icon: Icon, children }: { title: string; icon: typeof Camera; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-3 rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
      <h2 className="flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-neutral-500">
        <Icon className="size-3.5" /> {title}
      </h2>
      <div className="flex flex-col gap-2">{children}</div>
    </div>
  )
}

// CircularCoverage renders a small ring gauge for ingest coverage
// percentage using plain inline SVG (circumference-based stroke-dash).
function CircularCoverage({ percent }: { percent: number }) {
  const r = 34
  const c = 2 * Math.PI * r
  const clamped = Math.max(0, Math.min(100, percent))
  return (
    <div className="flex items-center gap-4">
      <svg width={88} height={88} viewBox="0 0 88 88">
        <circle cx={44} cy={44} r={r} fill="none" stroke="#262626" strokeWidth={8} />
        <circle
          cx={44}
          cy={44}
          r={r}
          fill="none"
          stroke="#f59e0b"
          strokeWidth={8}
          strokeLinecap="round"
          strokeDasharray={c}
          strokeDashoffset={c - (clamped / 100) * c}
          transform="rotate(-90 44 44)"
        />
        <text x={44} y={49} textAnchor="middle" fontSize={16} fontWeight={600} fill="#e5e5e5">
          {Math.round(clamped)}%
        </text>
      </svg>
      <div className="text-[12px] text-neutral-500">
        of SSD-originated segments have a<br />verified copy on a share
      </div>
    </div>
  )
}

export default function Stats() {
  const { data, isLoading, isError } = useQuery({ queryKey: ['stats'], queryFn: getStats })

  if (isLoading) {
    return <div className="px-8 py-8 text-[13px] text-neutral-400">loading…</div>
  }
  if (isError || !data) {
    return <div className="px-8 py-8 text-[13px] text-neutral-400">couldn't load stats</div>
  }

  const maxMonthBytes = Math.max(1, ...data.by_month.map((m) => m.bytes))
  const maxCamera = Math.max(1, ...Object.values(data.by_camera))
  const maxClass = Math.max(1, ...Object.values(data.by_class))
  const maxSourceBytes = Math.max(1, ...data.by_source.map((s) => s.bytes))

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-5 px-8 py-8">
      <h1 className="text-lg font-semibold text-neutral-100">Stats</h1>

      <div className="flex flex-wrap items-center gap-5 rounded-lg border border-neutral-800 bg-neutral-900/50 px-4 py-2.5">
        <div className="flex items-center gap-2">
          <Database className="size-3.5 text-neutral-400" />
          <span className="tabular-nums text-[13px] font-semibold text-neutral-200">{data.segments.toLocaleString()}</span>
          <span className="text-xs text-neutral-400">segments</span>
        </div>
        <div className="flex items-center gap-2">
          <HardDrive className="size-3.5 text-neutral-400" />
          <span className="tabular-nums text-[13px] font-semibold text-neutral-200">{formatBytes(data.bytes)}</span>
          <span className="text-xs text-neutral-400">stored</span>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Panel title="Storage per source" icon={HardDrive}>
          {data.by_source.length === 0 && <span className="text-[12px] text-neutral-400">no sources yet</span>}
          {data.by_source.map((s) => (
            <BarRow key={s.source_id} label={s.label || titleCase(s.kind)} value={s.bytes} max={maxSourceBytes} formatValue={formatBytes} />
          ))}
        </Panel>

        <Panel title="Ingest coverage" icon={Gauge}>
          <CircularCoverage percent={data.ingested_percent} />
        </Panel>

        <Panel title="Bytes by month" icon={HardDrive}>
          {data.by_month.length === 0 && <span className="text-[12px] text-neutral-400">no data yet</span>}
          {data.by_month.map((m) => (
            <BarRow key={m.month} label={m.month} value={m.bytes} max={maxMonthBytes} formatValue={formatBytes} />
          ))}
        </Panel>

        <Panel title="Segments by camera" icon={Camera}>
          {Object.entries(data.by_camera).map(([cam, count]) => (
            <BarRow key={cam} label={cam} value={count} max={maxCamera} formatValue={(v) => v.toLocaleString()} />
          ))}
        </Panel>

        <Panel title="Object class histogram" icon={Database}>
          {Object.entries(data.by_class).length === 0 && <span className="text-[12px] text-neutral-400">no detections yet</span>}
          {Object.entries(data.by_class)
            .sort(([, a], [, b]) => b - a)
            .map(([cls, count]) => (
              <BarRow key={cls} label={cls} value={count} max={maxClass} formatValue={(v) => v.toLocaleString()} />
            ))}
        </Panel>
      </div>
    </div>
  )
}
