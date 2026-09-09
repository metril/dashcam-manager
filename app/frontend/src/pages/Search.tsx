// M12 Search page: a search box + filter chips (class multi-select, make,
// type, plate, date range, min confidence, min speed), results as a grid of
// segment cards; clicking a hit deep-links to the trip page at that
// segment/timestamp. "/" focuses the search box from anywhere in the app
// (Layout.tsx navigates here; this page autofocuses on mount and also
// handles its own "/" keydown for refocusing while already here).
//
// WP8 follow-up: every filter is read from the URL's search params on
// mount (so a Catalog row link like `?make=Toyota` lands here pre-filtered)
// and written back to the URL (via `replace`, not `push`, so filter
// changes don't pile up browser-history entries) whenever it changes — this
// also makes a given search shareable as a plain link. With no params
// present this behaves exactly as before (every filter starts empty).
import { useEffect, useRef, useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { Loader2, Search as SearchIcon, TriangleAlert, X } from 'lucide-react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { search } from '../lib/api'
import { formatClockTime, formatDateLong, titleCase } from '../lib/format'
import Tooltip from '../components/Tooltip'
import type { SearchHit, SearchResult } from '../types'

const CLASS_OPTIONS = ['car', 'truck', 'bus', 'person', 'bicycle', 'motorcycle']

const KIND_LABEL: Record<string, string> = {
  class: 'Object',
  plate: 'Plate',
  make: 'Make',
  type: 'Type',
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
        'rounded-full border px-2.5 py-1 text-xs font-medium transition-colors',
        active
          ? 'border-accent bg-accent/10 text-accent'
          : 'border-border text-text-muted hover:border-text-muted hover:text-text',
      ].join(' ')}
    >
      {children}
    </button>
  )
}

function HitChip({ hit, onClick }: { hit: SearchHit; onClick: () => void }) {
  return (
    <Tooltip
      content={`${KIND_LABEL[hit.kind] ?? hit.kind}: ${hit.text} @ ${hit.pts_offset.toFixed(1)}s (${Math.round(hit.confidence * 100)}%)`}
    >
      <button
        type="button"
        onClick={onClick}
        className="flex items-center gap-1 rounded border border-border bg-surface/60 px-1.5 py-0.5 text-xs text-text-secondary hover:border-accent hover:text-accent"
      >
        <span className="text-text-muted">{KIND_LABEL[hit.kind] ?? hit.kind}</span>
        <span className="font-medium">{hit.text}</span>
      </button>
    </Tooltip>
  )
}

function ResultCard({ result, onOpen }: { result: SearchResult; onOpen: (pts?: number) => void }) {
  const [thumbFailed, setThumbFailed] = useState(false)
  return (
    <div className="flex flex-col gap-2 rounded-lg border border-border bg-surface/40 p-2.5">
      <button
        type="button"
        onClick={() => onOpen()}
        className="flex h-28 items-center justify-center overflow-hidden rounded bg-surface-2/80"
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
          <SearchIcon className="size-4 text-text-muted" />
        )}
      </button>
      <div className="flex items-center justify-between text-xs text-text-muted">
        <span>{formatDateLong(result.start_time)}</span>
        <span className="tabular-nums">{formatClockTime(result.start_time)}</span>
      </div>
      {result.segment.unreadable_reason && (
        <Tooltip content={result.segment.unreadable_reason}>
          <div className="flex w-fit items-center gap-1 rounded-sm bg-danger/15 px-1 py-0.5 text-xs font-medium text-danger">
            <TriangleAlert className="size-3" strokeWidth={2} />
            Unreadable
          </div>
        </Tooltip>
      )}
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
  const [searchParams, setSearchParams] = useSearchParams()

  // Lazy initializers read the URL exactly once, on mount — after that,
  // this component's state is the source of truth and the effect below
  // keeps the URL following it (not the other way around), so typing
  // doesn't fight with browser back/forward.
  const [q, setQ] = useState(() => searchParams.get('q') ?? '')
  const [classes, setClasses] = useState<Set<string>>(() => new Set(searchParams.getAll('class')))
  const [make, setMake] = useState(() => searchParams.get('make') ?? '')
  const [vehicleType, setVehicleType] = useState(() => searchParams.get('type') ?? '')
  const [plate, setPlate] = useState(() => searchParams.get('plate') ?? '')
  const [from, setFrom] = useState(() => searchParams.get('from') ?? '')
  const [to, setTo] = useState(() => searchParams.get('to') ?? '')
  const [minConf, setMinConf] = useState(() => Number(searchParams.get('min_conf')) || 0)
  const [minSpeed, setMinSpeed] = useState(() => Number(searchParams.get('min_speed')) || 0)

  const debouncedQ = useDebouncedValue(q, 300)
  const debouncedMake = useDebouncedValue(make, 300)
  const debouncedType = useDebouncedValue(vehicleType, 300)
  const debouncedPlate = useDebouncedValue(plate, 300)

  const classList = [...classes].sort()

  // Mirror every filter into the URL (replacing the current history entry,
  // never pushing a new one) so the current search is shareable and a
  // Catalog link's params show up pre-applied. Uses the raw (non-debounced)
  // values so the URL always matches what's on screen.
  useEffect(() => {
    const params = new URLSearchParams()
    if (q) params.set('q', q)
    for (const cls of classList) params.append('class', cls)
    if (make) params.set('make', make)
    if (vehicleType) params.set('type', vehicleType)
    if (plate) params.set('plate', plate)
    if (from) params.set('from', from)
    if (to) params.set('to', to)
    if (minConf) params.set('min_conf', String(minConf))
    if (minSpeed) params.set('min_speed', String(minSpeed))
    setSearchParams(params, { replace: true })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, classList.join(','), make, vehicleType, plate, from, to, minConf, minSpeed])

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

  const hasFilter =
    effectiveQ || classList.length > 0 || debouncedMake || debouncedType || debouncedPlate || minConf > 0 || minSpeed > 0

  const { data, isLoading, isFetchingNextPage, fetchNextPage, hasNextPage, isError } = useInfiniteQuery({
    queryKey: ['search', effectiveQ, classList, debouncedMake, debouncedType, debouncedPlate, from, to, minConf, minSpeed],
    queryFn: ({ pageParam }) =>
      search({
        q: effectiveQ || undefined,
        classes: classList.length > 0 ? classList : undefined,
        make: debouncedMake || undefined,
        type: debouncedType || undefined,
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
    navigate(`/footage/trips/${result.trip.id}?${params}`)
  }

  const results = data?.pages.flatMap((page) => page.results) ?? []

  return (
    <div className="mx-auto flex max-w-6xl flex-col gap-5 px-8 py-8">
      <h1 className="text-lg font-semibold text-text">Search</h1>

      <div className="flex flex-col gap-3 rounded-lg border border-border bg-surface/50 p-4">
        <div className="relative">
          <SearchIcon className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-text-muted" />
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search plates, makes, object classes… (press / to focus)"
            className="w-full rounded-md border border-border bg-bg py-2 pl-9 pr-9 text-[13px] text-text outline-none placeholder:text-text-muted focus:border-accent"
          />
          {q && (
            <button
              type="button"
              onClick={() => setQ('')}
              className="absolute right-3 top-1/2 -translate-y-1/2 text-text-muted hover:text-text-secondary"
            >
              <X className="size-4" />
            </button>
          )}
        </div>
        {q.length > 0 && q.length < minTrigramChars && (
          <span className="text-xs text-text-muted">Type at least {minTrigramChars} characters to search text</span>
        )}

        <div className="flex flex-wrap items-center gap-1.5">
          {CLASS_OPTIONS.map((cls) => (
            <Chip key={cls} active={classes.has(cls)} onClick={() => toggleClass(cls)}>
              {titleCase(cls)}
            </Chip>
          ))}
        </div>

        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Make
            <input
              value={make}
              onChange={(e) => setMake(e.target.value)}
              placeholder="Toyota"
              className="w-28 rounded border border-border bg-bg px-2 py-1 text-[12px] text-text outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Type
            <input
              value={vehicleType}
              onChange={(e) => setVehicleType(e.target.value)}
              placeholder="sedan"
              className="w-28 rounded border border-border bg-bg px-2 py-1 text-[12px] text-text outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Plate
            <input
              value={plate}
              onChange={(e) => setPlate(e.target.value)}
              placeholder="ABC123"
              className="w-28 rounded border border-border bg-bg px-2 py-1 text-[12px] text-text outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            From
            <input
              type="date"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              className="rounded border border-border bg-bg px-2 py-1 text-[12px] text-text outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            To
            <input
              type="date"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className="rounded border border-border bg-bg px-2 py-1 text-[12px] text-text outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Min confidence: {Math.round(minConf * 100)}%
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
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Min speed: {Math.round(minSpeed * 3.6)} km/h
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
        <div className="flex flex-col items-center justify-center gap-2 py-16 text-text-muted">
          <SearchIcon className="size-6" />
          <span className="text-[13px]">Enter a search term or pick a filter to get started</span>
        </div>
      )}

      {isLoading && (
        <div className="flex items-center justify-center py-16 text-text-muted">
          <Loader2 className="size-5 animate-spin" />
        </div>
      )}

      {isError && <div className="py-16 text-center text-[13px] text-text-muted">Search failed</div>}

      {hasFilter && !isLoading && !isError && results.length === 0 && (
        <div className="py-16 text-center text-[13px] text-text-muted">No matches</div>
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
              className="mx-auto flex items-center gap-2 rounded-md border border-border px-4 py-2 text-[12px] font-medium text-text-secondary hover:border-text-muted hover:text-text disabled:opacity-50"
            >
              {isFetchingNextPage && <Loader2 className="size-3.5 animate-spin" />}
              Load more
            </button>
          )}
        </>
      )}
    </div>
  )
}
