// Help modal (WP5): opened by the header "?" button and the "?" key
// (HelpButton.tsx owns both triggers). Accessible dialog pattern —
// role=dialog, aria-modal, focus trapped inside while open, Esc and
// backdrop click close it, focus returns to the trigger on close.
import { useEffect, useRef } from 'react'
import { createPortal } from 'react-dom'
import { X } from 'lucide-react'

interface HelpModalProps {
  open: boolean
  onClose: () => void
}

const PLAYER_SHORTCUTS: Array<[string, string]> = [
  ['Space', 'Play / pause'],
  ['K', 'Stop'],
  ['J / L', 'Jog backward / forward, faster with repeated presses'],
  [', / .', 'Step one frame backward / forward'],
  ['← / →', 'Seek backward / forward 5 seconds'],
  ['Shift + ← / →', 'Previous / next segment'],
  ['I / O', 'Mark clip in / out point'],
  ['S', 'Add current segment to the export bin'],
  ['Escape', 'Clear the pending clip selection'],
  ['D', 'Toggle detection overlay'],
  ['F', 'Toggle fullscreen'],
  ['1 / 2 / 3', 'Show front / rear / both cameras'],
  ['+ / - / 0', 'Zoom timeline in / out / reset'],
  ['?', 'Open this help'],
]

const HOW_TO: Array<{ group: string; pages: Array<[string, string]> }> = [
  {
    group: 'Footage',
    pages: [
      ['Browse', 'Pick a day on the calendar to see its trips, then open a trip to view segments and play footage.'],
      ['Search', 'Find detected text, license plates, and objects across every trip, filtered by date, camera, and class.'],
      ['Catalog', 'Browse detected brands, vehicle types, plates, and object classes, then click through to Search.'],
    ],
  },
  {
    group: 'Exports',
    pages: [
      ['Exports', 'Track export jobs you started from the player, download finished clips, or send them to a share.'],
      ['Shares', 'Manage network shares used as ingest sources or export destinations, and trigger scans or mounts.'],
    ],
  },
  {
    group: 'System',
    pages: [
      ['Ingest', 'Monitor the USB SSD used to pull new footage off the dashcam, and start or watch an ingest run.'],
      ['Jobs', 'See background jobs (scans, ingests, exports) in progress or recently finished, with logs and status.'],
      ['Stats', 'Review storage usage, ingest coverage, and detection counts across all sources.'],
      ['Settings', 'Configure sources, retention, and other app-wide preferences.'],
    ],
  },
]

const GLOSSARY: Array<[string, string]> = [
  ['Trip', 'A continuous drive, made up of one or more front/rear camera segments recorded back to back.'],
  ['Segment', 'One recorded video file for one camera, typically a few minutes long.'],
  ['Source', 'A place footage is read from or written to — the dashcam SSD or a network share.'],
  ['Ingest', 'Copying new footage off a source into the library and indexing it (segments, trips, detections).'],
  ['Archived', 'A segment whose original video was removed from its source after a verified ingest; its detections and thumbnails remain, but it can no longer be played.'],
  ['Export bin', 'A staging area for clip ranges or whole segments you plan to export together.'],
  ['Burn in', 'Render detection boxes directly into an exported video instead of keeping them as a separate overlay.'],
  ['Detection', 'An object, plate, or text region an automated pass found in a segment, with a class and confidence.'],
]

export default function HelpModal({ open, onClose }: HelpModalProps) {
  const dialogRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<Element | null>(null)

  // Remember what had focus before opening so it can be restored on close,
  // and move focus into the dialog for the focus trap below.
  useEffect(() => {
    if (!open) return
    triggerRef.current = document.activeElement
    dialogRef.current?.focus()
    return () => {
      if (triggerRef.current instanceof HTMLElement) triggerRef.current.focus()
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.stopPropagation()
        onClose()
        return
      }
      if (e.key !== 'Tab') return
      const root = dialogRef.current
      if (!root) return
      const focusable = root.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input, select, textarea, [tabindex]:not([tabindex="-1"])',
      )
      if (focusable.length === 0) return
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    window.addEventListener('keydown', onKeyDown, true)
    return () => window.removeEventListener('keydown', onKeyDown, true)
  }, [open, onClose])

  if (!open) return null

  return createPortal(
    <div
      className="fixed inset-0 z-[100] flex items-center justify-center bg-black/70 p-4"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="help-modal-title"
        tabIndex={-1}
        className="flex max-h-[85vh] w-full max-w-2xl flex-col overflow-hidden rounded-lg border border-neutral-800 bg-neutral-950 shadow-2xl outline-none"
      >
        <div className="flex shrink-0 items-center justify-between border-b border-neutral-900 px-5 py-3.5">
          <h2 id="help-modal-title" className="text-[14px] font-semibold text-neutral-200">
            Help
          </h2>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close help"
            className="rounded-md p-1 text-neutral-500 transition-colors hover:bg-neutral-900 hover:text-neutral-200"
          >
            <X className="size-4" />
          </button>
        </div>

        <div className="flex flex-col gap-6 overflow-y-auto px-5 py-4">
          <section>
            <h3 className="mb-2 text-[12px] font-semibold uppercase tracking-wide text-amber-400">
              Player shortcuts
            </h3>
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5">
              {PLAYER_SHORTCUTS.map(([key, desc]) => (
                <div key={key} className="contents">
                  <dt className="whitespace-nowrap rounded border border-neutral-800 bg-neutral-900 px-2 py-0.5 text-center font-mono text-[12px] text-neutral-300">
                    {key}
                  </dt>
                  <dd className="self-center text-[13px] text-neutral-300">{desc}</dd>
                </div>
              ))}
            </dl>
          </section>

          <section>
            <h3 className="mb-2 text-[12px] font-semibold uppercase tracking-wide text-amber-400">How to</h3>
            <div className="flex flex-col gap-3">
              {HOW_TO.map(({ group, pages }) => (
                <div key={group}>
                  <h4 className="mb-1 text-[12px] font-semibold text-neutral-400">{group}</h4>
                  <dl className="flex flex-col gap-1">
                    {pages.map(([page, desc]) => (
                      <div key={page} className="flex gap-2 text-[13px]">
                        <dt className="w-20 shrink-0 font-medium text-neutral-200">{page}</dt>
                        <dd className="text-neutral-300">{desc}</dd>
                      </div>
                    ))}
                  </dl>
                </div>
              ))}
            </div>
          </section>

          <section>
            <h3 className="mb-2 text-[12px] font-semibold uppercase tracking-wide text-amber-400">Glossary</h3>
            <dl className="flex flex-col gap-1.5">
              {GLOSSARY.map(([term, desc]) => (
                <div key={term} className="flex gap-2 text-[13px]">
                  <dt className="w-24 shrink-0 font-medium text-neutral-200">{term}</dt>
                  <dd className="text-neutral-300">{desc}</dd>
                </div>
              ))}
            </dl>
          </section>
        </div>
      </div>
    </div>,
    document.body,
  )
}
