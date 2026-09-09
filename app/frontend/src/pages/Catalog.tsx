// WP8: full Catalog page (Footage > Catalog) — distinct-value browser over
// the same detection dimensions Search filters on. Tabs for Brands
// (make), Vehicle types, Plates (default min_count=2, since a plate seen
// only once is rarely useful to browse), and Object classes; each row
// links into Search with that value pre-set as a URL filter (make, type,
// plate, class).
import { useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { Loader2, Search as SearchIcon } from 'lucide-react'
import { Link } from 'react-router-dom'
import { getCatalog } from '../lib/api'
import { formatDateLong, titleCase } from '../lib/format'
import type { CatalogItem, CatalogKind } from '../types'

// timeAgo renders an ISO timestamp as a short relative label ("Updated 3
// minutes ago") for the catalog_terms refresh marker (see
// internal/db/queries_catalog.go's RefreshCatalog) — the catalog page shows
// how stale its precomputed listing is rather than implying it's live.
function timeAgo(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return 'unknown'
  const seconds = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000))
  if (seconds < 60) return 'just now'
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} minute${minutes === 1 ? '' : 's'} ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} hour${hours === 1 ? '' : 's'} ago`
  const days = Math.floor(hours / 24)
  return `${days} day${days === 1 ? '' : 's'} ago`
}

const TABS: { value: CatalogKind; label: string; defaultMinCount: number }[] = [
  { value: 'make', label: 'Brands', defaultMinCount: 0 },
  { value: 'type', label: 'Vehicle types', defaultMinCount: 0 },
  { value: 'plate', label: 'Plates', defaultMinCount: 2 },
  { value: 'class', label: 'Object classes', defaultMinCount: 0 },
]

// searchParamFor maps a catalog kind to the URL query parameter Search's
// own filter state is keyed on (see the file header note above).
function searchParamFor(kind: CatalogKind): string {
  return kind
}

const pageSize = 40

function CatalogRow({ kind, item }: { kind: CatalogKind; item: CatalogItem }) {
  const [thumbFailed, setThumbFailed] = useState(false)
  const params = new URLSearchParams({ [searchParamFor(kind)]: item.value })
  return (
    <Link
      to={`/footage/search?${params}`}
      className="flex items-center gap-3 rounded-lg border border-border bg-surface/40 p-2.5 transition-colors hover:border-accent/60"
    >
      <div className="flex h-14 w-20 shrink-0 items-center justify-center overflow-hidden rounded bg-surface-2/80">
        {!thumbFailed ? (
          <img
            src={item.sample_thumb}
            alt=""
            loading="lazy"
            className="h-full w-full object-cover"
            onError={() => setThumbFailed(true)}
          />
        ) : (
          <SearchIcon className="size-4 text-text-muted" />
        )}
      </div>
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate text-[13px] font-medium text-text">{titleCase(item.value)}</span>
        <span className="text-xs text-text-muted">
          First seen {formatDateLong(item.first_seen)} · last seen {formatDateLong(item.last_seen)}
        </span>
      </div>
      <span className="shrink-0 rounded-full bg-surface-2/80 px-2.5 py-1 text-xs font-medium tabular-nums text-text-secondary">
        {item.count}
      </span>
    </Link>
  )
}

export default function Catalog() {
  const [kind, setKind] = useState<CatalogKind>('make')
  const [q, setQ] = useState('')
  const [minCount, setMinCount] = useState<number>(TABS[0].defaultMinCount)

  function selectTab(tab: CatalogKind, defaultMinCount: number) {
    setKind(tab)
    setQ('')
    setMinCount(defaultMinCount)
  }

  const { data, isLoading, isFetchingNextPage, fetchNextPage, hasNextPage, isError } = useInfiniteQuery({
    queryKey: ['catalog', kind, q, minCount],
    queryFn: ({ pageParam }) => getCatalog({ kind, q: q || undefined, minCount, cursor: pageParam, limit: pageSize }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  })

  const items = data?.pages.flatMap((page) => page.items) ?? []
  const activeTab = TABS.find((t) => t.value === kind)!
  const refreshedAt = data?.pages[0]?.refreshed_at

  return (
    <div className="flex w-full flex-col gap-4 px-4 py-5 2xl:px-6">
      <div className="flex flex-col gap-1">
        <div className="flex items-baseline justify-between gap-3">
          <h1 className="text-lg font-semibold text-text">Catalog</h1>
          {refreshedAt && <span className="text-xs text-text-muted">Updated {timeAgo(refreshedAt)}</span>}
        </div>
        <p className="text-xs text-text-muted">
          Every brand, vehicle type, plate, and object class ever detected, with how often it showed up. Color is not
          reported by the recorder.
        </p>
      </div>

      <div className="flex flex-wrap items-center gap-1.5 border-b border-border pb-3">
        {TABS.map((tab) => (
          <button
            key={tab.value}
            type="button"
            onClick={() => selectTab(tab.value, tab.defaultMinCount)}
            className={[
              'rounded-md px-3 py-1.5 text-xs font-medium transition-colors',
              kind === tab.value
                ? 'bg-accent/10 text-accent'
                : 'text-text-muted hover:bg-surface hover:text-text',
            ].join(' ')}
          >
            {tab.label}
          </button>
        ))}
      </div>

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-1 flex-col gap-1 text-xs text-text-muted">
          Search {activeTab.label.toLowerCase()}
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder={kind === 'plate' ? 'ABC123' : kind === 'make' ? 'Toyota' : ''}
            className="w-full min-w-[10rem] rounded border border-border bg-bg px-2 py-1.5 text-[12px] text-text outline-none focus:border-accent"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-text-muted">
          Minimum times seen
          <input
            type="number"
            min={0}
            value={minCount}
            onChange={(e) => setMinCount(Math.max(0, Number(e.target.value) || 0))}
            className="w-24 rounded border border-border bg-bg px-2 py-1.5 text-[12px] text-text outline-none focus:border-accent"
          />
        </label>
      </div>

      {isLoading && (
        <div className="flex items-center justify-center py-16 text-text-muted">
          <Loader2 className="size-5 animate-spin" />
        </div>
      )}

      {isError && <div className="py-16 text-center text-[13px] text-text-muted">Failed to load catalog.</div>}

      {!isLoading && !isError && items.length === 0 && (
        <div className="py-16 text-center text-[13px] text-text-muted">No values found.</div>
      )}

      {items.length > 0 && (
        <>
          <div className="grid grid-cols-1 gap-2 2xl:grid-cols-2">
            {items.map((item) => (
              <CatalogRow key={item.value} kind={kind} item={item} />
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
