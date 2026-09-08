// M12 Search page: a search box + filter chips (class multi-select, make,
// plate, date range, min confidence, min speed), results as a grid of
// segment cards; clicking a hit deep-links to the trip page at that
// segment/timestamp. "/" focuses the search box from anywhere in the app
// (Layout.tsx navigates here; this page autofocuses on mount and also
// handles its own "/" keydown for refocusing while already here).
import { useEffect, useRef, useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { Loader2, Search as SearchIcon, X } from 'lucide-react'
import { useNavigate } from 'react-router-dom'
import { search } from '../lib/api'
import { formatClockTime, formatDateLong } from '../lib/format'
import type { SearchHit, SearchResult } from '../types'

const CLASS_OPTIONS = ['car', 'truck', 'bus', 'person', 'bicycle', 'motorcycle']

const KIND_LABEL: Record<string, string> = {
  class: 'object',
  plate: 'plate',
  make: 'make',
  type: 'type',
}

// useDebouncedValue mirrors `value` after `delayMs` of no further changes —
// used for the free-text/make/plate inputs so every keystroke doesn't fire
// a request.
function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])
  return debounced
}

function Chip({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={[
        'rounded-full border px-2.5 py-1 text-[11px] font-medium transition-colors',
        active
          ? 'border-amber-500 bg-amber-500/10 text-amber-400'
          : 'border-neutral-800 text-neutral-400 hover:border-neutral-700 hover:text-neutral-200',
      ].join(' ')}
    >
      {children}
    </button>
  )
}

function HitChip({ hit, onClick }: { hit: SearchHit; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex items-center gap-1 rounded border border-neutral-800 bg-neutral-900/60 px-1.5 py-0.5 text-[10px] text-neutral-300 hover:border-amber-600 hover:text-amber-300"
      title={`${KIND_LABEL[hit.kind] ?? hit.kind}: ${hit.text} @ ${hit.pts_offset.toFixed(1)}s (${Math.round(hit.confidence * 100)}%)`}
    >
      <span className="text-neutral-600">{KIND_LABEL[hit.kind] ?? hit.kind}</span>
      <span className="font-medium">{hit.text}</span>
    </button>
  )
}

function ResultCard({ result, onOpen }: { result: SearchResult; onOpen: (pts?: number) => void }) {
  const [thumbFailed, setThumbFailed] = useState(false)
  return (
    <div className="flex flex-col gap-2 rounded-lg border border-neutral-800 bg-neutral-900/40 p-2.5">
      <button
        type="button"
        onClick={() => onOpen()}
        className="flex h-28 items-center justify-center overflow-hidden rounded bg-neutral-800/80"
      >
        {!thumbFailed ? (
          <img
            src={result.thumb}
            alt=""
            loading="lazy"
            className="h-full w-full object-cover"
            onError={() => setThumbFailed(true)}
          />
        ) : (
          <SearchIcon className="size-4 text-neutral-600" />
        )}
      </button>
      <div className="flex items-center justify-between text-[11px] text-neutral-500">
        <span>{formatDateLong(result.start_time)}</span>
        <span className="tabular-nums">{formatClockTime(result.start_time)}</span>
      </div>
      <div className="flex flex-wrap gap-1">
        {result.hits.map((hit, i) => (
          <HitChip key={i} hit={hit} onClick={() => onOpen(hit.pts_offset)} />
        ))}
      </div>
    </div>
  )
}

// minTrigramChars mirrors search_fts's tokenize='trigram' index: a MATCH
// against fewer than 3 characters can't use the trigram index and either
// returns nothing useful or (depending on the query) errors, so the box
// hints at the minimum rather than silently doing nothing.
const minTrigramChars = 3

export default function Search() {
  const navigate = useNavigate()
  const inputRef = useRef<HTMLInputElement>(null)

  const [q, setQ] = useState('')
  const [classes, setClasses] = useState<Set<string>>(new Set())
  const [make, setMake] = useState('')
  const [plate, setPlate] = useState('')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [minConf, setMinConf] = useState(0)
  const [minSpeed, setMinSpeed] = useState(0)

  const debouncedQ = useDebouncedValue(q, 300)
  const debouncedMake = useDebouncedValue(make, 300)
  const debouncedPlate = useDebouncedValue(plate, 300)

  // Autofocus whenever this page mounts (covers both the app-wide "/"
  // shortcut navigating here from elsewhere, and a plain click on the
  // Search nav item) plus its own "/" listener for refocusing without a
  // remount while already here.
  useEffect(() => {
    inputRef.current?.focus()
  }, [])
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key !== '/' || e.target instanceof HTMLInputElement || e.target instanceof HTMLTextAreaElement) return
      e.preventDefault()
      inputRef.current?.focus()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const effectiveQ = debouncedQ.length >= minTrigramChars ? debouncedQ : ''
  const classList = [...classes].sort()

  const hasFilter = effectiveQ || classList.length > 0 || debouncedMake || debouncedPlate || minConf > 0 || minSpeed > 0

  const { data, isLoading, isFetchingNextPage, fetchNextPage, hasNextPage, isError } = useInfiniteQuery({
    queryKey: ['search', effectiveQ, classList, debouncedMake, debouncedPlate, from, to, minConf, minSpeed],
    queryFn: ({ pageParam }) =>
      search({
        q: effectiveQ || undefined,
        classes: classList.length > 0 ? classList : undefined,
        make: debouncedMake || undefined,
        plate: debouncedPlate || undefined,
        from: from || undefined,
        to: to || undefined,
        min_conf: minConf || undefined,
        min_speed: minSpeed || undefined,
        limit: 60,
        cursor: pageParam,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: Boolean(hasFilter),
  })

  function toggleClass(cls: string) {
    setClasses((prev) => {
      const next = new Set(prev)
      if (next.has(cls)) next.delete(cls)
      else next.add(cls)
      return next
    })
  }

  function openResult(result: SearchResult, ptsOffset?: number) {
    if (!result.trip) return
    const params = new URLSearchParams({ seg: String(result.segment.id) })
    if (ptsOffset != null) params.set('t', String(ptsOffset))
    navigate(`/trips/${result.trip.id}?${params}`)
  }

  const results = data?.pages.flatMap((page) => page.results) ?? []

  return (
    <div className="mx-auto flex max-w-6xl flex-col gap-5 px-8 py-8">
      <h1 className="text-lg font-semibold text-neutral-100">Search</h1>

      <div className="flex flex-col gap-3 rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
        <div className="relative">
          <SearchIcon className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-neutral-600" />
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search plates, makes, object classes… (press / to focus)"
            className="w-full rounded-md border border-neutral-800 bg-neutral-950 py-2 pl-9 pr-9 text-[13px] text-neutral-100 outline-none placeholder:text-neutral-600 focus:border-amber-600"
          />
          {q && (
            <button
              type="button"
              onClick={() => setQ('')}
              className="absolute right-3 top-1/2 -translate-y-1/2 text-neutral-600 hover:text-neutral-300"
            >
              <X className="size-4" />
            </button>
          )}
        </div>
        {q.length > 0 && q.length < minTrigramChars && (
          <span className="text-[11px] text-neutral-600">type at least {minTrigramChars} characters to search text</span>
        )}

        <div className="flex flex-wrap items-center gap-1.5">
          {CLASS_OPTIONS.map((cls) => (
            <Chip key={cls} active={classes.has(cls)} onClick={() => toggleClass(cls)}>
              {cls}
            </Chip>
          ))}
        </div>

        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-[11px] text-neutral-500">
            make
            <input
              value={make}
              onChange={(e) => setMake(e.target.value)}
              placeholder="Toyota"
              className="w-28 rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-[12px] text-neutral-200 outline-none focus:border-amber-600"
            />
          </label>
          <label className="flex flex-col gap-1 text-[11px] text-neutral-500">
            plate
            <input
              value={plate}
              onChange={(e) => setPlate(e.target.value)}
              placeholder="ABC123"
              className="w-28 rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-[12px] text-neutral-200 outline-none focus:border-amber-600"
            />
          </label>
          <label className="flex flex-col gap-1 text-[11px] text-neutral-500">
            from
            <input
              type="date"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              className="rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-[12px] text-neutral-200 outline-none focus:border-amber-600"
            />
          </label>
          <label className="flex flex-col gap-1 text-[11px] text-neutral-500">
            to
            <input
              type="date"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className="rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-[12px] text-neutral-200 outline-none focus:border-amber-600"
            />
          </label>
          <label className="flex flex-col gap-1 text-[11px] text-neutral-500">
            min confidence: {Math.round(minConf * 100)}%
            <input
              type="range"
              min={0}
              max={1}
              step={0.05}
              value={minConf}
              onChange={(e) => setMinConf(Number(e.target.value))}
              className="w-28"
            />
          </label>
          <label className="flex flex-col gap-1 text-[11px] text-neutral-500">
            min speed: {Math.round(minSpeed * 3.6)} km/h
            <input
              type="range"
              min={0}
              max={40}
              step={1}
              value={minSpeed}
              onChange={(e) => setMinSpeed(Number(e.target.value))}
              className="w-28"
            />
          </label>
        </div>
      </div>

      {!hasFilter && (
        <div className="flex flex-col items-center justify-center gap-2 py-16 text-neutral-600">
          <SearchIcon className="size-6" />
          <span className="text-[13px]">enter a search term or pick a filter to get started</span>
        </div>
      )}

      {isLoading && (
        <div className="flex items-center justify-center py-16 text-neutral-600">
          <Loader2 className="size-5 animate-spin" />
        </div>
      )}

      {isError && <div className="py-16 text-center text-[13px] text-neutral-500">search failed</div>}

      {hasFilter && !isLoading && !isError && results.length === 0 && (
        <div className="py-16 text-center text-[13px] text-neutral-600">no matches</div>
      )}

      {results.length > 0 && (
        <>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5">
            {results.map((r) => (
              <ResultCard key={r.segment.id} result={r} onOpen={(pts) => openResult(r, pts)} />
            ))}
          </div>
          {hasNextPage && (
            <button
              type="button"
              onClick={() => fetchNextPage()}
              disabled={isFetchingNextPage}
              className="mx-auto flex items-center gap-2 rounded-md border border-neutral-800 px-4 py-2 text-[12px] font-medium text-neutral-300 hover:border-neutral-700 hover:text-neutral-100 disabled:opacity-50"
            >
              {isFetchingNextPage && <Loader2 className="size-3.5 animate-spin" />}
              load more
            </button>
          )}
        </>
      )}
    </div>
  )
}
