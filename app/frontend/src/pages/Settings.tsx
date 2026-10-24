// Settings page: view/edit the flat key/value settings backed by
// GET/PUT /api/settings (see cmd/api/handlers_settings.go). Every key is
// applied live by the server; keys the server says still need a restart come
// back in restart_required and show a chip. Apply failures, and settings that
// differ from their DASHCAM_* environment seed, are shown on the row too.
//
// Layout: a dense grid of titled cards (grid gap-4 md:grid-cols-2
// 2xl:grid-cols-3). The Save button stays sticky in the header and covers the
// plain form fields; the Sign-in, Detections and Maintenance cards persist
// or act via their own buttons.
import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, RotateCw, Wrench, Database, Trash2, ShieldCheck, Image, Server } from 'lucide-react'
import {
  getSettings,
  getSettingsMeta,
  updateSettings,
  type HwMode,
  type Settings,
  type SettingsPatch,
  type SettingsUpdateResult,
} from '../lib/settingsApi'
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
import { getStoredUnits, setUnits, UNITS_CHANGE_EVENT, type UnitSystem } from '../lib/units'
import DetectionFilterFields from '../components/settings/DetectionFilterFields'
import JobResult from '../components/JobResult'
import OidcCard from '../components/settings/OidcCard'
import {
  Card,
  ErrorLine,
  Row,
  inputClass,
} from '../components/settings/shared'
import { SettingsHintsContext } from '../components/settings/hints'
import Segmented from '../components/ui/Segmented'
import Switch from '../components/ui/Switch'

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

const UNIT_SYSTEMS: UnitSystem[] = ['us', 'metric']
const UNIT_SYSTEM_LABEL: Record<UnitSystem, string> = { us: 'US', metric: 'Metric' }

// UnitsSegmented mirrors ThemeSegmented above for the units preference
// (lib/units.ts, localStorage-backed) — kept in sync via UNITS_CHANGE_EVENT
// the same way ThemeSegmented syncs off THEME_CHANGE_EVENT.
function UnitsSegmented() {
  const [units, setUnitsState] = useState<UnitSystem>(getStoredUnits)

  useEffect(() => {
    const onChange = () => setUnitsState(getStoredUnits())
    window.addEventListener(UNITS_CHANGE_EVENT, onChange)
    return () => window.removeEventListener(UNITS_CHANGE_EVENT, onChange)
  }, [])

  return (
    <div role="radiogroup" aria-label="Units" className="inline-flex rounded-md border border-border bg-surface p-0.5">
      {UNIT_SYSTEMS.map((u) => (
        <button
          key={u}
          type="button"
          role="radio"
          aria-checked={units === u}
          onClick={() => setUnits(u)}
          className={[
            'rounded px-2 py-1 text-xs font-medium transition-colors',
            units === u ? 'bg-accent/15 text-accent' : 'text-text-muted hover:text-text',
          ].join(' ')}
        >
          {UNIT_SYSTEM_LABEL[u]}
        </button>
      ))}
    </div>
  )
}

const HW_MODES: HwMode[] = ['auto', 'nvenc', 'qsv', 'vaapi', 'software']

// Human-readable labels for hw_mode values; titleCase alone would render the
// QSV/VAAPI acronyms wrong.
const HW_MODE_LABEL: Record<HwMode, string> = {
  auto: 'Auto',
  nvenc: 'NVIDIA NVENC',
  qsv: 'Intel QSV',
  vaapi: 'VAAPI',
  software: 'Software',
}

// Keys edited by the sticky Save button (the OIDC keys and public URL belong
// to the Sign-in card).
const FORM_KEYS = [
  'ssd_path',
  'ssd_uuid',
  'qsv_device',
  'cuda_device',
  'local_root',
  'ffmpeg_bin',
  'ffprobe_bin',
  'timezone',
  'hw_mode',
  'max_ffmpeg_jobs',
  'workers',
  'dashcam_poll_interval_s',
] as const satisfies readonly (keyof Settings)[]
type FormKey = (typeof FORM_KEYS)[number]

const SECRET_KEY_SOURCE: Record<string, string> = {
  env: 'Environment variable',
  file: 'File in the data directory',
  generated: 'Generated on first start and stored in the data directory',
}

export default function Settings() {
  const queryClient = useQueryClient()
  const { data, isLoading, isError } = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const metaQuery = useQuery({ queryKey: ['settings-meta'], queryFn: getSettingsMeta })
  const [searchParams, setSearchParams] = useSearchParams()
  const [oidcNotice] = useState(() => searchParams.get('oidc') === 'ok')
  const [lastResult, setLastResult] = useState<SettingsUpdateResult | null>(null)

  // The sign-in test callback lands here as /system/settings?oidc=ok.
  useEffect(() => {
    if (searchParams.get('oidc') !== 'ok') return
    queryClient.invalidateQueries({ queryKey: ['settings'] })
    queryClient.invalidateQueries({ queryKey: ['settings-meta'] })
    queryClient.invalidateQueries({ queryKey: ['me'] })
    setSearchParams({}, { replace: true })
  }, [searchParams, setSearchParams, queryClient])
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

  function applyResult(result: SettingsUpdateResult) {
    setLastResult(result)
    queryClient.setQueryData(['settings'], result.settings)
    queryClient.invalidateQueries({ queryKey: ['settings'] })
    queryClient.invalidateQueries({ queryKey: ['settings-meta'] })
    queryClient.invalidateQueries({ queryKey: ['me'] })
  }

  function refreshAll(result?: SettingsUpdateResult) {
    if (result) return applyResult(result)
    queryClient.invalidateQueries({ queryKey: ['settings'] })
    queryClient.invalidateQueries({ queryKey: ['settings-meta'] })
    queryClient.invalidateQueries({ queryKey: ['me'] })
  }

  const mutation = useMutation({
    mutationFn: (patch: SettingsPatch) => updateSettings(patch),
    onSuccess: (result) => {
      applyResult(result)
      setForm(result.settings)
      setSavedMessage(true)
      setTimeout(() => setSavedMessage(false), 3000)
    },
  })

  function set<K extends FormKey>(key: K, value: Settings[K]) {
    setForm((f) => (f ? { ...f, [key]: value } : f))
    setSavedMessage(false)
  }

  const dirtyKeys = FORM_KEYS.filter((k) => data && form && form[k] !== data[k])
  const dirty = dirtyKeys.length > 0

  function handleSave() {
    if (!form || !dirty) return
    const patch: Record<string, unknown> = {}
    for (const k of dirtyKeys) patch[k] = form[k]
    mutation.mutate(patch as SettingsPatch)
  }

  const hints = {
    restartRequired: Array.from(new Set([...(lastResult?.restart_required ?? []), ...(metaQuery.data?.restart_required ?? [])])),
    envDiffers: metaQuery.data?.env_differs ?? [],
    applyErrors: lastResult?.apply_errors ?? {},
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
    <SettingsHintsContext.Provider value={hints}>
    <div className="flex flex-col gap-4 p-6">
      <div className="sticky top-0 z-10 -mx-6 flex items-center justify-between gap-3 border-b border-border bg-bg/95 px-6 py-3 backdrop-blur">
        <div>
          <h1 className="text-sm font-semibold text-text">Settings</h1>
          <p className="text-[11px] text-text-muted">Changes apply right away unless a row says restart required.</p>
        </div>
        <div className="flex items-center gap-3">
          {mutation.isError && <ErrorLine error={mutation.error} />}
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

      {oidcNotice && (
        <p role="status" className="rounded border border-success/40 bg-success/10 px-3 py-2 text-xs text-success">
          Sign-in test passed and sign-in is now enabled.
        </p>
      )}
      {Object.keys(lastResult?.apply_errors ?? {}).length > 0 && (
        <p role="alert" className="rounded border border-warn/40 bg-warn/10 px-3 py-2 text-xs text-warn">
          Saved, but some changes could not be applied. See the highlighted rows.
        </p>
      )}

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
            <Row label="Units" note="Applies to speed, distance, fuel, and temperature everywhere in the app.">
              <UnitsSegmented />
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
                  <Switch checked={dryRun} onChange={setDryRun} label="Dry run" />
                  Dry run
                </label>
                <label className="flex items-center gap-1.5">
                  <Switch checked={vacuum} onChange={setVacuum} label="Vacuum after" />
                  Vacuum after
                </label>
              </div>
              {pruneMutation.isSuccess && <JobResult job={pruneMutation.data.job} />}
              {pruneMutation.isError && <ErrorLine error={pruneMutation.error} />}
            </div>
          </Card>
        </div>

        <Card title="Storage & hardware" icon={<Wrench className="size-3.5" />}>
          <Row label="SSD path" settingKey="ssd_path" note="Where the ingest SSD is mounted. Must be under /mnt.">
            <input type="text" value={form.ssd_path} onChange={(e) => set('ssd_path', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="/mnt/dashcam" />
          </Row>
          <Row label="SSD filesystem UUID" settingKey="ssd_uuid" note="From /dev/disk/by-uuid. Enables in-container hotplug mount. Leave blank for none.">
            <input type="text" value={form.ssd_uuid} onChange={(e) => set('ssd_uuid', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="F9B1-23CB" />
          </Row>
          <Row label="Local storage root" settingKey="local_root" note="Base folder for local storages. Must be under /mnt and not overlap the SSD path.">
            <input type="text" value={form.local_root} onChange={(e) => set('local_root', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="/mnt/dashcam-local" />
          </Row>
          <Row label="QSV device" settingKey="qsv_device">
            <input type="text" value={form.qsv_device} onChange={(e) => set('qsv_device', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="/dev/dri/renderD128" />
          </Row>
          <Row label="CUDA device" settingKey="cuda_device" note="GPU index, digits only.">
            <input type="text" value={form.cuda_device} onChange={(e) => set('cuda_device', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="0" />
          </Row>
          <Row label="ffmpeg binary" settingKey="ffmpeg_bin" note="A command name or an absolute path.">
            <input type="text" value={form.ffmpeg_bin} onChange={(e) => set('ffmpeg_bin', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="ffmpeg" />
          </Row>
          <Row label="ffprobe binary" settingKey="ffprobe_bin">
            <input type="text" value={form.ffprobe_bin} onChange={(e) => set('ffprobe_bin', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="ffprobe" />
          </Row>
          <Row label="Timezone" settingKey="timezone" note="IANA name such as America/New_York. Blank uses the server default.">
            <input type="text" value={form.timezone} onChange={(e) => set('timezone', e.target.value.trim())} className={`${inputClass} w-36`} placeholder="America/New_York" />
          </Row>

          <Row label="Hardware acceleration mode" settingKey="hw_mode">
            <Segmented
              aria-label="Hardware acceleration mode"
              options={HW_MODES.map((m) => ({ value: m, label: HW_MODE_LABEL[m] }))}
              value={form.hw_mode}
              onChange={(m) => set('hw_mode', m)}
            />
          </Row>

          <Row label="Max ffmpeg jobs" settingKey="max_ffmpeg_jobs">
            <input type="number" min={1} max={32} value={form.max_ffmpeg_jobs} onChange={(e) => set('max_ffmpeg_jobs', Math.max(1, Number(e.target.value) || 1))} className={`${inputClass} w-16`} />
          </Row>
          <Row label="Workers" settingKey="workers">
            <input type="number" min={1} max={64} value={form.workers} onChange={(e) => set('workers', Math.max(1, Number(e.target.value) || 1))} className={`${inputClass} w-16`} />
          </Row>
          <Row
            label="Dashcam auto-pull interval (seconds, 0 = off)"
            note="How often every configured dashcam is auto-pulled. Takes effect on the next poll tick."
          >
            <input
              type="number"
              min={0}
              value={form.dashcam_poll_interval_s}
              onChange={(e) => set('dashcam_poll_interval_s', Math.max(0, Number(e.target.value) || 0))}
              className={`${inputClass} w-20`}
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

        <Card title="Server" icon={<Server className="size-3.5" />}>
          <p className="text-[11px] text-text-muted">These are set at startup and can only be changed in the container configuration.</p>
          <ServerRow label="Database" value={metaQuery.data?.bootstrap.db} loading={metaQuery.isLoading} />
          <ServerRow label="Cache" value={metaQuery.data?.bootstrap.cache} loading={metaQuery.isLoading} />
          <ServerRow label="Listen address" value={metaQuery.data?.bootstrap.addr} loading={metaQuery.isLoading} />
          <ServerRow
            label="Secret key"
            value={metaQuery.data ? (SECRET_KEY_SOURCE[metaQuery.data.secret_key_source] ?? metaQuery.data.secret_key_source) : undefined}
            loading={metaQuery.isLoading}
          />
          {metaQuery.isError && <ErrorLine error={metaQuery.error} />}
        </Card>

        <OidcCard settings={data ?? form} meta={metaQuery.data} onSaved={refreshAll} />

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
              Re-indexes every segment with detections but no track assigned yet — needed once after upgrading to
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
    </SettingsHintsContext.Provider>
  )
}

function ServerRow({ label, value, loading }: { label: string; value?: string; loading: boolean }) {
  return (
    <div className="flex items-center justify-between gap-3 text-xs text-text-muted">
      <span>{label}</span>
      <code className="min-w-0 truncate text-[11px] text-text-secondary" title={value}>
        {value ?? (loading ? 'Loading…' : 'Unavailable')}
      </code>
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
