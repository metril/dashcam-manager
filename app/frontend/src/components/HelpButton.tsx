// Header "?" button (WP5) that opens HelpModal. Also owns the global "?"
// key listener so help opens from anywhere, ignoring keystrokes while
// focus is in a text input, textarea, select, or contenteditable element.
// Placed in Layout.tsx's header by another agent: <HelpButton />.
import { useEffect, useState } from 'react'
import { HelpCircle } from 'lucide-react'
import HelpModal from './HelpModal'

export default function HelpButton() {
  const [open, setOpen] = useState(false)

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key !== '?' || e.ctrlKey || e.metaKey || e.altKey) return
      if (e.target instanceof HTMLElement) {
        if (['INPUT', 'TEXTAREA', 'SELECT'].includes(e.target.tagName)) return
        if (e.target.isContentEditable) return
      }
      e.preventDefault()
      setOpen(true)
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label="Help"
        className="flex items-center justify-center rounded-md p-1.5 text-text-muted transition-colors hover:bg-surface hover:text-text"
      >
        <HelpCircle className="size-4" strokeWidth={2} />
      </button>
      <HelpModal open={open} onClose={() => setOpen(false)} />
    </>
  )
}
