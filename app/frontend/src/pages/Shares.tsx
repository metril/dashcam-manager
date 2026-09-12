// M9 Shares page: list configured SMB/CIFS shares with status pills, an
// add/edit dialog, test/mount/unmount actions, a scan-now button (enqueues
// a scan job for the share's linked source, mirroring the Sources scan
// flow from M4/M7), and free space.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { HardDrive, Loader2, Pencil, PlayCircle, Plug, PlugZap, Plus, Trash2, X } from 'lucide-react'
import { Link } from 'react-router-dom'
import {
  createShare,
  deleteShare,
  getShares,
  mountShare,
  SharesApiError,
  testShare,
  unmountShare,
  updateShare,
  type Share,
  type ShareInput,
} from '../lib/sharesApi'
import { scanSource } from '../lib/jobsApi'
import Tooltip from '../components/Tooltip'
import { formatBytes, humanizeState } from '../lib/format'

// kindBadgeStyle/kindLabel render the share kind pill (local-storage plan
// task 11) — "local" rows have no server/share/username/domain/options
// (they're a bind-mounted directory, not an SMB connection), and are
// managed end-to-end from the Storage page rather than here.
function kindBadgeStyle(kind: string): string {
  return kind === 'local' ? 'bg-accent/10 text-accent' : 'bg-surface-2 text-text-muted'
}

function kindLabel(kind: string): string {
  return kind === 'local' ? 'Local' : 'CIFS'
}

function statusStyle(status: string): string {
  if (status === 'mounted') return 'bg-success/10 text-success'
  if (status.startsWith('error')) return 'bg-danger/10 text-danger'
  if (status === 'disabled') return 'bg-surface-2 text-text-muted'
  return 'bg-surface-2 text-text-muted'
}

const emptyForm: ShareInput = {
  name: '',
  server: '',
  share: '',
  subpath: '',
  username: '',
  password: '',
  domain: '',
  options: '',
  enabled: true,
}

export default function Shares() {
  const queryClient = useQueryClient()
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editingID, setEditingID] = useState<number | null>(null)
  const [form, setForm] = useState<ShareInput>(emptyForm)
  const [testResults, setTestResults] = useState<Record<number, { ok: boolean; error?: string }>>({})
  // Force bypasses the scan's empty-root safety check (see
  // cmd/api/handlers_sources.go's handleSourcesScan / scan.Opts.Force) —
  // normally never needed (a fully-ingested-and-cleaned source rescans
  // cleanly on its own), so this defaults off and requires an explicit,
  // visible opt-in rather than being on by default.
  const [forceScan, setForceScan] = useState(false)

  const {
    data: shares,
    isLoading,
    isError,
  } = useQuery({ queryKey: ['shares'], queryFn: getShares, refetchInterval: 15000 })

  function invalidate() {
    queryClient.invalidateQueries({ queryKey: ['shares'] })
  }

  const createMutation = useMutation({
    mutationFn: (input: ShareInput) => createShare(input),
    onSuccess: () => {
      invalidate()
      closeDialog()
    },
  })
  const updateMutation = useMutation({
    mutationFn: ({ id, input }: { id: number; input: ShareInput }) => updateShare(id, input),
    onSuccess: () => {
      invalidate()
      closeDialog()
    },
  })
  const deleteMutation = useMutation({
    mutationFn: ({ id, purgeIndex }: { id: number; purgeIndex?: boolean }) => deleteShare(id, { purgeIndex }),
    onSuccess: invalidate,
  })

  // The API refuses to delete a share whose source still has indexed
  // segments (409) unless ?purge_index=1 is passed — see
  // cmd/api/handlers_shares.go's handleSharesDelete. Ask for that
  // confirmation explicitly (rather than always sending purge_index=1) so a
  // plain "delete this share definition" click can't silently destroy an
  // otherwise-healthy index.
  function confirmDeleteShare(s: Share) {
    if (!confirm(`Delete share "${s.name}"? Its indexed footage is unaffected unless you confirm purging it next.`)) {
      return
    }
    deleteMutation.mutate(
      { id: s.id },
      {
        onError: (err) => {
          if (err instanceof SharesApiError && err.status === 409) {
            if (
              confirm(
                `${err.message}\n\nDelete the share AND permanently remove its indexed segments/trips from the database? ` +
                  `(The underlying files on the share itself are not touched.)`,
              )
            ) {
              deleteMutation.mutate({ id: s.id, purgeIndex: true })
            }
          }
        },
      },
    )
  }
  const mountMutation = useMutation({ mutationFn: (id: number) => mountShare(id), onSuccess: invalidate })
  const unmountMutation = useMutation({ mutationFn: (id: number) => unmountShare(id), onSuccess: invalidate })
  const testMutation = useMutation({
    mutationFn: (id: number) => testShare(id),
    onSuccess: (result, id) => setTestResults((prev) => ({ ...prev, [id]: result })),
  })
  const scanMutation = useMutation({ mutationFn: (sourceId: number) => scanSource(sourceId, false, forceScan) })

  function openCreateDialog() {
    setEditingID(null)
    setForm(emptyForm)
    setDialogOpen(true)
  }

  function openEditDialog(s: Share) {
    setEditingID(s.id)
    setForm({
      name: s.name,
      server: s.server,
      share: s.share,
      subpath: s.subpath,
      username: s.username,
      password: '',
      domain: s.domain,
      options: s.options,
      enabled: s.enabled,
    })
    setDialogOpen(true)
  }

  function closeDialog() {
    setDialogOpen(false)
    setEditingID(null)
    setForm(emptyForm)
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const input: ShareInput = { ...form }
    if (!input.password) delete input.password // don't clobber a stored password with an empty PATCH
    if (editingID != null) {
      updateMutation.mutate({ id: editingID, input })
    } else {
      createMutation.mutate(input)
    }
  }

  const saving = createMutation.isPending || updateMutation.isPending
  const saveError = (createMutation.error ?? updateMutation.error) as Error | null

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between">
        <h1 className="text-sm font-semibold text-text">Shares</h1>
        <button
          type="button"
          onClick={openCreateDialog}
          className="flex items-center gap-1.5 rounded-md bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/20"
        >
          <Plus className="size-3.5" /> Add share
        </button>
      </div>

      <label
        className="flex items-center gap-1.5 self-start text-xs text-text-muted"
        title="Bypasses the scan's empty-root safety check, which normally refuses to scan a source when the root looks unmounted/empty. Only needed if you deliberately deleted everything off a source and want the index to reflect that — a source fully ingested to a share and then cleaned rescans cleanly on its own, without this."
      >
        <input
          type="checkbox"
          checked={forceScan}
          onChange={(e) => setForceScan(e.target.checked)}
          className="size-3"
        />
        Force scan (bypass empty-root safety check — only for a source you deliberately emptied)
      </label>

      {isLoading && <div className="text-xs text-text-muted">Loading…</div>}
      {isError && <div className="text-xs text-danger">Failed to load shares.</div>}
      {!isLoading && !isError && (shares?.length ?? 0) === 0 && (
        <div className="text-xs text-text-muted">No shares configured yet.</div>
      )}

      <div className="flex flex-col gap-2">
        {shares?.map((s) => {
          const result = testResults[s.id]
          return (
            <div key={s.id} className="rounded-lg border border-border bg-bg/60 p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs">
                  <span className="font-medium text-text">{s.name}</span>
                  <span className={['rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide', kindBadgeStyle(s.kind)].join(' ')}>
                    {kindLabel(s.kind)}
                  </span>
                  <span
                    className={['rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide', statusStyle(s.status)].join(
                      ' ',
                    )}
                  >
                    {humanizeState(s.status)}
                  </span>
                  {s.kind === 'local' ? (
                    <span className="font-mono text-xs text-text-muted">{s.mount_point}</span>
                  ) : (
                    <span className="font-mono text-xs text-text-muted">
                      //{s.server}/{s.share}
                      {s.subpath ? `/${s.subpath}` : ''}
                    </span>
                  )}
                </div>
                <div className="flex items-center gap-1">
                  {s.source_id != null && (
                    <button
                      type="button"
                      onClick={() => scanMutation.mutate(s.source_id!)}
                      disabled={scanMutation.isPending}
                      className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-accent disabled:opacity-50"
                    >
                      <PlayCircle className="size-3.5" /> Scan
                    </button>
                  )}
                  {s.kind === 'local' ? (
                    <Tooltip content="Manage on the Storage page">
                      <Link
                        to="/system/storage"
                        className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-accent"
                      >
                        <HardDrive className="size-3.5" /> Storage
                      </Link>
                    </Tooltip>
                  ) : (
                    <>
                      <button
                        type="button"
                        onClick={() => testMutation.mutate(s.id)}
                        disabled={testMutation.isPending}
                        className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text disabled:opacity-50"
                      >
                        {testMutation.isPending && testMutation.variables === s.id ? (
                          <Loader2 className="size-3.5 animate-spin" />
                        ) : (
                          'Test'
                        )}
                      </button>
                      {s.status === 'mounted' ? (
                        <button
                          type="button"
                          onClick={() => unmountMutation.mutate(s.id)}
                          className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text"
                        >
                          <PlugZap className="size-3.5" /> Unmount
                        </button>
                      ) : (
                        <button
                          type="button"
                          onClick={() => mountMutation.mutate(s.id)}
                          className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text"
                        >
                          <Plug className="size-3.5" /> Mount
                        </button>
                      )}
                      <Tooltip content="Edit">
                        <button
                          type="button"
                          onClick={() => openEditDialog(s)}
                          className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-text"
                        >
                          <Pencil className="size-3.5" />
                        </button>
                      </Tooltip>
                      <Tooltip content="Delete">
                        <button
                          type="button"
                          onClick={() => confirmDeleteShare(s)}
                          className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-text-muted transition-colors hover:bg-surface hover:text-danger"
                        >
                          <Trash2 className="size-3.5" />
                        </button>
                      </Tooltip>
                    </>
                  )}
                </div>
              </div>

              <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-text-muted">
                {s.kind !== 'local' && s.username && <span>User: {s.username}</span>}
                {s.total_bytes != null && s.free_bytes != null && (
                  <span>
                    {formatBytes(s.free_bytes)} free of {formatBytes(s.total_bytes)}
                  </span>
                )}
                {s.kind !== 'local' && s.last_mounted_at && (
                  <span>Last mounted: {new Date(s.last_mounted_at).toLocaleString()}</span>
                )}
              </div>

              {result && (
                <div className={['mt-1.5 text-xs', result.ok ? 'text-success' : 'text-danger'].join(' ')}>
                  {result.ok ? 'Test succeeded.' : `Test failed: ${result.error}`}
                </div>
              )}
            </div>
          )
        })}
      </div>

      {dialogOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-md rounded-lg border border-border bg-bg p-4">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="text-sm font-semibold text-text">{editingID != null ? 'Edit share' : 'Add share'}</h2>
              <button type="button" onClick={closeDialog} className="text-text-muted hover:text-text">
                <X className="size-4" />
              </button>
            </div>
            <form onSubmit={handleSubmit} className="flex flex-col gap-2.5">
              <Field label="Name">
                <input
                  type="text"
                  value={form.name ?? ''}
                  onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                  className={inputClass}
                  placeholder="Garage NAS"
                />
              </Field>
              <div className="grid grid-cols-2 gap-2.5">
                <Field label="Server *">
                  <input
                    type="text"
                    required
                    value={form.server}
                    onChange={(e) => setForm((f) => ({ ...f, server: e.target.value }))}
                    className={inputClass}
                    placeholder="nas.local"
                  />
                </Field>
                <Field label="Share *">
                  <input
                    type="text"
                    required
                    value={form.share}
                    onChange={(e) => setForm((f) => ({ ...f, share: e.target.value }))}
                    className={inputClass}
                    placeholder="footage"
                  />
                </Field>
              </div>
              <Field label="Subpath">
                <input
                  type="text"
                  value={form.subpath ?? ''}
                  onChange={(e) => setForm((f) => ({ ...f, subpath: e.target.value }))}
                  className={inputClass}
                  placeholder="dashcam"
                />
              </Field>
              <div className="grid grid-cols-2 gap-2.5">
                <Field label="Username">
                  <input
                    type="text"
                    value={form.username ?? ''}
                    onChange={(e) => setForm((f) => ({ ...f, username: e.target.value }))}
                    className={inputClass}
                  />
                </Field>
                <Field label="Domain">
                  <input
                    type="text"
                    value={form.domain ?? ''}
                    onChange={(e) => setForm((f) => ({ ...f, domain: e.target.value }))}
                    className={inputClass}
                  />
                </Field>
              </div>
              <Field label={editingID != null ? 'Password (leave blank to keep current)' : 'Password'}>
                <input
                  type="password"
                  value={form.password ?? ''}
                  onChange={(e) => setForm((f) => ({ ...f, password: e.target.value }))}
                  className={inputClass}
                />
              </Field>
              <Field label="Extra mount options">
                <input
                  type="text"
                  value={form.options ?? ''}
                  onChange={(e) => setForm((f) => ({ ...f, options: e.target.value }))}
                  className={inputClass}
                  placeholder="ro,vers=2.1"
                />
              </Field>
              <label className="flex items-center gap-2 text-xs text-text-muted">
                <input
                  type="checkbox"
                  checked={form.enabled ?? true}
                  onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
                  className="size-3.5"
                />
                Enabled (remounted automatically)
              </label>

              {saveError && <div className="text-xs text-danger">{saveError.message}</div>}

              <div className="mt-2 flex justify-end gap-2">
                <button
                  type="button"
                  onClick={closeDialog}
                  className="rounded-md px-3 py-1.5 text-xs text-text-muted hover:bg-surface"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  disabled={saving}
                  className="rounded-md bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
                >
                  {saving ? 'Saving…' : editingID != null ? 'Save' : 'Add'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1 text-xs text-text-muted">
      {label}
      {children}
    </label>
  )
}

const inputClass =
  'rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50'
