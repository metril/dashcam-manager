import { Route, Routes } from 'react-router-dom'
import Layout from './Layout'
import Browse from './pages/Browse'
import TripDetail from './pages/TripDetail'
import Placeholder from './pages/Placeholder'
import Jobs from './pages/Jobs'

function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Browse />} />
        <Route path="trips/:id" element={<TripDetail />} />
        <Route path="search" element={<Placeholder title="Search" />} />
        <Route path="jobs" element={<Jobs />} />
        <Route path="shares" element={<Placeholder title="Shares" />} />
        <Route path="ingest" element={<Placeholder title="Ingest" />} />
        <Route path="settings" element={<Placeholder title="Settings" />} />
      </Route>
    </Routes>
  )
}

export default App
