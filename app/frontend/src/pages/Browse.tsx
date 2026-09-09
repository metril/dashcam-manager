import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, Loader2 } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { getCalendar, getTrips } from '../lib/api'
import { monthLabel } from '../lib/format'
import type { CalendarDay } from '../types'
import CalendarGrid from '../components/CalendarGrid'
import StatsStrip from '../components/StatsStrip'
import TripCardRow from '../components/TripCardRow'

function monthRange(year: number, month: number) {
  const from = new Date(Date.UTC(year, month, 1))
  const to = new Date(Date.UTC(year, month + 1, 0))
  const fmt = (d: Date) => d.toISOString().slice(0, 10)
  return { from: fmt(from), to: fmt(to) }
}

export default function Browse() {
  const now = useMemo(() => new Date(), [])
  const [year, setYear] = useState(now.getUTCFullYear())
  const [month, setMonth] = useState(now.getUTCMonth())
  const [selectedDate, setSelectedDate] = useState<string | null>(null)

  const { from, to } = useMemo(() => monthRange(year, month), [year, month])

  const calendarQuery = useQuery({
    queryKey: ['calendar', from, to],
    queryFn: () => getCalendar(from, to),
  })

  const tripsQuery = useInfiniteQuery({
    queryKey: ['trips', selectedDate],
    queryFn: ({ pageParam }) => getTrips({ date: selectedDate!, cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: selectedDate != null,
  })
  const trips = useMemo(() => tripsQuery.data?.pages.flatMap((page) => page.trips) ?? [], [tripsQuery.data])

  function goMonth(delta: number) {
    const next = new Date(Date.UTC(year, month + delta, 1))
    setYear(next.getUTCFullYear())
    setMonth(next.getUTCMonth())
    setSelectedDate(null)
  }

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return
      if (e.key === 'ArrowLeft') goMonth(-1)
      if (e.key === 'ArrowRight') goMonth(1)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [year, month])

  const daysByDate = useMemo(() => {
    const m = new Map<string, CalendarDay>()
    for (const d of calendarQuery.data ?? []) m.set(d.date, d)
    return m
  }, [calendarQuery.data])

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-6 px-8 py-8">
      <header className="flex items-center justify-between">
        <h1 className="text-lg font-semibold text-neutral-100">Browse</h1>
        <StatsStrip />
      </header>

      <div className="grid grid-cols-[minmax(0,320px)_1fr] gap-8">
        <section>
          <div className="mb-3 flex items-center justify-between">
            <span className="text-[13px] font-semibold text-neutral-300">{monthLabel(year, month)}</span>
            <div className="flex items-center gap-1">
              <button
                type="button"
                onClick={() => goMonth(-1)}
                className="rounded-md p-1.5 text-neutral-500 hover:bg-neutral-900 hover:text-neutral-200"
                aria-label="Previous month"
              >
                <ChevronLeft className="size-4" />
              </button>
              <button
                type="button"
                onClick={() => goMonth(1)}
                className="rounded-md p-1.5 text-neutral-500 hover:bg-neutral-900 hover:text-neutral-200"
                aria-label="Next month"
              >
                <ChevronRight className="size-4" />
              </button>
            </div>
          </div>

          {calendarQuery.isLoading ? (
            <div className="flex h-48 items-center justify-center text-neutral-400">
              <Loader2 className="size-5 animate-spin" />
            </div>
          ) : calendarQuery.isError ? (
            <div className="flex h-48 flex-col items-center justify-center gap-1 rounded-lg border border-dashed border-red-900/60 text-[13px] text-red-400">
              <span>Couldn't load the calendar</span>
              <span className="text-xs text-neutral-400">
                {calendarQuery.error instanceof Error ? calendarQuery.error.message : 'Unknown error'}
              </span>
            </div>
          ) : (
            <CalendarGrid
              year={year}
              month={month}
              days={daysByDate}
              selectedDate={selectedDate}
              onSelectDate={setSelectedDate}
            />
          )}
          <p className="mt-3 text-xs text-neutral-400">
            Use <kbd className="rounded bg-neutral-900 px-1 py-0.5">&larr;</kbd> /{' '}
            <kbd className="rounded bg-neutral-900 px-1 py-0.5">&rarr;</kbd> to change month.
          </p>
        </section>

        <section className="flex flex-col gap-2.5">
          {!selectedDate && (
            <div className="flex h-48 items-center justify-center rounded-lg border border-dashed border-neutral-800 text-[13px] text-neutral-400">
              Pick a highlighted day to see its trips
            </div>
          )}

          {selectedDate && tripsQuery.isLoading && (
            <div className="flex h-48 items-center justify-center text-neutral-400">
              <Loader2 className="size-5 animate-spin" />
            </div>
          )}

          {selectedDate && tripsQuery.isError && (
            <div className="flex h-32 flex-col items-center justify-center gap-1 rounded-lg border border-dashed border-red-900/60 text-[13px] text-red-400">
              <span>Couldn't load trips for {selectedDate}</span>
              <span className="text-xs text-neutral-400">
                {tripsQuery.error instanceof Error ? tripsQuery.error.message : 'Unknown error'}
              </span>
            </div>
          )}

          {selectedDate && tripsQuery.data && (
            <>
              <div className="flex items-center justify-between">
                <h2 className="text-[13px] font-semibold text-neutral-300">
                  {selectedDate} &middot; {trips.length} trip
                  {trips.length === 1 ? '' : 's'}
                </h2>
              </div>
              {trips.length === 0 ? (
                <div className="flex h-32 items-center justify-center rounded-lg border border-dashed border-neutral-800 text-[13px] text-neutral-400">
                  No trips this day
                </div>
              ) : (
                <>
                  {trips.map((trip) => (
                    <TripCardRow key={trip.id} trip={trip} />
                  ))}
                  {tripsQuery.hasNextPage && (
                    <button
                      type="button"
                      onClick={() => tripsQuery.fetchNextPage()}
                      disabled={tripsQuery.isFetchingNextPage}
                      className="mt-1 flex items-center justify-center gap-2 rounded-lg border border-neutral-800 bg-neutral-900/40 py-2 text-[12px] font-medium text-neutral-400 hover:border-neutral-700 hover:text-neutral-200 disabled:opacity-50"
                    >
                      {tripsQuery.isFetchingNextPage ? (
                        <Loader2 className="size-3.5 animate-spin" />
                      ) : (
                        'Load more'
                      )}
                    </button>
                  )}
                </>
              )}
            </>
          )}
        </section>
      </div>
    </div>
  )
}
