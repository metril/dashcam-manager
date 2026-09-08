import { useQuery } from '@tanstack/react-query'
import { Camera, CircleAlert, CircleCheck, Loader2 } from 'lucide-react'

interface HealthResponse {
  status: string
  version: string
}

async function fetchHealth(): Promise<HealthResponse> {
  const res = await fetch('/api/health')
  if (!res.ok) {
    throw new Error(`health check failed: ${res.status}`)
  }
  return res.json()
}

function App() {
  const { data, isLoading, isError } = useQuery({
    queryKey: ['health'],
    queryFn: fetchHealth,
  })

  return (
    <div className="min-h-svh bg-neutral-950 text-neutral-100 flex flex-col items-center justify-center gap-4">
      <div className="flex items-center gap-2 text-2xl font-semibold">
        <Camera className="size-7" />
        dashcam-manager
      </div>
      <div className="flex items-center gap-2 text-sm text-neutral-400">
        {isLoading && (
          <>
            <Loader2 className="size-4 animate-spin" />
            checking API health…
          </>
        )}
        {isError && (
          <>
            <CircleAlert className="size-4 text-red-400" />
            API unreachable
          </>
        )}
        {data && (
          <>
            <CircleCheck className="size-4 text-green-400" />
            status: {data.status} (v{data.version})
          </>
        )}
      </div>
    </div>
  )
}

export default App
