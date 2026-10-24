import { useEffect, useState } from 'react'
import {
  BarChart3,
  BookOpen,
  Boxes,
  Camera,
  FolderOutput,
  FolderSearch,
  HardDrive,
  ListTodo,
  Monitor,
  Moon,
  SatelliteDish,
  Search,
  Settings as SettingsIcon,
  Share2,
  Sun,
  Gauge,
} from 'lucide-react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import JobsIndicator from './components/JobsIndicator'
import SectionTabs, { type TabItem } from './components/SectionTabs'
import HelpButton from './components/HelpButton'
import UserMenu from './components/UserMenu'
import { getSSDStatus } from './lib/ingestApi'
import { getStoredMode, setThemeMode, watchSystemTheme, THEME_CHANGE_EVENT, type ThemeMode } from './lib/theme'

interface Section {
  key: string
  label: string
  to: string
  icon: typeof Camera
  match: string // pathname prefix used to determine if this section is active
  tabs: TabItem[]
}

const sections: Section[] = [
  {
    key: 'footage',
    label: 'Footage',
    to: '/footage',
    icon: Camera,
    match: '/footage',
    tabs: [
      { to: '/footage', label: 'Browse', icon: FolderSearch, end: true },
      { to: '/footage/search', label: 'Search', icon: Search },
      { to: '/footage/catalog', label: 'Catalog', icon: BookOpen },
      { to: '/footage/vehicle', label: 'Vehicle', icon: Gauge },
    ],
  },
  {
    key: 'exports',
    label: 'Exports',
    to: '/exports',
    icon: FolderOutput,
    match: '/exports',
    tabs: [
      { to: '/exports', label: 'Exports', icon: FolderOutput, end: true },
      { to: '/exports/shares', label: 'Shares', icon: Share2 },
    ],
  },
  {
    key: 'system',
    label: 'System',
    to: '/system',
    icon: HardDrive,
    match: '/system',
    tabs: [
      { to: '/system', label: 'Ingest', icon: HardDrive, end: true },
      { to: '/system/dashcams', label: 'Dashcams', icon: SatelliteDish },
      { to: '/system/storage', label: 'Storage', icon: Boxes },
      { to: '/system/jobs', label: 'Jobs', icon: ListTodo },
      { to: '/system/stats', label: 'Stats', icon: BarChart3 },
      { to: '/system/settings', label: 'Settings', icon: SettingsIcon },
    ],
  },
]

const THEME_CYCLE: ThemeMode[] = ['system', 'light', 'dark']
const THEME_ICON: Record<ThemeMode, typeof Monitor> = { system: Monitor, light: Sun, dark: Moon }
const THEME_LABEL: Record<ThemeMode, string> = { system: 'System', light: 'Light', dark: 'Dark' }

// ThemeToggle is a single compact icon button cycling system -> light ->
// dark -> system. It re-syncs from localStorage on the THEME_CHANGE_EVENT
// custom event so it stays in step with the equivalent control on the
// Settings page (see pages/Settings.tsx) without any shared state/context.
function ThemeToggle() {
  const [mode, setMode] = useState<ThemeMode>(getStoredMode)

  useEffect(() => {
    const onChange = () => setMode(getStoredMode())
    window.addEventListener(THEME_CHANGE_EVENT, onChange)
    return () => window.removeEventListener(THEME_CHANGE_EVENT, onChange)
  }, [])

  const Icon = THEME_ICON[mode]

  return (
    <button
      type="button"
      onClick={() => {
        const next = THEME_CYCLE[(THEME_CYCLE.indexOf(mode) + 1) % THEME_CYCLE.length]
        setThemeMode(next)
      }}
      title={`Theme: ${THEME_LABEL[mode]} (click to cycle)`}
      className="flex items-center gap-1.5 rounded-md p-1.5 text-text-muted transition-colors hover:bg-surface hover:text-text"
    >
      <Icon className="size-4" strokeWidth={2} />
    </button>
  )
}

function SSDPill() {
  const { data } = useQuery({
    queryKey: ['ssd-status'],
    queryFn: () => getSSDStatus(),
    refetchInterval: 15000,
  })

  let dot = 'bg-text-muted'
  let label = 'SSD absent'
  if (data?.present && !data.mounted_rw) {
    dot = 'bg-accent'
    label = 'SSD read-only'
  } else if (data?.present) {
    dot = 'bg-success'
    label = 'SSD'
  }

  return (
    <NavLink
      to="/system"
      className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium text-text-muted transition-colors hover:bg-surface hover:text-text"
    >
      <span className={['size-1.5 rounded-full', dot].join(' ')} />
      {label}
    </NavLink>
  )
}

export default function Layout() {
  const navigate = useNavigate()
  const location = useLocation()

  // Live-update the resolved theme when the OS/browser color-scheme
  // preference changes while the stored mode is 'system' — otherwise a
  // system-mode user who flips their OS theme wouldn't see it reflected
  // until their next explicit setThemeMode call or a full reload.
  // watchSystemTheme itself no-ops (and only re-applies) when the stored
  // mode isn't 'system', so this is safe to run unconditionally for the
  // lifetime of the app shell.
  useEffect(() => watchSystemTheme(), [])

  // App-wide "/" shortcut: jump to Search from anywhere. Search.tsx
  // handles focusing its own input once mounted (both on its own "/"
  // keydown when already there, and on mount when navigated to via this
  // handler), so this only needs to navigate.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return
      if (e.target instanceof HTMLElement) {
        if (['INPUT', 'TEXTAREA', 'SELECT'].includes(e.target.tagName)) return
        if (e.target.isContentEditable) return
      }
      if (location.pathname === '/footage/search') return // Search's own listener handles refocus
      e.preventDefault()
      navigate('/footage/search')
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate, location.pathname])

  const activeSection = sections.find((s) => location.pathname.startsWith(s.match)) ?? sections[0]

  return (
    <div className="flex min-h-svh flex-col bg-bg text-text">
      <header className="flex h-12 items-center justify-between gap-4 border-b border-border px-4 2xl:px-6">
        <div className="flex items-center gap-6">
          <div className="flex items-center gap-2">
            <span className="relative flex size-2.5">
              <span className="absolute inline-flex size-full animate-ping rounded-full bg-accent opacity-60" />
              <span className="relative inline-flex size-2.5 rounded-full bg-accent" />
            </span>
            <span className="text-[13px] font-semibold tracking-wide text-text">
              dashcam<span className="text-accent">.</span>manager
            </span>
          </div>
          <nav className="flex items-center gap-1">
            {sections.map((section) => (
              <NavLink
                key={section.key}
                to={section.to}
                className={() =>
                  [
                    'flex items-center gap-1.5 rounded-md px-3 py-1.5 text-[13px] font-medium transition-colors',
                    activeSection.key === section.key
                      ? 'bg-accent/10 text-accent'
                      : 'text-text-muted hover:bg-surface hover:text-text',
                  ].join(' ')
                }
              >
                <section.icon className="size-4" strokeWidth={2} />
                {section.label}
              </NavLink>
            ))}
          </nav>
        </div>
        <div className="flex items-center gap-2">
          <SSDPill />
          <JobsIndicator />
          <ThemeToggle />
          <HelpButton />
          <UserMenu />
        </div>
      </header>
      <SectionTabs tabs={activeSection.tabs} />
      <main className="min-h-0 flex-1 overflow-y-auto">
        <Outlet />
      </main>
    </div>
  )
}
