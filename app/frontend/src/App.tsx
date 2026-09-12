import { lazy, Suspense } from 'react'
import { Link, Navigate, Route, Routes, useParams } from 'react-router-dom'
import Layout from './Layout'
import Browse from './pages/Browse'
import TripDetail from './pages/TripDetail'
import Jobs from './pages/Jobs'
import Shares from './pages/Shares'
import Ingest from './pages/Ingest'
import Exports from './pages/Exports'
import Search from './pages/Search'
import Stats from './pages/Stats'
import Settings from './pages/Settings'
import Dashcams from './pages/Dashcams'
import Storage from './pages/Storage'

// Catalog is a new WP7 page (Footage > Catalog); lazy-loaded since it's a
// small, self-contained addition to the Footage section.
const Catalog = lazy(() => import('./pages/Catalog'))

// Preserves the :id param when redirecting the old /trips/:id path to its
// new home under Footage.
function TripRedirect() {
  const { id } = useParams()
  return <Navigate to={`/footage/trips/${id}`} replace />
}

// Catch-all for any unmatched path (WP9): a small "Page not found" view
// with a link back to Footage, instead of a blank render.
function NotFound() {
  return (
    <div className="flex flex-col items-center justify-center gap-3 p-12 text-center">
      <h1 className="text-sm font-semibold text-text">Page not found</h1>
      <p className="text-xs text-text-muted">The page you're looking for doesn't exist.</p>
      <Link
        to="/footage"
        className="rounded-md border border-border px-3 py-1.5 text-xs font-medium text-text-secondary transition-colors hover:border-accent/60 hover:text-accent"
      >
        Go to Footage
      </Link>
    </div>
  )
}

function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        {/* Footage section */}
        <Route path="footage" element={<Browse />} />
        <Route path="footage/search" element={<Search />} />
        <Route
          path="footage/catalog"
          element={
            <Suspense fallback={<div className="p-6 text-xs text-text-muted">Loading…</div>}>
              <Catalog />
            </Suspense>
          }
        />
        <Route path="footage/trips/:id" element={<TripDetail />} />

        {/* Exports section */}
        <Route path="exports" element={<Exports />} />
        <Route path="exports/shares" element={<Shares />} />

        {/* System section */}
        <Route path="system" element={<Ingest />} />
        <Route path="system/dashcams" element={<Dashcams />} />
        <Route path="system/storage" element={<Storage />} />
        <Route path="system/jobs" element={<Jobs />} />
        <Route path="system/stats" element={<Stats />} />
        <Route path="system/settings" element={<Settings />} />

        {/* Old paths redirect to their new homes */}
        <Route index element={<Navigate to="/footage" replace />} />
        <Route path="search" element={<Navigate to="/footage/search" replace />} />
        <Route path="jobs" element={<Navigate to="/system/jobs" replace />} />
        <Route path="shares" element={<Navigate to="/exports/shares" replace />} />
        <Route path="ingest" element={<Navigate to="/system" replace />} />
        <Route path="stats" element={<Navigate to="/system/stats" replace />} />
        <Route path="settings" element={<Navigate to="/system/settings" replace />} />
        <Route path="trips/:id" element={<TripRedirect />} />

        {/* Catch-all: unmatched paths get a small "Page not found" view */}
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  )
}

export default App
