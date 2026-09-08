import { Route, Routes } from 'react-router-dom'
import Layout from './Layout'
import Browse from './pages/Browse'
import TripDetail from './pages/TripDetail'
import Placeholder from './pages/Placeholder'
import Jobs from './pages/Jobs'
import Shares from './pages/Shares'
import Ingest from './pages/Ingest'

function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Browse />} />
        <Route path="trips/:id" element={<TripDetail />} />
        <Route path="search" element={<Placeholder title="Search" />} />
        <Route path="jobs" element={<Jobs />} />
        <Route path="shares" element={<Shares />} />
        <Route path="ingest" element={<Ingest />} />
        <Route path="settings" element={<Placeholder title="Settings" />} />
      </Route>
    </Routes>
  )
}

export default App
