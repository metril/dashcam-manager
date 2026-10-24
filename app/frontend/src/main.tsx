import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import App from './App.tsx'
import './index.css'
import { isUnauthorized, redirectToLogin } from './lib/authApi'

// With the optional OIDC login enabled, an expired/missing session surfaces
// as a 401 from any API call; send the browser to sign in again.
const onError = (err: unknown) => {
  if (isUnauthorized(err)) redirectToLogin()
}

const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError }),
  mutationCache: new MutationCache({ onError }),
  defaultOptions: {
    queries: {
      // A 401 won't fix itself by retrying.
      retry: (failureCount, err) => !isUnauthorized(err) && failureCount < 3,
      // Browse/trip data doesn't change second-to-second; avoid refetching
      // on every window refocus while the user is just looking around.
      staleTime: 30_000,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
)
