import type { CalendarDay } from '../types'
import { dateKey } from '../lib/format'
import Tooltip from './Tooltip'

const WEEKDAYS = ['Su', 'Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa']

interface Props {
  year: number
  month: number // 0-indexed, matches Date's getUTCMonth()
  days: Map<string, CalendarDay>
  selectedDate: string | null
  onSelectDate: (date: string) => void
}

/** CalendarGrid renders a month grid (UTC-dated cells, matching the API's
 * date grouping) with a footage-density dot per day that has trips. */
export default function CalendarGrid({ year, month, days, selectedDate, onSelectDate }: Props) {
  const firstOfMonth = new Date(Date.UTC(year, month, 1))
  const startWeekday = firstOfMonth.getUTCDay()
  const daysInMonth = new Date(Date.UTC(year, month + 1, 0)).getUTCDate()

  const cells: Array<{ date: Date; key: string } | null> = []
  for (let i = 0; i < startWeekday; i++) cells.push(null)
  for (let d = 1; d <= daysInMonth; d++) {
    const date = new Date(Date.UTC(year, month, d))
    cells.push({ date, key: dateKey(date) })
  }

  const todayKey = dateKey(new Date())
  const maxSegments = Math.max(1, ...Array.from(days.values(), (d) => d.segments))

  return (
    <div>
      <div className="grid grid-cols-7 gap-1.5 pb-1.5 text-center text-xs font-medium uppercase tracking-wider text-neutral-400">
        {WEEKDAYS.map((w) => (
          <div key={w}>{w}</div>
        ))}
      </div>
      <div className="grid grid-cols-7 gap-1.5">
        {cells.map((cell, i) => {
          if (!cell) return <div key={`empty-${i}`} />
          const day = days.get(cell.key)
          const isSelected = selectedDate === cell.key
          const isToday = cell.key === todayKey
          const intensity = day ? Math.max(0.22, day.segments / maxSegments) : 0

          const dayButton = (
            <button
              type="button"
              disabled={!day}
              onClick={() => day && onSelectDate(cell.key)}
              className={[
                'group relative flex aspect-square flex-col items-center justify-center rounded-md border text-[12px] transition-colors',
                day
                  ? isSelected
                    ? 'border-amber-500 bg-amber-500/15 text-amber-300'
                    : 'border-neutral-800 text-neutral-200 hover:border-amber-500/60 hover:bg-neutral-900'
                  : 'border-transparent text-neutral-400',
                isToday && !isSelected ? 'ring-1 ring-inset ring-neutral-600' : '',
              ].join(' ')}
            >
              <span className="tabular-nums">{cell.date.getUTCDate()}</span>
              {day && (
                <span
                  className="absolute bottom-1.5 h-1 w-1 rounded-full bg-amber-500"
                  style={{ opacity: intensity }}
                />
              )}
            </button>
          )

          if (!day) return <span key={cell.key}>{dayButton}</span>

          return (
            <Tooltip key={cell.key} content={`${day.trips} trip${day.trips === 1 ? '' : 's'}, ${day.segments} segments`}>
              {dayButton}
            </Tooltip>
          )
        })}
      </div>
    </div>
  )
}
