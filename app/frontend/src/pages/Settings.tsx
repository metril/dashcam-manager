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
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getSettings, updateSettings, SettingsApiError, type Settings } from '../lib/settingsApi'

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
  'rounded-md border border-neutral-800 bg-neutral-900 px-2 py-1.5 text-xs text-neutral-200 outline-none focus:border-amber-500/50'

function RestartChip() {
  return (
    <span
      title="This setting is only read once at process startup; saving it takes effect after the container is restarted."
      className="rounded bg-neutral-900 px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wider text-neutral-500"
    >
      restart required
    </span>
  )
}

export default function Settings() {
  const queryClient = useQueryClient()
  const { data, isLoading, isError } = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const [form, setForm] = useState<Settings | null>(null)
  const [savedMessage, setSavedMessage] = useState(false)

  // Initialize the form once from the fetched data; never overwrite it on a
  // later refetch (e.g. window refocus) so unsaved edits aren't wiped out.
  if (form === null && data) setForm(data)

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

  if (isLoading) return <div className="p-6 text-xs text-neutral-400">Loading…</div>
  if (isError || !form) return <div className="p-6 text-xs text-red-400">Failed to load settings.</div>

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex items-center justify-between">
        <h1 className="text-sm font-semibold text-neutral-200">Settings</h1>
        <button
          type="button"
          onClick={handleSave}
          disabled={!dirty || mutation.isPending}
          className="rounded-md bg-amber-500/10 px-3 py-1.5 text-xs font-medium text-amber-400 transition-colors hover:bg-amber-500/20 disabled:cursor-not-allowed disabled:opacity-40"
        >
          {mutation.isPending ? 'Saving…' : 'Save'}
        </button>
      </div>

      <p className="max-w-2xl text-xs text-neutral-400">
        A <span className="rounded bg-neutral-900 px-1 py-0.5 text-neutral-500">restart required</span> chip marks
        settings that are only read once when the API process starts: hardware-acceleration mode, max ffmpeg jobs,
        and worker count. Saving them updates the stored value immediately, but the running process won't pick it up
        until it's restarted.
      </p>

      {mutation.isError && (
        <div className="text-xs text-red-400">
          {mutation.error instanceof SettingsApiError ? mutation.error.message : 'Failed to save settings.'}
        </div>
      )}
      {savedMessage && !mutation.isError && <div className="text-xs text-emerald-400">Settings saved.</div>}

      <div className="flex max-w-md flex-col gap-3 rounded-lg border border-neutral-800 bg-neutral-950/60 p-4">
        <Field label="SSD path" note="managed by env DASHCAM_SSD_PATH in compose/.env">
          <input
            type="text"
            value={form.ssd_path}
            readOnly
            disabled
            className={`${inputClass} cursor-not-allowed opacity-60`}
            placeholder="/mnt/dashcam"
          />
        </Field>

        <Field
          label="SSD filesystem UUID"
          chip={RESTART_ONLY.includes('ssd_uuid')}
          note="from /dev/disk/by-uuid; enables in-container hotplug mount. Leave blank for no override — DASHCAM_SSD_UUID applies (or manage the mount yourself if that's unset too)."
        >
          <input
            type="text"
            value={form.ssd_uuid}
            onChange={(e) => set('ssd_uuid', e.target.value.trim())}
            className={inputClass}
            placeholder="F9B1-23CB"
          />
        </Field>

        <Field label="QSV device" note="managed by env DASHCAM_QSV_DEVICE in compose/.env">
          <input
            type="text"
            value={form.qsv_device}
            readOnly
            disabled
            className={`${inputClass} cursor-not-allowed opacity-60`}
            placeholder="/dev/dri/renderD128"
          />
        </Field>

        <Field label="Hardware acceleration mode" chip={RESTART_ONLY.includes('hw_mode')}>
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
        </Field>

        <div className="grid grid-cols-2 gap-3">
          <Field label="Max ffmpeg jobs" chip={RESTART_ONLY.includes('max_ffmpeg_jobs')}>
            <input
              type="number"
              min={1}
              value={form.max_ffmpeg_jobs}
              onChange={(e) => set('max_ffmpeg_jobs', Math.max(1, Number(e.target.value) || 1))}
              className={inputClass}
            />
          </Field>
          <Field label="Workers" chip={RESTART_ONLY.includes('workers')}>
            <input
              type="number"
              min={1}
              value={form.workers}
              onChange={(e) => set('workers', Math.max(1, Number(e.target.value) || 1))}
              className={inputClass}
            />
          </Field>
        </div>
      </div>
    </div>
  )
}

function Field({
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
    <label className="flex flex-col gap-1 text-xs text-neutral-400">
      <span className="flex items-center gap-1.5">
        {label}
        {chip && <RestartChip />}
      </span>
      {children}
      {note && <span className="text-xs text-neutral-400">{note}</span>}
    </label>
  )
}
