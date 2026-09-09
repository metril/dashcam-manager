// M9 Shares page: list configured SMB/CIFS shares with status pills, an
// add/edit dialog, test/mount/unmount actions, a scan-now button (enqueues
// a scan job for the share's linked source, mirroring the Sources scan
// flow from M4/M7), and free space.
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, Pencil, PlayCircle, Plug, PlugZap, Plus, Trash2, X } from 'lucide-react'
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

function statusStyle(status: string): string {
  if (status === 'mounted') return 'bg-emerald-500/10 text-emerald-400'
  if (status.startsWith('error')) return 'bg-red-500/10 text-red-400'
  if (status === 'disabled') return 'bg-neutral-800 text-neutral-500'
  return 'bg-neutral-800 text-neutral-400'
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
        <h1 className="text-sm font-semibold text-neutral-200">Shares</h1>
        <button
          type="button"
          onClick={openCreateDialog}
          className="flex items-center gap-1.5 rounded-md bg-amber-500/10 px-2.5 py-1.5 text-xs font-medium text-amber-400 transition-colors hover:bg-amber-500/20"
        >
          <Plus className="size-3.5" /> Add share
        </button>
      </div>

      <label
        className="flex items-center gap-1.5 self-start text-xs text-neutral-500"
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

      {isLoading && <div className="text-xs text-neutral-400">Loading…</div>}
      {isError && <div className="text-xs text-red-400">Failed to load shares.</div>}
      {!isLoading && !isError && (shares?.length ?? 0) === 0 && (
        <div className="text-xs text-neutral-400">No shares configured yet.</div>
      )}

      <div className="flex flex-col gap-2">
        {shares?.map((s) => {
          const result = testResults[s.id]
          return (
            <div key={s.id} className="rounded-lg border border-neutral-800 bg-neutral-950/60 p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs">
                  <span className="font-medium text-neutral-200">{s.name}</span>
                  <span
                    className={['rounded px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide', statusStyle(s.status)].join(
                      ' ',
                    )}
                  >
                    {humanizeState(s.status)}
                  </span>
                  <span className="font-mono text-xs text-neutral-400">
                    //{s.server}/{s.share}
                    {s.subpath ? `/${s.subpath}` : ''}
                  </span>
                </div>
                <div className="flex items-center gap-1">
                  {s.source_id != null && (
                    <button
                      type="button"
                      onClick={() => scanMutation.mutate(s.source_id!)}
                      disabled={scanMutation.isPending}
                      className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-amber-400 disabled:opacity-50"
                    >
                      <PlayCircle className="size-3.5" /> Scan
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => testMutation.mutate(s.id)}
                    disabled={testMutation.isPending}
                    className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-neutral-200 disabled:opacity-50"
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
                      className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
                    >
                      <PlugZap className="size-3.5" /> Unmount
                    </button>
                  ) : (
                    <button
                      type="button"
                      onClick={() => mountMutation.mutate(s.id)}
                      className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
                    >
                      <Plug className="size-3.5" /> Mount
                    </button>
                  )}
                  <Tooltip content="Edit">
                    <button
                      type="button"
                      onClick={() => openEditDialog(s)}
                      className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
                    >
                      <Pencil className="size-3.5" />
                    </button>
                  </Tooltip>
                  <Tooltip content="Delete">
                    <button
                      type="button"
                      onClick={() => confirmDeleteShare(s)}
                      className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-red-400"
                    >
                      <Trash2 className="size-3.5" />
                    </button>
                  </Tooltip>
                </div>
              </div>

              <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-neutral-500">
                {s.username && <span>user: {s.username}</span>}
                {s.total_bytes != null && s.free_bytes != null && (
                  <span>
                    {formatBytes(s.free_bytes)} free of {formatBytes(s.total_bytes)}
                  </span>
                )}
                {s.last_mounted_at && <span>last mounted: {new Date(s.last_mounted_at).toLocaleString()}</span>}
              </div>

              {result && (
                <div className={['mt-1.5 text-xs', result.ok ? 'text-emerald-400' : 'text-red-400'].join(' ')}>
                  {result.ok ? 'Test succeeded.' : `Test failed: ${result.error}`}
                </div>
              )}
            </div>
          )
        })}
      </div>

      {dialogOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-md rounded-lg border border-neutral-800 bg-neutral-950 p-4">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="text-sm font-semibold text-neutral-200">{editingID != null ? 'Edit share' : 'Add share'}</h2>
              <button type="button" onClick={closeDialog} className="text-neutral-500 hover:text-neutral-200">
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
              <label className="flex items-center gap-2 text-xs text-neutral-400">
                <input
                  type="checkbox"
                  checked={form.enabled ?? true}
                  onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
                  className="size-3.5"
                />
                Enabled (remounted automatically)
              </label>

              {saveError && <div className="text-xs text-red-400">{saveError.message}</div>}

              <div className="mt-2 flex justify-end gap-2">
                <button
                  type="button"
                  onClick={closeDialog}
                  className="rounded-md px-3 py-1.5 text-xs text-neutral-400 hover:bg-neutral-900"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  disabled={saving}
                  className="rounded-md bg-amber-500/10 px-3 py-1.5 text-xs font-medium text-amber-400 hover:bg-amber-500/20 disabled:opacity-50"
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
    <label className="flex flex-col gap-1 text-xs text-neutral-400">
      {label}
      {children}
    </label>
  )
}

const inputClass =
  'rounded-md border border-neutral-800 bg-neutral-900 px-2 py-1.5 text-xs text-neutral-200 outline-none focus:border-amber-500/50'
