// Settings page: view/edit the small flat key/value settings row backed by
// GET/PUT /api/settings (see cmd/api/handlers_settings.go). hw_mode,
// max_ffmpeg_jobs, and workers are only read once at process boot
// (cmd/api/main.go's workers bootstrap, cmd/api/hwaccel.go's resolveHWMode,
// and cmd/api/jobs_runtime.go sizing the ffmpeg concurrency semaphore once in
// registerJobHandlers rather than re-reading max_ffmpeg_jobs per job) — so
// those need a container restart to take effect. We label those with a
// small chip rather than pretending the change is live. ssd_path and
// qsv_device are rendered read-only below: they're only ever seeded from
// the DASHCAM_SSD_PATH / DASHCAM_QSV_DEVICE env vars and the server always
// reads them from env, never from this stored row, so editing them here
// would silently do nothing.
//
// Layout: a dense grid of titled cards (grid gap-4 md:grid-cols-2
// 2xl:grid-cols-3), no max-w on the page — the previous stacked max-w-md
// cards left most of the viewport empty on anything wider than a phone.
// The Save button stays sticky in the header and only ever covers the
// plain form fields (theme/ssd_uuid/hw_mode/max_ffmpeg_jobs/workers);
// every other card (detection filter, maintenance actions) persists or
// acts immediately via its own button, same as before.
import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Loader2, RotateCw, Wrench, Database, Trash2, ShieldCheck, Image } from 'lucide-react'
import { getSettings, updateSettings, SettingsApiError, type Settings } from '../lib/settingsApi'
import {
  ApiError,
  backfillTracks,
  getDetectionFilter,
  setDetectionFilter,
  getStats,
  recheckUnreadableSegments,
  regenerateThumbs,
  verifyDatabase,
  getMaintenanceReport,
  pruneDatabase,
  type DetectionFilter,
} from '../lib/api'
import { getExportCapabilities, type ExportCapabilities } from '../lib/exportsApi'
import { getStoredMode, setThemeMode, THEME_CHANGE_EVENT, type ThemeMode } from '../lib/theme'
import DetectionFilterFields from '../components/settings/DetectionFilterFields'

const THEME_MODES: ThemeMode[] = ['system', 'light', 'dark']
const THEME_MODE_LABEL: Record<ThemeMode, string> = { system: 'System', light: 'Light', dark: 'Dark' }

// ThemeSegmented mirrors Layout's header ThemeToggle for the same
// underlying setting (lib/theme.ts, localStorage-backed) — the two stay in
// sync via the THEME_CHANGE_EVENT window event rather than shared state.
function ThemeSegmented() {
  const [mode, setMode] = useState<ThemeMode>(getStoredMode)

  useEffect(() => {
    const onChange = () => setMode(getStoredMode())
    window.addEventListener(THEME_CHANGE_EVENT, onChange)
    return () => window.removeEventListener(THEME_CHANGE_EVENT, onChange)
  }, [])

  return (
    <div role="radiogroup" aria-label="Theme" className="inline-flex rounded-md border border-border bg-surface p-0.5">
      {THEME_MODES.map((m) => (
        <button
          key={m}
          type="button"
          role="radio"
          aria-checked={mode === m}
          onClick={() => setThemeMode(m)}
          className={[
            'rounded px-2 py-1 text-xs font-medium transition-colors',
            mode === m ? 'bg-accent/15 text-accent' : 'text-text-muted hover:text-text',
          ].join(' ')}
        >
          {THEME_MODE_LABEL[m]}
        </button>
      ))}
    </div>
  )
}

const HW_MODES: Settings['hw_mode'][] = ['auto', 'qsv', 'vaapi', 'software']

// Human-readable labels for hw_mode values — titleCase alone would render
// the QSV/VAAPI acronyms wrong ("Qsv", "Vaapi"), so these are spelled out
// explicitly instead.
const HW_MODE_LABEL: Record<Settings['hw_mode'], string> = {
  auto: 'Auto',
  qsv: 'Intel QSV',
  vaapi: 'VAAPI',
  software: 'Software',
}

// Keys the API only reads once at boot; changing them here writes the
// stored value immediately but has no effect on the running process until
// it's restarted.
const RESTART_ONLY: (keyof Settings)[] = ['hw_mode', 'max_ffmpeg_jobs', 'workers', 'ssd_uuid']

const inputClass =
  'rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-text outline-none focus:border-accent/50'

function RestartChip() {
  return (
    <span
      title="This setting is only read once at process startup; saving it takes effect after the container is restarted."
      className="rounded bg-surface px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-text-muted"
    >
      restart required
    </span>
  )
}

// Card: a titled section in the settings grid. Every card gets the same
// border/padding/heading treatment so the page reads as one dense system
// instead of five differently-styled boxes.
function Card({
  title,
  icon,
  className,
  children,
}: {
  title: string
  icon?: React.ReactNode
  className?: string
  children: React.ReactNode
}) {
  return (
    <section className={`flex flex-col gap-3 rounded-lg border border-border bg-bg/60 p-4 ${className ?? ''}`}>
      <h2 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-text-secondary">
        {icon}
        {title}
      </h2>
      {children}
    </section>
  )
}

// Row: a compact label-left / control-right settings row, with optional
// helper text underneath — the building block every card's body rows use.
function Row({
  label,
  chip,
  note,
  children,
}: {
  label: string
  chip?: boolean
  note?: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-1 text-xs text-text-muted">
      <div className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-1.5">
          {label}
          {chip && <RestartChip />}
        </span>
        {children}
      </div>
      {note && <span className="text-[11px] text-text-muted">{note}</span>}
    </div>
  )
}

// JobResult: the "queued N, view job #id" feedback line shared by every
// maintenance action button below (the pattern the original tracks
// backfill button used) — POST handlers here all return { status, count?,
// job? } or { status, job } shapes.
function JobResult({ count, job }: { count?: number; job?: { id: number } }) {
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

function ErrorLine({ error }: { error: unknown }) {
  return <p className="text-[11px] text-danger">{error instanceof Error ? error.message : 'Request failed.'}</p>
}

export default function Settings() {
  const queryClient = useQueryClient()
  const { data, isLoading, isError } = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [form, setForm] = useState<Settings | null>(null)
  const [savedMessage, setSavedMessage] = useState(false)

  // Initialize the form once from the fetched data; never overwrite it on a
  // later refetch (e.g. window refocus) so unsaved edits aren't wiped out.
  if (form === null && data) setForm(data)

  const statsQuery = useQuery({ queryKey: ['stats'], queryFn: getStats })
  const capsQuery = useQuery<ExportCapabilities>({ queryKey: ['export-capabilities'], queryFn: getExportCapabilities })
  const reportQuery = useQuery({
    queryKey: ['maintenance-report'],
    queryFn: getMaintenanceReport,
    retry: false,
  })

  const detectionFilterQuery = useQuery({ queryKey: ['detection-filter'], queryFn: getDetectionFilter })
  const [filter, setFilter] = useState<DetectionFilter | null>(null)
  if (filter === null && detectionFilterQuery.data) setFilter(detectionFilterQuery.data)
  const filterMutation = useMutation({ mutationFn: (f: DetectionFilter) => setDetectionFilter(f) })
  // Editing the filter after a successful save should drop the stale
  // "Saved" label on the button — otherwise it keeps reading "Saved" even
  // once the in-memory filter has diverged from what's persisted.
  function setFilterDirty(f: DetectionFilter) {
    setFilter(f)
    filterMutation.reset()
  }

  const tracksBackfillMutation = useMutation({ mutationFn: () => backfillTracks() })
  const recheckMutation = useMutation({ mutationFn: () => recheckUnreadableSegments() })
  const regenThumbsMutation = useMutation({ mutationFn: () => regenerateThumbs('all') })
  const verifyMutation = useMutation({
    mutationFn: () => verifyDatabase(),
    onSuccess: () => {
      // The report itself lands once the db_verify job finishes; give the
      // queue a moment then refetch so the table below picks it up without
      // requiring a manual page reload.
      setTimeout(() => queryClient.invalidateQueries({ queryKey: ['maintenance-report'] }), 3000)
    },
  })
  const [dryRun, setDryRun] = useState(true)
  const [vacuum, setVacuum] = useState(false)
  // The UI keeps the "dry run" framing (it's the safer default, checked
  // by default below), but the API now requires an explicit apply:true
  // to delete anything — see pruneDatabase's doc comment.
  const pruneMutation = useMutation({ mutationFn: () => pruneDatabase({ apply: !dryRun, vacuum }) })

  const mutation = useMutation({
    mutationFn: (patch: Partial<Settings>) => updateSettings(patch),
    onSuccess: (result) => {
      queryClient.setQueryData(['settings'], result)
      queryClient.invalidateQueries({ queryKey: ['settings'] })
      setForm(result)
      setSavedMessage(true)
      setTimeout(() => setSavedMessage(false), 3000)
    },
  })

  function set<K extends keyof Settings>(key: K, value: Settings[K]) {
    setForm((f) => (f ? { ...f, [key]: value } : f))
    setSavedMessage(false)
  }

  // ssd_path and qsv_device are read-only (env-managed) and timezone isn't
  // rendered at all, so none of the three should ever be part of a save.
  const dirtyKeys = (Object.keys(form ?? {}) as (keyof Settings)[]).filter(
    (k) =>
      data &&
      form &&
      form[k] !== data[k] &&
      k !== 'ssd_path' &&
      k !== 'qsv_device' &&
      k !== 'timezone',
  )
  const dirty = dirtyKeys.length > 0

  function handleSave() {
    if (!form || !dirty) return
    const patch: Partial<Settings> = {}
    for (const k of dirtyKeys) (patch as Record<string, unknown>)[k] = form[k]
    mutation.mutate(patch)
  }

  function handleRegenThumbs() {
    if (!confirm('Regenerate thumbnails for every segment? This re-runs ffmpeg across the whole library and can take a while.')) return
    regenThumbsMutation.mutate()
  }

  function handlePrune() {
    if (!dryRun && !confirm('Prune the database? This permanently deletes orphaned rows and cached files reported by the last verify.')) return
    pruneMutation.mutate()
  }

  if (isLoading) return <div className="p-6 text-xs text-text-muted">Loading…</div>
  if (isError || !form) return <div className="p-6 text-xs text-danger">Failed to load settings.</div>

  const thumbs = statsQuery.data?.thumbs

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="sticky top-0 z-10 -mx-6 flex items-center justify-between gap-3 border-b border-border bg-bg/95 px-6 py-3 backdrop-blur">
        <div>
          <h1 className="text-sm font-semibold text-text">Settings</h1>
          <p className="text-[11px] text-text-muted">
            <span className="rounded bg-surface px-1 py-0.5 text-text-muted">restart required</span> settings save
            immediately but need a container restart to take effect.
          </p>
        </div>
        <div className="flex items-center gap-3">
          {mutation.isError && <ErrorLine error={mutation.error instanceof SettingsApiError ? mutation.error : null} />}
          {savedMessage && !mutation.isError && <span className="text-xs text-success">Saved.</span>}
          <button
            type="button"
            onClick={handleSave}
            disabled={!dirty || mutation.isPending}
            className="rounded-md bg-accent/10 px-3 py-1.5 text-xs font-medium text-accent transition-colors hover:bg-accent/20 disabled:cursor-not-allowed disabled:opacity-40"
          >
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>

      {/* items-start so a short card never stretches to match a taller
          sibling's row height; Appearance and Maintenance (both
          comparatively short/tall-but-narrow) share one flex column so
          they stack instead of leaving empty grid cells beside them —
          see the fix-round-1 note this addresses. Detections spans both
          columns at md (there's no natural third slot yet) and becomes
          its own column at 2xl. */}
      <div className="grid items-start gap-4 md:grid-cols-2 2xl:grid-cols-3">
        <div className="flex flex-col gap-4">
          <Card title="Appearance">
            <Row label="Theme" note="System follows your OS/browser color-scheme preference.">
              <ThemeSegmented />
            </Row>
          </Card>

          <Card title="Maintenance" icon={<Database className="size-3.5" />}>
            <div className="flex flex-col gap-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium text-text">Regenerate thumbnails</span>
                <button
                  type="button"
                  disabled={regenThumbsMutation.isPending}
                  onClick={handleRegenThumbs}
                  className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text disabled:opacity-40 hover:border-text-muted"
                >
                  {regenThumbsMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Image className="size-3.5" />}
                  Regenerate all
                </button>
              </div>
              {regenThumbsMutation.isSuccess && <JobResult job={regenThumbsMutation.data.job} />}
              {regenThumbsMutation.isError && <ErrorLine error={regenThumbsMutation.error} />}
            </div>

            <div className="flex flex-col gap-2 border-t border-border pt-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium text-text">Recheck unreadable segments</span>
                <button
                  type="button"
                  disabled={recheckMutation.isPending}
                  onClick={() => recheckMutation.mutate()}
                  className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text disabled:opacity-40 hover:border-text-muted"
                >
                  {recheckMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <RotateCw className="size-3.5" />}
                  Recheck
                </button>
              </div>
              {recheckMutation.isSuccess && <JobResult count={recheckMutation.data.count} job={recheckMutation.data.job} />}
              {recheckMutation.isError && <ErrorLine error={recheckMutation.error} />}
            </div>

            <div className="flex flex-col gap-2 border-t border-border pt-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium text-text">Verify database</span>
                <button
                  type="button"
                  disabled={verifyMutation.isPending}
                  onClick={() => verifyMutation.mutate()}
                  className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text disabled:opacity-40 hover:border-text-muted"
                >
                  {verifyMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
                  Verify
                </button>
              </div>
              {verifyMutation.isSuccess && <JobResult job={verifyMutation.data.job} />}
              {verifyMutation.isError && <ErrorLine error={verifyMutation.error} />}

              {reportQuery.data ? (
                <div className="overflow-x-auto rounded border border-border">
                  <table className="w-full text-[11px]">
                    <tbody>
                      <ReportRow label="Ran at" value={new Date(reportQuery.data.ran_at).toLocaleString()} />
                      <ReportRow
                        label="Integrity"
                        value={reportQuery.data.integrity_ok ? 'ok' : 'FAILED'}
                        warn={!reportQuery.data.integrity_ok}
                      />
                      <ReportRow label="Missing media" value={reportQuery.data.missing_media_segments} warn={reportQuery.data.missing_media_segments > 0} />
                      <ReportRow label="Orphan detections" value={reportQuery.data.orphan_detections} warn={reportQuery.data.orphan_detections > 0} />
                      <ReportRow label="Orphan thumbs" value={reportQuery.data.orphan_thumb_files} warn={reportQuery.data.orphan_thumb_files > 0} />
                      <ReportRow label="DB size" value={formatBytes(reportQuery.data.db_size_bytes)} />
                      <ReportRow label="WAL size" value={formatBytes(reportQuery.data.wal_size_bytes)} />
                    </tbody>
                  </table>
                </div>
              ) : reportQuery.isError ? (
                reportQuery.error instanceof ApiError && reportQuery.error.status === 404 ? (
                  <span className="text-[11px] text-text-muted">No report yet — run Verify.</span>
                ) : (
                  <ErrorLine error={reportQuery.error} />
                )
              ) : (
                reportQuery.isLoading && <span className="text-[11px] text-text-muted">Loading…</span>
              )}
            </div>

            <div className="flex flex-col gap-2 border-t border-border pt-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium text-text">Prune database</span>
                <button
                  type="button"
                  disabled={pruneMutation.isPending}
                  onClick={handlePrune}
                  className={[
                    'flex items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-xs font-medium disabled:opacity-40',
                    dryRun
                      ? 'border-border bg-surface/60 text-text hover:border-text-muted'
                      : 'border-danger/40 bg-danger/10 text-danger hover:bg-danger/20',
                  ].join(' ')}
                >
                  {pruneMutation.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Trash2 className="size-3.5" />}
                  {dryRun ? 'Dry run' : 'Prune'}
                </button>
              </div>
              <div className="flex items-center gap-3 text-[11px] text-text-muted">
                <label className="flex items-center gap-1.5">
                  <input type="checkbox" checked={dryRun} onChange={(e) => setDryRun(e.target.checked)} />
                  Dry run
                </label>
                <label className="flex items-center gap-1.5">
                  <input type="checkbox" checked={vacuum} onChange={(e) => setVacuum(e.target.checked)} />
                  Vacuum after
                </label>
              </div>
              {pruneMutation.isSuccess && <JobResult job={pruneMutation.data.job} />}
              {pruneMutation.isError && <ErrorLine error={pruneMutation.error} />}
            </div>
          </Card>
        </div>

        <Card title="Storage & hardware" icon={<Wrench className="size-3.5" />}>
          <Row label="SSD path" note="managed by env DASHCAM_SSD_PATH in compose/.env">
            <input
              type="text"
              value={form.ssd_path}
              readOnly
              disabled
              className={`${inputClass} w-36 cursor-not-allowed opacity-60`}
              placeholder="/mnt/dashcam"
            />
          </Row>

          <Row
            label="SSD filesystem UUID"
            chip={RESTART_ONLY.includes('ssd_uuid')}
            note="from /dev/disk/by-uuid; enables in-container hotplug mount. Leave blank for no override."
          >
            <input
              type="text"
              value={form.ssd_uuid}
              onChange={(e) => set('ssd_uuid', e.target.value.trim())}
              className={`${inputClass} w-36`}
              placeholder="F9B1-23CB"
            />
          </Row>

          <Row label="QSV device" note="managed by env DASHCAM_QSV_DEVICE in compose/.env">
            <input
              type="text"
              value={form.qsv_device}
              readOnly
              disabled
              className={`${inputClass} w-36 cursor-not-allowed opacity-60`}
              placeholder="/dev/dri/renderD128"
            />
          </Row>

          <Row label="Hardware acceleration mode" chip={RESTART_ONLY.includes('hw_mode')}>
            <select
              value={form.hw_mode}
              onChange={(e) => set('hw_mode', e.target.value as Settings['hw_mode'])}
              className={inputClass}
            >
              {HW_MODES.map((m) => (
                <option key={m} value={m}>
                  {HW_MODE_LABEL[m]}
                </option>
              ))}
            </select>
          </Row>

          <Row label="Max ffmpeg jobs" chip={RESTART_ONLY.includes('max_ffmpeg_jobs')}>
            <input
              type="number"
              min={1}
              value={form.max_ffmpeg_jobs}
              onChange={(e) => set('max_ffmpeg_jobs', Math.max(1, Number(e.target.value) || 1))}
              className={`${inputClass} w-16`}
            />
          </Row>
          <Row label="Workers" chip={RESTART_ONLY.includes('workers')}>
            <input
              type="number"
              min={1}
              value={form.workers}
              onChange={(e) => set('workers', Math.max(1, Number(e.target.value) || 1))}
              className={`${inputClass} w-16`}
            />
          </Row>

          <div className="flex flex-col gap-1.5 border-t border-border pt-2 text-[11px] text-text-muted">
            <span className="font-medium text-text-secondary">Encoder capabilities</span>
            {capsQuery.isLoading && <span>Loading…</span>}
            {capsQuery.isError && <span className="text-danger">Unavailable.</span>}
            {capsQuery.data && (
              <div className="flex flex-wrap gap-1.5">
                <CapChip label="H.264" ok />
                <CapChip label="H.265" ok={capsQuery.data.h265_available} />
                <CapChip label="AV1" ok={capsQuery.data.av1_available} />
              </div>
            )}
          </div>

          <div className="flex flex-col gap-1.5 border-t border-border pt-2 text-[11px] text-text-muted">
            <span className="font-medium text-text-secondary">Thumbnail hardware stats</span>
            {statsQuery.isLoading && <span>Loading…</span>}
            {statsQuery.isError && <span className="text-danger">Unavailable.</span>}
            {statsQuery.data && !thumbs && <span>No thumbnail jobs have run yet.</span>}
            {thumbs && (
              <dl className="grid grid-cols-2 gap-x-3 gap-y-0.5">
                <dt>Mode</dt>
                <dd className="text-right text-text-secondary">{thumbs.mode || 'never run'}</dd>
                <dt>HW ok</dt>
                <dd className="text-right text-text-secondary">{thumbs.hw_ok}</dd>
                <dt>HW fallbacks</dt>
                <dd className="text-right text-text-secondary">{thumbs.hw_fallbacks}</dd>
                <dt>Unreadable</dt>
                <dd className="text-right text-text-secondary">{thumbs.unreadable}</dd>
                <dt>Last run</dt>
                <dd className="text-right text-text-secondary">
                  {thumbs.last_run ? new Date(thumbs.last_run).toLocaleString() : '—'}
                </dd>
              </dl>
            )}
          </div>
        </Card>

        <Card title="Detections" className="md:col-span-2 2xl:col-span-1">
          {filter ? (
            <>
              <DetectionFilterFields filter={filter} onChange={setFilterDirty} />
              <div className="flex items-center gap-2 border-t border-border pt-2">
                <button
                  type="button"
                  onClick={() => filterMutation.mutate(filter)}
                  className="rounded-md border border-accent/30 bg-accent/10 px-2.5 py-1.5 text-xs font-medium text-accent hover:bg-accent/20"
                >
                  {filterMutation.isPending ? 'Saving…' : filterMutation.isSuccess ? 'Saved' : 'Save as default'}
                </button>
                {filterMutation.isError && <ErrorLine error={filterMutation.error} />}
              </div>
            </>
          ) : (
            <span className="text-xs text-text-muted">Loading…</span>
          )}

          <div className="flex flex-col gap-2 border-t border-border pt-2">
            <div className="flex items-center justify-between">
              <span className="text-xs font-medium text-text">Object tracks</span>
              <button
                type="button"
                disabled={tracksBackfillMutation.isPending}
                onClick={() => tracksBackfillMutation.mutate()}
                className="flex items-center gap-1.5 rounded-md border border-border bg-surface/60 px-2.5 py-1.5 text-xs font-medium text-text disabled:opacity-40 hover:border-text-muted"
              >
                {tracksBackfillMutation.isPending ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <RotateCw className="size-3.5" />
                )}
                Rebuild object tracks
              </button>
            </div>
            <span className="text-[11px] text-text-muted">
              Re-indexes every segment with detections but no track_id assigned yet — needed once after upgrading to
              the IoU tracker, or if track grouping looks incomplete.
            </span>
            {tracksBackfillMutation.isSuccess && (
              <JobResult count={tracksBackfillMutation.data.count} job={tracksBackfillMutation.data.job} />
            )}
            {tracksBackfillMutation.isError && <ErrorLine error={tracksBackfillMutation.error} />}
          </div>
        </Card>
      </div>
    </div>
  )
}

function CapChip({ label, ok }: { label: string; ok: boolean }) {
  return (
    <span
      className={[
        'rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider',
        ok ? 'bg-success/10 text-success' : 'bg-surface text-text-muted line-through',
      ].join(' ')}
    >
      {label}
    </span>
  )
}

function ReportRow({ label, value, warn }: { label: string; value: string | number; warn?: boolean }) {
  return (
    <tr className="border-b border-border last:border-0">
      <td className="px-2 py-1 text-text-muted">{label}</td>
      <td className={`px-2 py-1 text-right ${warn ? 'text-warn' : 'text-text-secondary'}`}>{value}</td>
    </tr>
  )
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}
