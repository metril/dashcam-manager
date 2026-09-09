import { useEffect } from 'react'
import {
  BarChart3,
  BookOpen,
  Camera,
  FolderOutput,
  FolderSearch,
  HardDrive,
  ListTodo,
  Search,
  Settings as SettingsIcon,
  Share2,
} from 'lucide-react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import JobsIndicator from './components/JobsIndicator'
import SectionTabs, { type TabItem } from './components/SectionTabs'
import HelpButton from './components/HelpButton'
import { getSSDStatus } from './lib/ingestApi'

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
      { to: '/system/jobs', label: 'Jobs', icon: ListTodo },
      { to: '/system/stats', label: 'Stats', icon: BarChart3 },
      { to: '/system/settings', label: 'Settings', icon: SettingsIcon },
    ],
  },
]

function SSDPill() {
  const { data } = useQuery({
    queryKey: ['ssd-status'],
    queryFn: () => getSSDStatus(),
    refetchInterval: 15000,
  })

  let dot = 'bg-neutral-600'
  let label = 'SSD absent'
  if (data?.present && !data.mounted_rw) {
    dot = 'bg-amber-500'
    label = 'SSD read-only'
  } else if (data?.present) {
    dot = 'bg-emerald-500'
    label = 'SSD'
  }

  return (
    <NavLink
      to="/system"
      className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs font-medium text-neutral-400 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
    >
      <span className={['size-1.5 rounded-full', dot].join(' ')} />
      {label}
    </NavLink>
  )
}

export default function Layout() {
  const navigate = useNavigate()
  const location = useLocation()

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
    <div className="flex min-h-svh flex-col bg-neutral-950 text-neutral-100">
      <header className="flex items-center justify-between gap-4 border-b border-neutral-900 px-6 py-3">
        <div className="flex items-center gap-6">
          <div className="flex items-center gap-2">
            <span className="relative flex size-2.5">
              <span className="absolute inline-flex size-full animate-ping rounded-full bg-amber-500 opacity-60" />
              <span className="relative inline-flex size-2.5 rounded-full bg-amber-500" />
            </span>
            <span className="text-[13px] font-semibold tracking-wide text-neutral-200">
              dashcam<span className="text-amber-500">.</span>manager
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
                      ? 'bg-amber-500/10 text-amber-400'
                      : 'text-neutral-400 hover:bg-neutral-900 hover:text-neutral-100',
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
          <HelpButton />
        </div>
      </header>
      <SectionTabs tabs={activeSection.tabs} />
      <main className="min-h-0 flex-1 overflow-y-auto">
        <Outlet />
      </main>
    </div>
  )
}
