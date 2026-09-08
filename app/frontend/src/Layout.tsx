import { useEffect } from 'react'
import {
  BarChart3,
  Camera,
  FolderOutput,
  FolderSearch,
  HardDrive,
  ListTodo,
  Search,
  Settings,
  Share2,
} from 'lucide-react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import JobsIndicator from './components/JobsIndicator'
import { getSSDStatus } from './lib/ingestApi'

interface NavItem {
  to: string
  label: string
  icon: typeof Camera
}

const navItems: NavItem[] = [
  { to: '/', label: 'Browse', icon: FolderSearch },
  { to: '/search', label: 'Search', icon: Search },
  { to: '/jobs', label: 'Jobs', icon: ListTodo },
  { to: '/shares', label: 'Shares', icon: Share2 },
  { to: '/ingest', label: 'Ingest', icon: HardDrive },
  { to: '/exports', label: 'Exports', icon: FolderOutput },
  { to: '/stats', label: 'Stats', icon: BarChart3 },
  { to: '/settings', label: 'Settings', icon: Settings },
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
      to="/ingest"
      className="flex items-center gap-1.5 rounded-md px-2 py-1 text-[11px] font-medium text-neutral-400 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
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
      if (location.pathname === '/search') return // Search's own listener handles refocus
      e.preventDefault()
      navigate('/search')
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate, location.pathname])

  return (
    <div className="flex min-h-svh bg-neutral-950 text-neutral-100">
      <aside className="flex w-56 shrink-0 flex-col border-r border-neutral-900 bg-black/40">
        <div className="flex items-center gap-2 px-5 py-5">
          <span className="relative flex size-2.5">
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-amber-500 opacity-60" />
            <span className="relative inline-flex size-2.5 rounded-full bg-amber-500" />
          </span>
          <span className="text-[13px] font-semibold tracking-wide text-neutral-200">
            dashcam<span className="text-amber-500">.</span>manager
          </span>
        </div>
        <nav className="flex flex-col gap-0.5 px-3">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                [
                  'flex items-center gap-2.5 rounded-md px-3 py-2 text-[13px] font-medium transition-colors',
                  isActive
                    ? 'bg-amber-500/10 text-amber-400'
                    : 'text-neutral-400 hover:bg-neutral-900 hover:text-neutral-100',
                ].join(' ')
              }
            >
              <item.icon className="size-4" strokeWidth={2} />
              {item.label}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className="flex min-w-0 flex-1 flex-col overflow-y-auto">
        <header className="flex items-center justify-end gap-2 border-b border-neutral-900 px-6 py-2.5">
          <SSDPill />
          <JobsIndicator />
        </header>
        <div className="min-h-0 flex-1">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
