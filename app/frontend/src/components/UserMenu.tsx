// Header user chip + sign-out for the optional OIDC login. Renders nothing
// when auth is disabled (GET /api/me reports enabled=false).
import { useMutation, useQuery } from '@tanstack/react-query'
import { LogOut } from 'lucide-react'
import { fetchMe, logout } from '../lib/authApi'

export default function UserMenu() {
  const { data } = useQuery({ queryKey: ['me'], queryFn: fetchMe, staleTime: Infinity })
  const signOut = useMutation({ mutationFn: logout })

  if (!data?.enabled || !data.user) return null
  const { name, email } = data.user

  return (
    <div className="flex items-center gap-1">
      <span className="hidden max-w-40 truncate text-[13px] text-text-muted sm:inline" title={email}>
        {name || email}
      </span>
      <button
        type="button"
        onClick={() => signOut.mutate()}
        disabled={signOut.isPending}
        aria-label="Sign out"
        title="Sign out"
        className="flex items-center justify-center rounded-md p-1.5 text-text-muted transition-colors hover:bg-surface hover:text-text"
      >
        <LogOut className="size-4" strokeWidth={2} />
      </button>
    </div>
  )
}
