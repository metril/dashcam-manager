// Client for the optional OIDC login (server: internal/auth). When auth is
// disabled /api/me reports enabled=false and none of the rest is used.
export interface Me {
  enabled: boolean
  user: { email: string; name: string } | null
}

export async function fetchMe(): Promise<Me> {
  const res = await fetch('/api/me')
  if (!res.ok) {
    const err = new Error(`${res.status} ${res.statusText}`) as Error & { status: number }
    err.status = res.status
    throw err
  }
  return res.json() as Promise<Me>
}

// logout: the CSRF hardening (cmd/api/csrf.go) requires a JSON Content-Type
// on every POST. The server answers with where to go next — the IdP's
// end-session URL when it has one, else "/".
export async function logout(): Promise<void> {
  const res = await fetch('/auth/logout', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
  })
  const body = res.ok ? ((await res.json()) as { redirect?: string }) : {}
  window.location.assign(body.redirect || '/')
}

// buildLoginURL is the sign-in URL that returns the user to loc afterwards.
export function buildLoginURL(loc: { pathname: string; search: string }): string {
  return `/auth/login?next=${encodeURIComponent(loc.pathname + loc.search)}`
}

let redirecting = false

// redirectToLogin sends the browser to sign in after a 401. The module-level
// guard stops a burst of failing queries from navigating repeatedly.
export function redirectToLogin(): void {
  if (redirecting) return
  redirecting = true
  window.location.assign(buildLoginURL(window.location))
}

// isUnauthorized: every lib/*Api.ts error class carries the HTTP status.
export function isUnauthorized(err: unknown): boolean {
  return (err as { status?: number } | null)?.status === 401
}
