// Storage page (local-storage plan task 11): every persistent storage
// (local dirs, CIFS shares, and the export cache pseudo-storage) with
// usage, an add/edit form for local directories, per-row scan/hash, and a
// Duplicates section (verified/probable groups across storages). The
// recorder's SSD is never a storage and never appears here — see
// cmd/api/handlers_storages.go's storageView doc comment.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import {
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  HardDrive,
  Loader2,
  Pencil,
  PlayCircle,
  Plus,
  RotateCw,
  Trash2,
  TriangleAlert,
  X,
} from 'lucide-react'
import {
  createLocalStorage,
  deleteStorage,
  getDuplicates,
  getStorages,
  hashStorages,
  refreshDuplicates,
  scanStorage,
  StorageApiError,
  updateLocalStorage,
  type DuplicateGroup,
  type DuplicateKind,
  type LocalStorageInput,
  type Storage as StorageRow,
} from '../lib/storageApi'
import { formatBytes, humanizeState, pluralize, titleCase } from '../lib/format'
import Tooltip from '../components/Tooltip'

function kindBadgeStyle(kind: string): string {
  if (kind === 'local') return 'bg-accent/10 text-accent'
  if (kind === 'cache') return 'bg-surface-2 text-text-muted'
  return 'bg-surface-2 text-text-muted'
}

function statusStyle(status: string): string {
  if (status === 'mounted' || status === 'ok') return 'bg-success/10 text-success'
  if (status.startsWith('error')) return 'bg-danger/10 text-danger'
  return 'bg-surface-2 text-text-muted'
}

// UsageBar renders a storage's filesystem free/total as a small bar,
// matching Shares.tsx's free/total line but as a visual bar since this
// page has room for one — nothing renders when free/total aren't known
// (an unmounted share, or a local dir the app can't currently read).
function UsageBar({ storage }: { storage: StorageRow }) {
  if (storage.free_bytes == null || storage.total_bytes == null || storage.total_bytes === 0) {
    return <span className="text-xs text-text-muted">—</span>
  }
  const usedBytes = storage.total_bytes - storage.free_bytes
  const pct = Math.min(100, Math.max(0, (usedBytes / storage.total_bytes) * 100))
  return (
    <div className="flex flex-col gap-1">
      <div className="h-1.5 w-32 overflow-hidden rounded bg-surface-2">
        <div className={['h-full rounded', pct > 90 ? 'bg-danger' : 'bg-accent/70'].join(' ')} style={{ width: `${pct}%` }} />
      </div>
      <span className="text-xs text-text-muted">
        {formatBytes(storage.free_bytes)} free of {formatBytes(storage.total_bytes)}
      </span>
    </div>
  )
}

const emptyForm: LocalStorageInput = { label: '', path: '', enabled: true }

// scanStatusAlreadyRunning mirrors cmd/api/handlers_storages.go's
// handleStoragesScan "already_running" status literal — kept as a named
// constant (rather than an inline string) so lint:text's JSX-child-string
// check doesn't flag the raw API value as leaked copy.
const scanStatusAlreadyRunning = 'already_running'

// DUP_TABS/dupTabLabel: the two duplicate_groups.kind values as tabs.
// Kept out of the JSX tree (see scanStatusAlreadyRunning above) for the
// same lint:text reason.
const DUP_TABS: DuplicateKind[] = ['sha256', 'size_name']
function dupTabLabel(kind: DuplicateKind): string {
  return kind === 'sha256' ? 'Verified' : 'Probable'
}
const DUP_KIND_SIZE_NAME: DuplicateKind = 'size_name'

function StorageFormDialog({
  editing,
  onClose,
}: {
  editing: StorageRow | null | 'new'
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [form, setForm] = useState<LocalStorageInput>(
    editing && editing !== 'new' ? { label: editing.label, path: editing.path, enabled: editing.enabled } : emptyForm,
  )

  const createMutation = useMutation({
    mutationFn: (input: LocalStorageInput) => createLocalStorage(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['storages'] })
      onClose()
    },
  })
  const updateMutation = useMutation({
    mutationFn: (input: LocalStorageInput) => updateLocalStorage((editing as StorageRow).id, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['storages'] })
      onClose()
    },
  })

  const saving = createMutation.isPending || updateMutation.isPending
  const error = (createMutation.error ?? updateMutation.error) as Error | null
  const isNew = editing === 'new'

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (isNew) {
      createMutation.mutate(form)
    } else {
      updateMutation.mutate(form)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="w-full max-w-md rounded-lg border border-border bg-bg p-4">
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold text-text">{isNew ? 'Add local storage' : 'Edit local storage'}</h2>
          <button type="button" onClick={onClose} className="text-text-muted hover:text-text">
            <X className="size-4" />
          </button>
        </div>
        <form onSubmit={handleSubmit} className="flex flex-col gap-2.5">
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Name
            <input
              type="text"
              value={form.label}
              onChange={(e) => setForm((f) => ({ ...f, label: e.target.value }))}
              placeholder="Backup drive"
              className="rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-text-muted">
            Path *
            <input
              type="text"
              required
              value={form.path}
              onChange={(e) => setForm((f) => ({ ...f, path: e.target.value }))}
              placeholder="/mnt/shares/local-test"
              className="rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50"
            />
          </label>
          <p className="text-xs text-text-muted">Must be an existing directory under the local storage root.</p>
          <label className="flex items-center gap-2 text-xs text-text-muted">
            <input
              type="checkbox"
              checked={form.enabled ?? true}
              onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
              className="size-3.5"
            />
            Enabled
          </label>

          {error && <div className="text-xs text-danger">{error.message}</div>}

          <div className="mt-2 flex justify-end gap-2">
            <button type="button" onClick={onClose} className="rounded-md px-3 py-1.5 text-xs text-text-muted hover:bg-surface">
              Cancel
            </button>
            <button
              type="submit"
              disabled={saving}
              className="rounded-md bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
            >
              {saving ? 'Saving…' : isNew ? 'Add' : 'Save'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

function StorageTable() {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<StorageRow | null | 'new'>(null)

  const { data: storages, isLoading, isError } = useQuery({
    queryKey: ['storages'],
    queryFn: getStorages,
    refetchInterval: 15000,
  })

  function invalidate() {
    queryClient.invalidateQueries({ queryKey: ['storages'] })
  }

  const scanMutation = useMutation({ mutationFn: (id: string) => scanStorage(id) })
  const scanAlreadyRunning = scanMutation.data?.status === scanStatusAlreadyRunning
  const hashMutation = useMutation({ mutationFn: (id?: string) => hashStorages(id) })
  const deleteMutation = useMutation({
    mutationFn: ({ id, purgeIndex }: { id: string; purgeIndex?: boolean }) => deleteStorage(id, { purgeIndex }),
    onSuccess: invalidate,
  })

  function confirmDelete(s: StorageRow) {
    if (!confirm(`Delete storage "${s.label}"? Its indexed footage is unaffected unless you confirm purging it next.`)) {
      return
    }
    deleteMutation.mutate(
      { id: s.id },
      {
        onError: (err) => {
          if (err instanceof StorageApiError && err.status === 409) {
            if (
              confirm(
                `${err.message}\n\nDelete the storage AND permanently remove its indexed segments/trips from the database? ` +
                  `(The underlying files on disk are not touched.)`,
              )
            ) {
              deleteMutation.mutate({ id: s.id, purgeIndex: true })
            }
          }
        },
      },
    )
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold text-text">Storages</h2>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => hashMutation.mutate(undefined)}
            disabled={hashMutation.isPending}
            className="flex items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs font-medium text-text-secondary transition-colors hover:border-text-muted disabled:opacity-50"
          >
            {hashMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
            Hash all
          </button>
          <button
            type="button"
            onClick={() => setEditing('new')}
            className="flex items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/20"
          >
            <Plus className="size-3.5" /> Add local storage
          </button>
        </div>
      </div>

      {hashMutation.isSuccess && (
        <p className="text-xs text-text-muted">
          Hash backfill queued.{' '}
          <Link to="/system/jobs" className="text-accent hover:underline">
            View job #{hashMutation.data.job.id}
          </Link>
        </p>
      )}

      {isLoading && <div className="text-xs text-text-muted">Loading…</div>}
      {isError && <div className="text-xs text-danger">Failed to load storages.</div>}
      {!isLoading && !isError && (storages?.length ?? 0) === 0 && (
        <div className="text-xs text-text-muted">No storages configured yet.</div>
      )}

      {storages && storages.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-border">
          <table className="w-full text-left text-[13px]">
            <thead className="bg-surface/60 text-xs uppercase tracking-wide text-text-muted">
              <tr>
                <th className="px-3 py-2 font-medium">Storage</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 font-medium">Usage</th>
                <th className="px-3 py-2 font-medium">Footage</th>
                <th className="px-3 py-2 font-medium">Exports</th>
                <th className="px-3 py-2 font-medium">Hash pending</th>
                <th className="px-3 py-2 font-medium" />
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {storages.map((s) => (
                <tr key={s.id} className="text-text-secondary">
                  <td className="px-3 py-2">
                    <div className="flex items-center gap-2">
                      <span className={['rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide', kindBadgeStyle(s.kind)].join(' ')}>
                        {s.kind === 'cache' ? 'Cache' : titleCase(s.kind)}
                      </span>
                      <div className="flex flex-col">
                        <span className="font-medium text-text">{s.label}</span>
                        <span className="font-mono text-xs text-text-muted">{s.path}</span>
                      </div>
                    </div>
                  </td>
                  <td className="px-3 py-2">
                    <span className={['rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide', statusStyle(s.status)].join(' ')}>
                      {humanizeState(s.status)}
                    </span>
                  </td>
                  <td className="px-3 py-2">
                    <UsageBar storage={s} />
                  </td>
                  <td className="px-3 py-2 tabular-nums text-text-muted">
                    {pluralize(s.segment_count, 'segment')} · {formatBytes(s.segment_bytes)}
                  </td>
                  <td className="px-3 py-2 tabular-nums text-text-muted">
                    {pluralize(s.export_count, 'export')} · {formatBytes(s.export_bytes)}
                  </td>
                  <td className="px-3 py-2 tabular-nums">
                    {s.hash_pending > 0 ? (
                      <span className="text-warn">{s.hash_pending.toLocaleString()}</span>
                    ) : (
                      <span className="text-text-muted">0</span>
                    )}
                  </td>
                  <td className="px-3 py-2 text-right">
                    <div className="flex items-center justify-end gap-1">
                      {s.kind !== 'cache' && (
                        <>
                          <Tooltip content="Scan">
                            <button
                              type="button"
                              onClick={() => scanMutation.mutate(s.id)}
                              disabled={scanMutation.isPending && scanMutation.variables === s.id}
                              className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-accent disabled:opacity-50"
                            >
                              <PlayCircle className="size-3.5" />
                            </button>
                          </Tooltip>
                          <Tooltip content="Hash">
                            <button
                              type="button"
                              onClick={() => hashMutation.mutate(s.id)}
                              disabled={hashMutation.isPending}
                              className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text disabled:opacity-50"
                            >
                              <RotateCw className="size-3.5" />
                            </button>
                          </Tooltip>
                        </>
                      )}
                      {s.kind === 'local' && (
                        <>
                          <Tooltip content="Edit">
                            <button
                              type="button"
                              onClick={() => setEditing(s)}
                              className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text"
                            >
                              <Pencil className="size-3.5" />
                            </button>
                          </Tooltip>
                          <Tooltip content="Delete">
                            <button
                              type="button"
                              onClick={() => confirmDelete(s)}
                              className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-danger"
                            >
                              <Trash2 className="size-3.5" />
                            </button>
                          </Tooltip>
                        </>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {scanMutation.isSuccess && (
        <p className="text-xs text-text-muted">
          Scan {scanAlreadyRunning ? 'already running' : 'queued'}.{' '}
          {scanMutation.data.job && (
            <Link to="/system/jobs" className="text-accent hover:underline">
              View job #{scanMutation.data.job.id}
            </Link>
          )}
        </p>
      )}

      {editing != null && <StorageFormDialog editing={editing} onClose={() => setEditing(null)} />}
    </div>
  )
}

const DUP_LIMIT = 20

function DuplicateGroupRow({ group, storages }: { group: DuplicateGroup; storages: StorageRow[] }) {
  const [expanded, setExpanded] = useState(false)
  const labelFor = (shareId?: number) => {
    const id = shareId != null ? String(shareId) : 'cache'
    return storages.find((s) => s.id === id)?.label ?? (id === 'cache' ? 'Export cache' : `Storage ${id}`)
  }

  return (
    <div className="rounded-lg border border-border bg-surface/40">
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        className="flex w-full items-center gap-3 px-3 py-2 text-left text-[13px]"
      >
        {expanded ? <ChevronDown className="size-3.5 text-text-muted" /> : <ChevronRight className="size-3.5 text-text-muted" />}
        <span className="flex-1 truncate font-medium text-text">{group.sample_name}</span>
        <span className="text-xs text-text-muted">{pluralize(group.storages, 'storage')}</span>
        <span className="text-xs text-text-muted">{formatBytes(group.bytes)} each</span>
        <span className="text-xs font-medium text-accent">{formatBytes(group.reclaimable)} reclaimable</span>
      </button>
      {expanded && (
        <div className="border-t border-border px-3 py-2">
          <table className="w-full text-left text-xs">
            <thead className="text-text-muted">
              <tr>
                <th className="pb-1 pr-3 font-medium">Storage</th>
                <th className="pb-1 pr-3 font-medium">Path</th>
                <th className="pb-1 pr-3 font-medium">Kind</th>
                <th className="pb-1 font-medium">Hashed?</th>
              </tr>
            </thead>
            <tbody>
              {group.members.map((m, i) => (
                <tr key={i} className="border-t border-border/80 text-text-secondary">
                  <td className="py-1 pr-3">{labelFor(m.share_id)}</td>
                  <td className="py-1 pr-3 font-mono">{m.rel_path}</td>
                  <td className="py-1 pr-3">{titleCase(m.ref_kind)}</td>
                  <td className="py-1">
                    {group.kind === 'sha256' ? (
                      <span className="flex items-center gap-1 text-success">
                        <CheckCircle2 className="size-3" /> Yes
                      </span>
                    ) : (
                      <span className="text-text-muted">No</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function DuplicatesSection({ storages }: { storages: StorageRow[] }) {
  const queryClient = useQueryClient()
  const [kind, setKind] = useState<DuplicateKind>('sha256')
  const [storageFilter, setStorageFilter] = useState<string>('')
  const [offset, setOffset] = useState(0)

  const { data, isLoading, isError } = useQuery({
    queryKey: ['duplicates', kind, storageFilter, offset],
    queryFn: () => getDuplicates({ kind, storageId: storageFilter || undefined, limit: DUP_LIMIT, offset }),
  })

  const refreshMutation = useMutation({
    mutationFn: () => refreshDuplicates(),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['duplicates'] }),
  })

  function selectTab(next: DuplicateKind) {
    setKind(next)
    setOffset(0)
  }

  const groups = data?.groups ?? []
  const total = data?.total ?? 0
  const reclaimableTotal = data?.totals?.reclaimable_bytes ?? 0

  // hash pending hint: probable groups exist while some storage still has
  // unhashed files — hashing could turn a probable group into a verified
  // one (or rule it out), so the count is a useful "why isn't this
  // verified yet" hint next to the Probable tab.
  const hashPendingTotal = storages.reduce((sum, s) => sum + s.hash_pending, 0)

  const nonCacheStorages = storages.filter((s) => s.kind !== 'cache')

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold text-text">Duplicates</h2>
        <button
          type="button"
          onClick={() => refreshMutation.mutate()}
          disabled={refreshMutation.isPending}
          className="flex items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs font-medium text-text-secondary transition-colors hover:border-text-muted disabled:opacity-50"
        >
          {refreshMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
          Refresh
        </button>
      </div>

      {refreshMutation.isSuccess && (
        <p className="text-xs text-text-muted">
          Refresh queued.{' '}
          <Link to="/system/jobs" className="text-accent hover:underline">
            View job #{refreshMutation.data.job.id}
          </Link>
        </p>
      )}

      <div className="flex flex-wrap items-center gap-1.5 border-b border-border pb-3">
        {DUP_TABS.map((k) => (
          <button
            key={k}
            type="button"
            onClick={() => selectTab(k)}
            className={[
              'rounded-md px-3 py-1.5 text-xs font-medium transition-colors',
              kind === k ? 'bg-accent/10 text-accent' : 'text-text-muted hover:bg-surface hover:text-text',
            ].join(' ')}
          >
            {dupTabLabel(k)}
          </button>
        ))}
        {kind === DUP_KIND_SIZE_NAME && hashPendingTotal > 0 && (
          <span className="text-xs text-text-muted">Hash pending: {hashPendingTotal.toLocaleString()}</span>
        )}
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <label className="flex items-center gap-2 text-xs text-text-muted">
          Storage
          <select
            value={storageFilter}
            onChange={(e) => {
              setStorageFilter(e.target.value)
              setOffset(0)
            }}
            className="rounded border border-border bg-bg px-2 py-1.5 text-[12px] text-text outline-none focus:border-accent"
          >
            <option value="">All storages</option>
            {nonCacheStorages.map((s) => (
              <option key={s.id} value={s.id}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
        <span className="text-xs text-text-muted">
          {pluralize(total, 'group')} · {formatBytes(reclaimableTotal)} reclaimable
        </span>
      </div>

      {isLoading && <div className="text-xs text-text-muted">Loading…</div>}
      {isError && <div className="text-xs text-danger">Failed to load duplicates.</div>}
      {!isLoading && !isError && groups.length === 0 && (
        <div className="text-xs text-text-muted">No {kind === 'sha256' ? 'verified' : 'probable'} duplicates found.</div>
      )}

      <div className="flex flex-col gap-2">
        {groups.map((g) => (
          <DuplicateGroupRow key={g.group_key} group={g} storages={storages} />
        ))}
      </div>

      {total > DUP_LIMIT && (
        <div className="flex items-center gap-3">
          <button
            type="button"
            disabled={offset === 0}
            onClick={() => setOffset((o) => Math.max(0, o - DUP_LIMIT))}
            className="text-xs text-text-muted disabled:opacity-30 hover:text-text"
          >
            Previous
          </button>
          <span className="text-xs text-text-muted">
            {offset + 1}–{Math.min(offset + DUP_LIMIT, total)} of {total}
          </span>
          <button
            type="button"
            disabled={offset + DUP_LIMIT >= total}
            className="flex items-center gap-1 text-xs text-text-muted disabled:opacity-30 hover:text-text"
            onClick={() => setOffset((o) => o + DUP_LIMIT)}
          >
            Next <ChevronRight className="size-3" />
          </button>
        </div>
      )}
    </div>
  )
}

export default function Storage() {
  const { data: storages } = useQuery({ queryKey: ['storages'], queryFn: getStorages, refetchInterval: 15000 })

  return (
    <div className="flex flex-col gap-6 p-6">
      <div className="flex items-center gap-2">
        <HardDrive className="size-5 text-accent" />
        <h1 className="text-lg font-semibold text-text">Storage</h1>
      </div>
      <p className="flex items-center gap-1.5 text-xs text-text-muted">
        <TriangleAlert className="size-3.5" />
        The recorder's SSD is an ingest source, not a storage — it never appears here. See{' '}
        <Link to="/system" className="text-accent hover:underline">
          Ingest
        </Link>{' '}
        for it.
      </p>

      <StorageTable />
      <DuplicatesSection storages={storages ?? []} />
    </div>
  )
}
