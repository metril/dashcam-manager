// Theme mode: 'system' follows the OS/browser preference (prefers-color-scheme),
// 'light'/'dark' pin an explicit choice. The resolved value ('light' | 'dark') is
// what actually gets applied — as the `data-theme` attribute on <html> — since
// that's what index.css's [data-theme="light"] override selector matches.
//
// index.html carries a pre-paint inline script that duplicates the storage
// read + resolve + attribute-set logic here (it must run before the first
// paint and before any React/module code, so it can't import this module).
// Keep the two in sync if this logic changes.
export type ThemeMode = 'system' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'

const STORAGE_KEY = 'dashcam-theme'
export const THEME_CHANGE_EVENT = 'dashcam:theme-change'

export function getStoredMode(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // localStorage unavailable (private mode, disabled) — fall back silently.
  }
  return 'system'
}

function systemPrefersLight(): boolean {
  return typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: light)').matches === true
}

export function resolveTheme(mode: ThemeMode): ResolvedTheme {
  if (mode === 'system') return systemPrefersLight() ? 'light' : 'dark'
  return mode
}

function applyResolved(resolved: ResolvedTheme) {
  document.documentElement.setAttribute('data-theme', resolved)
}

// setThemeMode persists the choice and applies it immediately. Fires a
// window CustomEvent so any other mounted control (header toggle, Settings
// page) showing the current mode can re-sync without prop drilling or a
// shared store.
export function setThemeMode(mode: ThemeMode) {
  try {
    localStorage.setItem(STORAGE_KEY, mode)
  } catch {
    // Ignore — theme just won't persist across reloads this session.
  }
  applyResolved(resolveTheme(mode))
  window.dispatchEvent(new CustomEvent(THEME_CHANGE_EVENT))
}

// watchSystemTheme re-applies the resolved theme whenever the OS preference
// changes, but only while the stored mode is 'system' (an explicit
// light/dark choice should never be overridden by an OS-level change).
// Returns an unsubscribe function.
export function watchSystemTheme(): () => void {
  const mq = window.matchMedia?.('(prefers-color-scheme: light)')
  if (!mq) return () => {}
  const onChange = () => {
    if (getStoredMode() === 'system') applyResolved(resolveTheme('system'))
  }
  mq.addEventListener('change', onChange)
  return () => mq.removeEventListener('change', onChange)
}
