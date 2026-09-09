// WP7 nav restructure: horizontal tab bar rendered below the header's three
// section links, showing the pages within whichever section is active.
// Shared by Layout.tsx for the Footage/Exports/System sections.
import { NavLink } from 'react-router-dom'
import type { LucideIcon } from 'lucide-react'

export interface TabItem {
  to: string
  label: string
  icon: LucideIcon
  end?: boolean
}

export default function SectionTabs({ tabs }: { tabs: TabItem[] }) {
  return (
    <nav className="flex items-center gap-1 border-b border-border px-4 2xl:px-6">
      {tabs.map((tab) => (
        <NavLink
          key={tab.to}
          to={tab.to}
          end={tab.end}
          className={({ isActive }) =>
            [
              'flex items-center gap-1.5 border-b-2 px-2.5 py-2 text-[13px] font-medium transition-colors',
              isActive
                ? 'border-accent text-accent'
                : 'border-transparent text-text-muted hover:text-text',
            ].join(' ')
          }
        >
          <tab.icon className="size-3.5" strokeWidth={2} />
          {tab.label}
        </NavLink>
      ))}
    </nav>
  )
}
