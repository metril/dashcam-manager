import {
  Camera,
  FolderSearch,
  HardDrive,
  ListTodo,
  Search,
  Settings,
  Share2,
} from 'lucide-react'
import { NavLink, Outlet } from 'react-router-dom'
import JobsIndicator from './components/JobsIndicator'

interface NavItem {
  to: string
  label: string
  icon: typeof Camera
  disabled?: boolean
}

const navItems: NavItem[] = [
  { to: '/', label: 'Browse', icon: FolderSearch },
  { to: '/search', label: 'Search', icon: Search, disabled: true },
  { to: '/jobs', label: 'Jobs', icon: ListTodo },
  { to: '/shares', label: 'Shares', icon: Share2, disabled: true },
  { to: '/ingest', label: 'Ingest', icon: HardDrive, disabled: true },
  { to: '/settings', label: 'Settings', icon: Settings, disabled: true },
]

export default function Layout() {
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
              to={item.disabled ? '#' : item.to}
              end={item.to === '/'}
              onClick={(e) => item.disabled && e.preventDefault()}
              className={({ isActive }) =>
                [
                  'flex items-center gap-2.5 rounded-md px-3 py-2 text-[13px] font-medium transition-colors',
                  item.disabled
                    ? 'cursor-not-allowed text-neutral-600'
                    : isActive
                      ? 'bg-amber-500/10 text-amber-400'
                      : 'text-neutral-400 hover:bg-neutral-900 hover:text-neutral-100',
                ].join(' ')
              }
            >
              <item.icon className="size-4" strokeWidth={2} />
              {item.label}
              {item.disabled && (
                <span className="ml-auto rounded bg-neutral-900 px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wider text-neutral-600">
                  soon
                </span>
              )}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className="flex min-w-0 flex-1 flex-col overflow-y-auto">
        <header className="flex items-center justify-end border-b border-neutral-900 px-6 py-2.5">
          <JobsIndicator />
        </header>
        <div className="min-h-0 flex-1">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
