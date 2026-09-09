// DualVideoPlayer: the M6 trip player. Front is the "driver" — an ordered
// playlist of front segments that plays through and naturally advances on
// `ended`. Front playback double-buffers across two <video> elements
// ("slots"): the inactive slot preloads the next segment while the active
// slot plays the current one, and advancing swaps which slot is active
// instead of re-assigning `src` on the element that's already playing —
// so each segment is fetched exactly once. Rear is "slaved": for whatever
// absolute instant the front video is currently showing, we look up which
// rear segment actually covers that instant (by absolute start/end time —
// deliberately NOT trip.pair_offset_s, which the M6 investigation found
// can be wrong; see the concern in m6-report.md) and keep it seeked to
// match within a 0.25s drift tolerance.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Camera,
  Download,
  Eye,
  EyeOff,
  Gauge,
  Maximize,
  Pause,
  Play,
  Plus,
  Scissors,
  SkipBack,
  SkipForward,
  X,
} from 'lucide-react'
import { getSegmentDetections, getTripTimeline } from '../../lib/api'
import { attachSegmentSource, clearSegmentSource, detachSegmentSource } from '../../lib/hlsPlayback'
import type { DetFrame, TripDetail, TripSegment } from '../../types'
import { formatDateLong, formatDuration, formatSegmentLabel, pluralize, titleCase } from '../../lib/format'
import DetectionOverlay from './DetectionOverlay'
import TimelineStrip from './TimelineStrip'
import { useExportBin } from './ExportBinContext'
import { buildPlaylist, entryContaining, indexAtOrAfter, type PlaylistEntry } from './playerMath'

type Layout = 'front' | 'rear' | 'both'
const DRIFT_TOLERANCE_S = 0.25
const FRAME_STEP_S = 1 / 30

// segmentBinLabel renders the same human label used everywhere a segment
// is shown by its recorder index (seq), not its opaque DB id — e.g. the
// segment tiles on TripDetail ("Segment 328") — so an export-bin entry
// added from here reads consistently: "Segment 328 · Front · 0:30".
function segmentBinLabel(segment: TripSegment): string {
  return `Segment ${segment.seq} · ${titleCase(segment.camera)} · ${formatDuration(segment.duration_s)}`
}

function classCounts(frame: DetFrame | null): Record<string, number> {
  if (!frame) return {}
  const out: Record<string, number> = {}
  for (const o of frame.objects) out[o.class] = (out[o.class] ?? 0) + 1
  if (frame.plates.length) out.plate = frame.plates.length
  return out
}

function nearestFrame(frames: DetFrame[], atS: number, toleranceS: number): DetFrame | null {
  let best: DetFrame | null = null
  let bestDelta = Infinity
  for (const f of frames) {
    const d = Math.abs(f.pts_offset - atS)
    if (d < bestDelta) {
      best = f
      bestDelta = d
    }
  }
  return best && bestDelta <= toleranceS ? best : null
}

export interface JumpSignal {
  startTime: string
  nonce: number
}

export default function DualVideoPlayer({
  trip,
  jumpTo,
  onActiveTimeChange,
  onExportSegment,
}: {
  trip: TripDetail
  jumpTo?: JumpSignal | null
  /** Called with the front playback position's absolute wall-clock ms whenever it changes (e.g. to sync a map marker). */
  onActiveTimeChange?: (absMs: number) => void
  /** One-click "Export this segment": called with the currently visible front segment's id. */
  onExportSegment?: (segmentId: number) => void
}) {
  const containerRef = useRef<HTMLDivElement | null>(null)

  // Front double-buffer: two <video> elements, tracked in state (not refs
  // read during render) so JSX/DetectionOverlay always see the element
  // that's actually mounted. `frontActiveSlot` says which one is visible
  // and playing; the other is the preload buffer.
  const [frontEls, setFrontEls] = useState<[HTMLVideoElement | null, HTMLVideoElement | null]>([null, null])
  const [frontActiveSlot, setFrontActiveSlot] = useState<0 | 1>(0)
  const setFrontSlot0 = useCallback((el: HTMLVideoElement | null) => {
    setFrontEls((prev) => (prev[0] === el ? prev : [el, prev[1]]))
  }, [])
  const setFrontSlot1 = useCallback((el: HTMLVideoElement | null) => {
    setFrontEls((prev) => (prev[1] === el ? prev : [prev[0], el]))
  }, [])
  const activeFrontEl = frontEls[frontActiveSlot]
  const inactiveFrontEl = frontEls[1 - frontActiveSlot]
  // Which segment id is currently loaded into each slot — a ref (not
  // state) since it's only consulted inside effects, never during render.
  const frontSlotSegId = useRef<[number | null, number | null]>([null, null])
  // A ref mirror of `activeFrontEl`, kept fresh for effects/interval
  // callbacks that must always read the *current* active element without
  // being re-created every time it changes (e.g. the reverse-shuttle
  // interval).
  const activeFrontElRef = useRef<HTMLVideoElement | null>(null)
  useEffect(() => {
    activeFrontElRef.current = activeFrontEl
  }, [activeFrontEl])
  // Mirror of frontEls, kept fresh purely so the unmount cleanup effect
  // below can detach any hls.js instance still attached to either slot
  // without needing frontEls itself in its dependency array (which would
  // re-run the cleanup — tearing down and re-creating the effect — on
  // every slot swap instead of only on unmount).
  const frontElsRef = useRef<[HTMLVideoElement | null, HTMLVideoElement | null]>([null, null])
  useEffect(() => {
    frontElsRef.current = frontEls
  }, [frontEls])

  const [rearEl, setRearEl] = useState<HTMLVideoElement | null>(null)
  const setRearElCb = useCallback((el: HTMLVideoElement | null) => {
    setRearEl((prev) => (prev === el ? prev : el))
  }, [])
  const rearElRef = useRef<HTMLVideoElement | null>(null)
  useEffect(() => {
    rearElRef.current = rearEl
  }, [rearEl])
  const rearSlotSegId = useRef<number | null>(null)
  const [rearActiveSegId, setRearActiveSegId] = useState<number | null>(null)
  const [rearCurrentTimeS, setRearCurrentTimeS] = useState(0)

  // Destroy any hls.js instance still attached to a front slot or the
  // rear element when the player unmounts (trip navigated away from) —
  // hls.js instances hold their own buffers/worker state that a plain
  // React unmount (which just detaches the DOM node) wouldn't otherwise
  // clean up. Runs once on mount/unmount only; see frontElsRef's comment
  // for why it doesn't depend on frontEls/rearEl directly.
  useEffect(() => {
    return () => {
      for (const el of frontElsRef.current) {
        if (el) detachSegmentSource(el)
      }
      if (rearElRef.current) detachSegmentSource(rearElRef.current)
    }
  }, [])

  const frontPlaylist = useMemo(() => buildPlaylist(trip.segments, 'front'), [trip.segments])
  const rearPlaylist = useMemo(() => buildPlaylist(trip.segments, 'rear'), [trip.segments])

  const [layout, setLayout] = useState<Layout>(trip.front && trip.rear ? 'both' : trip.rear ? 'rear' : 'front')
  const [overlayOn, setOverlayOn] = useState(true)
  const [frontIdx, setFrontIdx] = useState(0)
  const [playing, setPlaying] = useState(false)
  const [rateSign, setRateSign] = useState<1 | -1>(1)
  const [rateMag, setRateMag] = useState(1)
  const [currentTimeS, setCurrentTimeS] = useState(0) // within the active front segment
  const [fullscreen, setFullscreen] = useState(false)
  const [rearMissing, setRearMissing] = useState(false)
  // Non-null (possibly "") while the rear segment currently covering
  // currentAbsMs is archived (final-fix-wave item 7 / residual-findings
  // item 2) — its original file is gone, so there's nothing to attach a
  // source to; shown as an overlay instead of a broken/blank video.
  const [rearArchivedToShare, setRearArchivedToShare] = useState<string | null>(null)
  // Non-null while the rear segment currently covering currentAbsMs is
  // unreadable (see TripSegment.unreadable_reason) — same "nothing to
  // attach a source to, show a message instead" treatment as archived
  // above.
  const [rearUnreadableReason, setRearUnreadableReason] = useState<string | null>(null)

  // Mirrors of `playing`/`rateSign`/`rateMag` for use inside effects that
  // must not themselves depend on (and thus re-run/re-attach listeners on
  // every change of) those values — e.g. deciding whether a *newly
  // loaded* segment should auto-play.
  const playingRef = useRef(playing)
  useEffect(() => {
    playingRef.current = playing
  }, [playing])
  const rateRef = useRef({ sign: rateSign, mag: rateMag })
  useEffect(() => {
    rateRef.current = { sign: rateSign, mag: rateMag }
  }, [rateSign, rateMag])

  const exportBin = useExportBin()
  const pending = exportBin.pendingFor(trip.id)

  const front: PlaylistEntry | undefined = frontPlaylist[frontIdx]
  const nextFront: PlaylistEntry | undefined = frontPlaylist[frontIdx + 1]

  const { data: timeline } = useQuery({
    queryKey: ['trip-timeline', trip.id],
    queryFn: () => getTripTimeline(trip.id),
  })

  const { data: frontDetections } = useQuery({
    queryKey: ['segment-detections', front?.segment.id ?? null],
    queryFn: () => getSegmentDetections(front!.segment.id),
    enabled: front != null && front.segment.has_detections,
    staleTime: Infinity,
  })

  const currentAbsMs = front != null ? front.absStartMs + currentTimeS * 1000 : 0
  const tripStartMs = Date.parse(trip.started_at)
  const currentOffsetS = (currentAbsMs - tripStartMs) / 1000

  useEffect(() => {
    onActiveTimeChange?.(currentAbsMs)
  }, [currentAbsMs, onActiveTimeChange])

  const currentFrame = frontDetections ? nearestFrame(frontDetections.detections, currentTimeS, 0.1) : null
  const gpsSpeedMps = currentFrame?.gps?.speed_mps ?? null
  const counts = classCounts(currentFrame)

  const rearEntry = useMemo(
    () => rearPlaylist.find((e) => e.segment.id === rearActiveSegId) ?? null,
    [rearPlaylist, rearActiveSegId],
  )
  const rearOverlaySegId = rearEntry?.segment.has_detections ? rearEntry.segment.id : null

  // Keep active/inactive front slots' `muted` in sync with which one is
  // playing — the active slot carries audio, the preload buffer stays
  // muted so it can't be heard before it's swapped in.
  useEffect(() => {
    frontEls.forEach((el, i) => {
      if (el) el.muted = i !== frontActiveSlot
    })
  }, [frontEls, frontActiveSlot])

  // A pending "land at this local time" request for the front video, set
  // by seekToOffsetS when the seek target is a different segment than the
  // one currently active — consumed by the load/swap effect below once
  // the target segment is actually ready (either by swap-from-preload, or
  // after `loadedmetadata` for a fresh load).
  const pendingFrontSeekRef = useRef<number | null>(null)

  // --- load/advance the front playlist: swap the preloaded slot into
  // place when possible (each segment fetched once); fall back to a
  // direct load on the active slot for a jump that skipped preloading. ---
  useEffect(() => {
    if (!front) return
    const wantId = front.segment.id

    if (frontSlotSegId.current[frontActiveSlot] === wantId) {
      // Already showing the right segment (e.g. re-render without an
      // index change) — nothing to do.
      return
    }

    if (frontSlotSegId.current[1 - frontActiveSlot] === wantId && inactiveFrontEl) {
      // The preload buffer already has it — swap slots instead of
      // re-fetching.
      const newActive = inactiveFrontEl
      const oldActive = activeFrontEl
      oldActive?.pause()
      setFrontActiveSlot((s) => (s === 0 ? 1 : 0))
      const pending = pendingFrontSeekRef.current
      pendingFrontSeekRef.current = null
      const target = pending ?? 0
      newActive.currentTime = target
      setCurrentTimeS(target)
      if (playingRef.current && rateRef.current.sign === 1) {
        void newActive.play().catch(() => {})
      }
      return
    }

    if (!activeFrontEl) return
    // Not preloaded — load it directly into the active slot.
    const el = activeFrontEl
    attachSegmentSource(el, wantId)
    frontSlotSegId.current[frontActiveSlot] = wantId
    const onLoaded = () => {
      const pending = pendingFrontSeekRef.current
      pendingFrontSeekRef.current = null
      const target = pending ?? 0
      el.currentTime = target
      setCurrentTimeS(target)
      if (playingRef.current && rateRef.current.sign === 1) {
        void el.play().catch(() => {})
      }
    }
    el.addEventListener('loadedmetadata', onLoaded, { once: true })
    return () => el.removeEventListener('loadedmetadata', onLoaded)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [front?.segment.id, activeFrontEl, inactiveFrontEl, frontActiveSlot])

  // --- preload the next front segment into whichever slot is inactive ---
  useEffect(() => {
    if (!inactiveFrontEl) return
    if (nextFront) {
      if (frontSlotSegId.current[1 - frontActiveSlot] !== nextFront.segment.id) {
        attachSegmentSource(inactiveFrontEl, nextFront.segment.id)
        frontSlotSegId.current[1 - frontActiveSlot] = nextFront.segment.id
      }
    } else if (frontSlotSegId.current[1 - frontActiveSlot] !== null) {
      clearSegmentSource(inactiveFrontEl)
      frontSlotSegId.current[1 - frontActiveSlot] = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nextFront?.segment.id, inactiveFrontEl, frontActiveSlot])

  // --- rear slaving: retarget src + drift-correct against currentAbsMs ---
  useEffect(() => {
    const el = rearEl
    if (!el) return
    const entry = entryContaining(rearPlaylist, currentAbsMs)
    if (!entry) {
      setRearMissing(true)
      setRearArchivedToShare(null)
      setRearUnreadableReason(null)
      return
    }
    setRearMissing(false)

    if (entry.segment.archived_at) {
      // Nothing to play — don't even attempt attachSegmentSource (it
      // would just 404). Clear any stale source/segment id so a later
      // non-archived entry is treated as a fresh load.
      setRearArchivedToShare(entry.segment.archived_to_share ?? '')
      setRearUnreadableReason(null)
      if (rearSlotSegId.current !== null) {
        clearSegmentSource(el)
        rearSlotSegId.current = null
        setRearActiveSegId(null)
      }
      return
    }
    if (entry.segment.unreadable_reason) {
      // Same idea for a file that decodes as broken rather than gone.
      setRearUnreadableReason(entry.segment.unreadable_reason)
      setRearArchivedToShare(null)
      if (rearSlotSegId.current !== null) {
        clearSegmentSource(el)
        rearSlotSegId.current = null
        setRearActiveSegId(null)
      }
      return
    }
    setRearArchivedToShare(null)
    setRearUnreadableReason(null)
    const targetLocalS = (currentAbsMs - entry.absStartMs) / 1000

    if (rearSlotSegId.current !== entry.segment.id) {
      attachSegmentSource(el, entry.segment.id)
      rearSlotSegId.current = entry.segment.id
      setRearActiveSegId(entry.segment.id)
      const onLoaded = () => {
        el.currentTime = targetLocalS
        setRearCurrentTimeS(targetLocalS)
        if (playingRef.current) void el.play().catch(() => {})
      }
      el.addEventListener('loadedmetadata', onLoaded, { once: true })
      return () => el.removeEventListener('loadedmetadata', onLoaded)
    }

    if (Math.abs(el.currentTime - targetLocalS) > DRIFT_TOLERANCE_S) {
      el.currentTime = targetLocalS
    }
  }, [currentAbsMs, rearPlaylist, rearEl])

  // --- rear timeupdate -> rearCurrentTimeS, so the overlay always has a
  // fresh time without reading the element ref during render ---
  useEffect(() => {
    const el = rearEl
    if (!el) return
    const onTimeUpdate = () => setRearCurrentTimeS(el.currentTime)
    el.addEventListener('timeupdate', onTimeUpdate)
    return () => el.removeEventListener('timeupdate', onTimeUpdate)
  }, [rearEl])

  // --- play/pause + forward rate follow `playing`/`rateSign`/`rateMag` ---
  const reverseTimerRef = useRef<number | null>(null)
  useEffect(() => {
    if (reverseTimerRef.current != null) {
      window.clearInterval(reverseTimerRef.current)
      reverseTimerRef.current = null
    }
    const fEl = activeFrontEl
    const rEl = rearEl
    if (!fEl) return

    if (!playing) {
      fEl.pause()
      rEl?.pause()
      return
    }

    if (rateSign === 1) {
      fEl.playbackRate = rateMag
      void fEl.play().catch(() => {})
      if (rEl) {
        rEl.playbackRate = rateMag
        void rEl.play().catch(() => {})
      }
      return
    }

    // Native <video> can't play backward — step currentTime down manually;
    // each assignment fires `timeupdate`, which drives the rear resync
    // effect above too, so reverse shuttling stays in sync the same way
    // forward playback does.
    fEl.pause()
    rEl?.pause()
    reverseTimerRef.current = window.setInterval(() => {
      const el = activeFrontElRef.current
      if (!el) return
      const next = el.currentTime - 0.1 * rateMag
      if (next <= 0) {
        if (frontIdx > 0) {
          setFrontIdx((i) => i - 1)
        } else {
          el.currentTime = 0
          setPlaying(false)
        }
        return
      }
      el.currentTime = next
    }, 100)

    return () => {
      if (reverseTimerRef.current != null) window.clearInterval(reverseTimerRef.current)
    }
  }, [playing, rateSign, rateMag, activeFrontEl, rearEl, frontIdx])

  // --- front timeupdate -> currentTimeS (drives overlay/rear/timeline);
  // re-attaches whenever the active slot swaps so listeners always live on
  // the currently-playing element, and cleans up on every change. ---
  useEffect(() => {
    const el = activeFrontEl
    if (!el) return
    const onTimeUpdate = () => setCurrentTimeS(el.currentTime)
    const onEnded = () => {
      if (frontIdx + 1 < frontPlaylist.length) {
        setFrontIdx((i) => i + 1)
      } else {
        setPlaying(false)
      }
    }
    el.addEventListener('timeupdate', onTimeUpdate)
    el.addEventListener('ended', onEnded)
    return () => {
      el.removeEventListener('timeupdate', onTimeUpdate)
      el.removeEventListener('ended', onEnded)
    }
  }, [activeFrontEl, frontIdx, frontPlaylist.length])

  const seekToOffsetS = useCallback(
    (offsetS: number) => {
      const clamped = Math.max(0, Math.min(trip.duration_s ?? offsetS, offsetS))
      const targetAbsMs = tripStartMs + clamped * 1000
      const idx = Math.min(frontPlaylist.length - 1, Math.max(0, indexAtOrAfter(frontPlaylist, targetAbsMs)))
      const entry = frontPlaylist[idx]
      if (!entry) return
      const localS = Math.max(0, (targetAbsMs - entry.absStartMs) / 1000)
      if (idx === frontIdx) {
        const el = activeFrontElRef.current
        if (el) el.currentTime = localS
        setCurrentTimeS(localS)
      } else {
        pendingFrontSeekRef.current = localS
        setFrontIdx(idx)
      }
    },
    [frontPlaylist, frontIdx, trip.duration_s, tripStartMs],
  )

  useEffect(() => {
    if (!jumpTo) return
    const offsetS = (Date.parse(jumpTo.startTime) - tripStartMs) / 1000
    seekToOffsetS(offsetS)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jumpTo?.nonce])

  const stepSegment = useCallback(
    (dir: 1 | -1) => {
      const idx = Math.min(frontPlaylist.length - 1, Math.max(0, frontIdx + dir))
      if (idx !== frontIdx) setFrontIdx(idx)
    },
    [frontIdx, frontPlaylist.length],
  )

  const stepFrame = useCallback((dir: 1 | -1) => {
    setPlaying(false)
    setRateSign(1)
    setRateMag(1)
    const el = activeFrontElRef.current
    if (el) el.currentTime = Math.max(0, el.currentTime + dir * FRAME_STEP_S)
  }, [])

  const toggleFullscreen = useCallback(() => {
    const el = containerRef.current
    if (!el) return
    if (document.fullscreenElement) {
      void document.exitFullscreen()
    } else {
      void el.requestFullscreen()
    }
  }, [])

  useEffect(() => {
    const onFsChange = () => setFullscreen(document.fullscreenElement === containerRef.current)
    document.addEventListener('fullscreenchange', onFsChange)
    return () => document.removeEventListener('fullscreenchange', onFsChange)
  }, [])

  // --- keyboard shortcuts ---
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      // Leave browser/OS chords (copy, save, devtools, ...) and any typing
      // context (inputs, selects, contenteditable) alone.
      if (e.ctrlKey || e.metaKey) return
      if (e.target instanceof HTMLElement) {
        if (['INPUT', 'TEXTAREA', 'SELECT'].includes(e.target.tagName)) return
        if (e.target.isContentEditable) return
      }
      switch (e.key) {
        case ' ':
          e.preventDefault()
          setPlaying((p) => {
            const next = !p
            if (next) {
              setRateSign(1)
              setRateMag(1)
            }
            return next
          })
          break
        case 'j':
        case 'J': {
          if (e.repeat) break
          const { sign, mag } = rateRef.current
          const newMag = sign === 1 ? 2 : Math.min(8, mag * 2)
          setRateSign(-1)
          setRateMag(newMag)
          setPlaying(true)
          break
        }
        case 'k':
        case 'K':
          setPlaying(false)
          setRateSign(1)
          setRateMag(1)
          break
        case 'l':
        case 'L': {
          if (e.repeat) break
          const { sign, mag } = rateRef.current
          const newMag = sign === -1 ? 2 : Math.min(8, mag * 2)
          setRateSign(1)
          setRateMag(newMag)
          setPlaying(true)
          break
        }
        case ',':
          stepFrame(-1)
          break
        case '.':
          stepFrame(1)
          break
        case 'i':
        case 'I':
          exportBin.setIn(trip.id, currentOffsetS)
          break
        case 'o':
        case 'O':
          exportBin.setOut(trip.id, currentOffsetS)
          break
        case 's':
        case 'S':
          if (front && !front.segment.unreadable_reason) exportBin.addSegment(front.segment.id, segmentBinLabel(front.segment))
          break
        case 'd':
        case 'D':
          setOverlayOn((v) => !v)
          break
        case 'f':
        case 'F':
          toggleFullscreen()
          break
        case '1':
          setLayout('front')
          break
        case '2':
          setLayout('rear')
          break
        case '3':
          setLayout('both')
          break
        case 'ArrowRight':
          if (e.shiftKey) stepSegment(1)
          else seekToOffsetS(currentOffsetS + 5)
          break
        case 'ArrowLeft':
          if (e.shiftKey) stepSegment(-1)
          else seekToOffsetS(currentOffsetS - 5)
          break
        case 'Escape':
          exportBin.clearPending(trip.id)
          break
        default:
          return
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [currentOffsetS, exportBin, front, seekToOffsetS, stepFrame, stepSegment, toggleFullscreen, trip.id])

  const showFront = layout !== 'rear'
  const showRear = layout !== 'front'

  return (
    <div ref={containerRef} className="flex flex-col gap-3 bg-black/20 p-3">
      <div className={['grid gap-2', layout === 'both' ? 'grid-cols-2' : 'grid-cols-1'].join(' ')}>
        {showFront && (
          <div className="relative aspect-video overflow-hidden rounded-md bg-black">
            <video ref={setFrontSlot0} className={['h-full w-full', frontActiveSlot === 0 ? '' : 'hidden'].join(' ')} playsInline />
            <video ref={setFrontSlot1} className={['h-full w-full', frontActiveSlot === 1 ? '' : 'hidden'].join(' ')} playsInline />
            <DetectionOverlay
              segmentId={front?.segment.has_detections ? front.segment.id : null}
              currentTimeS={currentTimeS}
              videoEl={activeFrontEl}
              visible={overlayOn}
            />
            <span className="absolute left-2 top-2 rounded bg-black/60 px-1.5 py-0.5 text-xs font-medium text-neutral-300">
              {formatSegmentLabel('Front', front?.segment.seq)}
            </span>
            {front?.segment.archived_at && (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-black/70 text-center text-[12px] text-amber-400">
                <span>Archived{front.segment.archived_to_share ? ` to ${front.segment.archived_to_share}` : ''}</span>
                <span className="text-xs text-neutral-500">Original video no longer available</span>
              </div>
            )}
            {front?.segment.unreadable_reason && (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-black/70 text-center text-[12px] text-red-400">
                <span>This segment&apos;s file is unreadable</span>
                <span className="text-xs text-neutral-500">{front.segment.unreadable_reason}</span>
              </div>
            )}
          </div>
        )}
        {showRear && (
          <div className="relative aspect-video overflow-hidden rounded-md bg-black">
            {/* The rear <video> stays mounted (just hidden) even when no rear
                segment currently covers the front's absolute time, so its ref
                never goes null — the rear-slaving effect needs a live element
                to detect the *next* time a covering segment appears. */}
            <video
              ref={setRearElCb}
              className={[
                'h-full w-full',
                rearMissing || rearArchivedToShare !== null || rearUnreadableReason !== null ? 'invisible' : '',
              ].join(' ')}
              playsInline
              muted
            />
            <DetectionOverlay
              segmentId={rearOverlaySegId}
              currentTimeS={rearCurrentTimeS}
              videoEl={rearEl}
              visible={overlayOn && !rearMissing && rearArchivedToShare === null && rearUnreadableReason === null}
            />
            {rearMissing && (
              <div className="absolute inset-0 flex items-center justify-center text-[12px] text-neutral-400">
                No rear footage at this time
              </div>
            )}
            {rearArchivedToShare !== null && (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-black/70 text-center text-[12px] text-amber-400">
                <span>Archived{rearArchivedToShare ? ` to ${rearArchivedToShare}` : ''}</span>
                <span className="text-xs text-neutral-500">Original video no longer available</span>
              </div>
            )}
            {rearUnreadableReason !== null && (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-black/70 text-center text-[12px] text-red-400">
                <span>This segment&apos;s file is unreadable</span>
                <span className="text-xs text-neutral-500">{rearUnreadableReason}</span>
              </div>
            )}
            <span className="absolute left-2 top-2 rounded bg-black/60 px-1.5 py-0.5 text-xs font-medium text-neutral-300">
              Rear
            </span>
          </div>
        )}
      </div>

      <TimelineStrip
        timeline={timeline}
        currentOffsetS={currentOffsetS}
        onSeek={seekToOffsetS}
        onRangeSelect={(inS, outS) => {
          exportBin.setIn(trip.id, inS)
          exportBin.setOut(trip.id, outS)
        }}
      />

      <div className="flex flex-wrap items-center gap-3 text-[12px] text-neutral-400">
        <button
          type="button"
          onClick={() => setPlaying((p) => !p)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 bg-neutral-900/60 px-2.5 py-1.5 text-neutral-200 hover:border-neutral-700"
        >
          {playing ? <Pause className="size-3.5" /> : <Play className="size-3.5" />}
          {playing ? (rateSign < 0 ? `${rateMag}x rev` : rateMag > 1 ? `${rateMag}x` : 'Playing') : 'Paused'}
        </button>
        <button
          type="button"
          onClick={() => stepSegment(-1)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          <SkipBack className="size-3.5" /> Previous segment
        </button>
        <button
          type="button"
          onClick={() => stepSegment(1)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          <SkipForward className="size-3.5" /> Next segment
        </button>
        <button
          type="button"
          onClick={() => setOverlayOn((v) => !v)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          {overlayOn ? <Eye className="size-3.5" /> : <EyeOff className="size-3.5" />} Overlay
        </button>
        <button
          type="button"
          onClick={toggleFullscreen}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          <Maximize className="size-3.5" /> {fullscreen ? 'Exit fullscreen' : 'Fullscreen'}
        </button>
        {onExportSegment && (
          <button
            type="button"
            disabled={front == null || !!front.segment.unreadable_reason}
            title={front?.segment.unreadable_reason ? "This segment's file is unreadable — can't export it" : undefined}
            onClick={() => front && onExportSegment(front.segment.id)}
            className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700 disabled:opacity-40"
          >
            <Download className="size-3.5" /> Export this segment
          </button>
        )}
        <div className="ml-auto flex items-center gap-3 tabular-nums">
          <span>{formatDuration(currentOffsetS)}</span>
          {gpsSpeedMps != null && (
            <span className="flex items-center gap-1">
              <Gauge className="size-3.5" /> {Math.round(gpsSpeedMps * 3.6)} km/h
            </span>
          )}
          {Object.keys(counts).length > 0 && (
            <span className="flex items-center gap-1 text-neutral-500">
              <Camera className="size-3.5" />
              {Object.entries(counts)
                .map(([k, v]) => `${v} ${titleCase(k)}`)
                .join(', ')}
            </span>
          )}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2 border-t border-neutral-800 pt-2 text-xs text-neutral-500">
        <button
          type="button"
          onClick={() => exportBin.setIn(trip.id, currentOffsetS)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1 text-neutral-300 hover:border-neutral-700"
        >
          <Scissors className="size-3.5" /> Mark in
        </button>
        <button
          type="button"
          onClick={() => exportBin.setOut(trip.id, currentOffsetS)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1 text-neutral-300 hover:border-neutral-700"
        >
          <Scissors className="size-3.5" /> Mark out
        </button>
        <span className="flex items-center gap-1 tabular-nums">
          {pending.inS != null ? formatDuration(pending.inS) : '—'} &rarr;{' '}
          {pending.outS != null ? formatDuration(pending.outS) : '—'}
        </span>
        <button
          type="button"
          disabled={pending.inS == null && pending.outS == null}
          onClick={() => exportBin.clearPending(trip.id)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1 text-neutral-300 disabled:opacity-40 hover:border-neutral-700"
        >
          <X className="size-3.5" /> Clear
        </button>
        <button
          type="button"
          disabled={pending.inS == null || pending.outS == null}
          onClick={() => exportBin.addPendingRange(trip.id, formatDateLong(trip.started_at))}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1 text-neutral-300 disabled:opacity-40 hover:border-neutral-700"
        >
          <Plus className="size-3.5" /> Add range
        </button>
        <button
          type="button"
          disabled={front == null || !!front.segment.unreadable_reason}
          title={front?.segment.unreadable_reason ? "This segment's file is unreadable — can't add it" : undefined}
          onClick={() => front && exportBin.addSegment(front.segment.id, segmentBinLabel(front.segment))}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1 text-neutral-300 disabled:opacity-40 hover:border-neutral-700"
        >
          <Plus className="size-3.5" /> Add segment
        </button>
        {front?.segment.unreadable_reason && (
          <span className="text-xs text-red-400">This segment&apos;s file is unreadable, so it can&apos;t be exported.</span>
        )}
        <span className="ml-auto text-neutral-400">
          {exportBin.state.ranges.length === 0 && exportBin.state.segments.length === 0
            ? 'Export bin is empty'
            : [
                exportBin.state.ranges.length > 0 ? pluralize(exportBin.state.ranges.length, 'range') : null,
                exportBin.state.segments.length > 0 ? pluralize(exportBin.state.segments.length, 'segment') : null,
              ]
                .filter(Boolean)
                .join(' and ') + ' in export bin'}
        </span>
      </div>
    </div>
  )
}
