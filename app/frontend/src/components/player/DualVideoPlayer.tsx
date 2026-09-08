// DualVideoPlayer: the M6 trip player. Front is the "driver" — an ordered
// playlist of front segments that plays through and naturally advances on
// `ended`, with the next segment preloaded into a second <video> element
// so advancing is closer to seamless. Rear is "slaved": for whatever
// absolute instant the front video is currently showing, we look up which
// rear segment actually covers that instant (by absolute start/end time —
// deliberately NOT trip.pair_offset_s, which the M6 investigation found
// can be wrong; see the concern below) and keep it seeked to match within
// a 0.25s drift tolerance.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Camera,
  Eye,
  EyeOff,
  Gauge,
  Maximize,
  Pause,
  Play,
  Scissors,
  SkipBack,
  SkipForward,
} from 'lucide-react'
import { getSegmentDetections, getTripTimeline, segmentMediaUrl } from '../../lib/api'
import type { DetFrame, TripDetail } from '../../types'
import { formatDuration } from '../../lib/format'
import DetectionOverlay from './DetectionOverlay'
import TimelineStrip from './TimelineStrip'
import { useExportBin } from './ExportBinContext'
import { buildPlaylist, entryContaining, indexAtOrAfter, type PlaylistEntry } from './playerMath'

type Layout = 'front' | 'rear' | 'both'
const DRIFT_TOLERANCE_S = 0.25
const FRAME_STEP_S = 1 / 30

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

export default function DualVideoPlayer({ trip, jumpTo }: { trip: TripDetail; jumpTo?: JumpSignal | null }) {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const frontVideoRef = useRef<HTMLVideoElement | null>(null)
  const frontPreloadRef = useRef<HTMLVideoElement | null>(null)
  const rearVideoRef = useRef<HTMLVideoElement | null>(null)

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

  const currentFrame = frontDetections ? nearestFrame(frontDetections.detections, currentTimeS, 0.1) : null
  const gpsSpeedMps = currentFrame?.gps?.speed_mps ?? null
  const counts = classCounts(currentFrame)

  // --- load segment media into the front video + its preload buffer ---
  const frontSlotIds = useRef<{ active: number | null; preload: number | null }>({ active: null, preload: null })
  useEffect(() => {
    const el = frontVideoRef.current
    if (el && front && frontSlotIds.current.active !== front.segment.id) {
      el.src = segmentMediaUrl(front.segment.id)
      el.load()
      frontSlotIds.current.active = front.segment.id
      setCurrentTimeS(0)
    }
    const preload = frontPreloadRef.current
    if (preload) {
      if (nextFront && frontSlotIds.current.preload !== nextFront.segment.id) {
        preload.src = segmentMediaUrl(nextFront.segment.id)
        preload.load()
        frontSlotIds.current.preload = nextFront.segment.id
      } else if (!nextFront) {
        preload.removeAttribute('src')
        frontSlotIds.current.preload = null
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [front?.segment.id])

  // --- rear slaving: retarget src + drift-correct against currentAbsMs ---
  const rearSlotId = useRef<number | null>(null)
  useEffect(() => {
    const el = rearVideoRef.current
    if (!el) return
    const entry = entryContaining(rearPlaylist, currentAbsMs)
    if (!entry) {
      setRearMissing(true)
      return
    }
    setRearMissing(false)
    const targetLocalS = (currentAbsMs - entry.absStartMs) / 1000

    if (rearSlotId.current !== entry.segment.id) {
      el.src = segmentMediaUrl(entry.segment.id)
      el.load()
      rearSlotId.current = entry.segment.id
      const onLoaded = () => {
        el.currentTime = targetLocalS
        if (playing) void el.play().catch(() => {})
        el.removeEventListener('loadedmetadata', onLoaded)
      }
      el.addEventListener('loadedmetadata', onLoaded)
      return
    }

    if (Math.abs(el.currentTime - targetLocalS) > DRIFT_TOLERANCE_S) {
      el.currentTime = targetLocalS
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentAbsMs, rearPlaylist])

  // --- play/pause + forward rate follow `playing`/`rateSign`/`rateMag` ---
  const reverseTimerRef = useRef<number | null>(null)
  useEffect(() => {
    const fEl = frontVideoRef.current
    const rEl = rearVideoRef.current
    if (reverseTimerRef.current != null) {
      window.clearInterval(reverseTimerRef.current)
      reverseTimerRef.current = null
    }
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
      const el = frontVideoRef.current
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [playing, rateSign, rateMag])

  // --- front timeupdate -> currentTimeS (drives overlay/rear/timeline) ---
  useEffect(() => {
    const el = frontVideoRef.current
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
  }, [frontIdx, frontPlaylist.length])

  // when frontIdx advances mid-playback (via `ended` or reverse-to-0), keep playing
  useEffect(() => {
    const el = frontVideoRef.current
    if (el && playing && rateSign === 1) void el.play().catch(() => {})
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [frontIdx])

  const seekToOffsetS = useCallback(
    (offsetS: number) => {
      const clamped = Math.max(0, Math.min(trip.duration_s ?? offsetS, offsetS))
      const targetAbsMs = tripStartMs + clamped * 1000
      const idx = Math.min(frontPlaylist.length - 1, Math.max(0, indexAtOrAfter(frontPlaylist, targetAbsMs)))
      const entry = frontPlaylist[idx]
      if (!entry) return
      const localS = Math.max(0, (targetAbsMs - entry.absStartMs) / 1000)
      if (idx === frontIdx) {
        const el = frontVideoRef.current
        if (el) el.currentTime = localS
        setCurrentTimeS(localS)
      } else {
        setFrontIdx(idx)
        // the src-swap effect resets currentTimeS to 0; nudge it once loaded.
        const el = frontVideoRef.current
        if (el) {
          const onLoaded = () => {
            el.currentTime = localS
            el.removeEventListener('loadedmetadata', onLoaded)
          }
          el.addEventListener('loadedmetadata', onLoaded)
        }
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
    const el = frontVideoRef.current
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
      if (e.target instanceof HTMLElement && ['INPUT', 'TEXTAREA'].includes(e.target.tagName)) return
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
        case 'J':
          if (e.repeat) break
          setRateSign((sign) => {
            if (sign === 1) {
              setRateMag(2)
            } else {
              setRateMag((m) => Math.min(8, m * 2))
            }
            return -1
          })
          setPlaying(true)
          break
        case 'k':
        case 'K':
          setPlaying(false)
          setRateSign(1)
          setRateMag(1)
          break
        case 'l':
        case 'L':
          if (e.repeat) break
          setRateSign((sign) => {
            if (sign === -1) {
              setRateMag(2)
            } else {
              setRateMag((m) => Math.min(8, m * 2))
            }
            return 1
          })
          setPlaying(true)
          break
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
          if (front) exportBin.addSegment(front.segment.id)
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
  const rearEntry = rearPlaylist.find((e) => e.segment.id === rearSlotId.current)
  const rearOverlaySegId = rearEntry?.segment.has_detections ? rearEntry.segment.id : null

  return (
    <div ref={containerRef} className="flex flex-col gap-3 bg-black/20 p-3">
      <div className={['grid gap-2', layout === 'both' ? 'grid-cols-2' : 'grid-cols-1'].join(' ')}>
        {showFront && (
          <div className="relative aspect-video overflow-hidden rounded-md bg-black">
            <video ref={frontVideoRef} className="h-full w-full" playsInline muted={false} />
            <video ref={frontPreloadRef} className="hidden" playsInline muted preload="auto" />
            <DetectionOverlay
              segmentId={front?.segment.has_detections ? front.segment.id : null}
              currentTimeS={currentTimeS}
              videoEl={frontVideoRef.current}
              visible={overlayOn}
            />
            <span className="absolute left-2 top-2 rounded bg-black/60 px-1.5 py-0.5 text-[10px] font-medium text-neutral-300">
              front &middot; seg {front?.segment.seq ?? '—'}
            </span>
          </div>
        )}
        {showRear && (
          <div className="relative aspect-video overflow-hidden rounded-md bg-black">
            {/* The rear <video> stays mounted (just hidden) even when no rear
                segment currently covers the front's absolute time, so its ref
                never goes null — the rear-slaving effect needs a live element
                to detect the *next* time a covering segment appears. */}
            <video ref={rearVideoRef} className={['h-full w-full', rearMissing ? 'invisible' : ''].join(' ')} playsInline muted />
            <DetectionOverlay
              segmentId={rearOverlaySegId}
              currentTimeS={rearVideoRef.current?.currentTime ?? 0}
              videoEl={rearVideoRef.current}
              visible={overlayOn && !rearMissing}
            />
            {rearMissing && (
              <div className="absolute inset-0 flex items-center justify-center text-[12px] text-neutral-600">
                no rear footage at this time
              </div>
            )}
            <span className="absolute left-2 top-2 rounded bg-black/60 px-1.5 py-0.5 text-[10px] font-medium text-neutral-300">
              rear
            </span>
          </div>
        )}
      </div>

      <TimelineStrip timeline={timeline} currentOffsetS={currentOffsetS} onSeek={seekToOffsetS} />

      <div className="flex flex-wrap items-center gap-3 text-[12px] text-neutral-400">
        <button
          type="button"
          onClick={() => setPlaying((p) => !p)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 bg-neutral-900/60 px-2.5 py-1.5 text-neutral-200 hover:border-neutral-700"
        >
          {playing ? <Pause className="size-3.5" /> : <Play className="size-3.5" />}
          {playing ? (rateSign < 0 ? `${rateMag}x rev` : rateMag > 1 ? `${rateMag}x` : 'playing') : 'paused'}
        </button>
        <button
          type="button"
          onClick={() => stepSegment(-1)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          <SkipBack className="size-3.5" /> prev seg
        </button>
        <button
          type="button"
          onClick={() => stepSegment(1)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          <SkipForward className="size-3.5" /> next seg
        </button>
        <button
          type="button"
          onClick={() => setOverlayOn((v) => !v)}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          {overlayOn ? <Eye className="size-3.5" /> : <EyeOff className="size-3.5" />} overlay
        </button>
        <button
          type="button"
          onClick={toggleFullscreen}
          className="flex items-center gap-1 rounded-md border border-neutral-800 px-2 py-1.5 hover:border-neutral-700"
        >
          <Maximize className="size-3.5" /> {fullscreen ? 'exit' : 'fullscreen'}
        </button>
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
                .map(([k, v]) => `${v} ${k}`)
                .join(', ')}
            </span>
          )}
        </div>
      </div>

      <div className="flex items-center gap-3 border-t border-neutral-900 pt-2 text-[11px] text-neutral-500">
        <span className="flex items-center gap-1">
          <Scissors className="size-3.5" />
          {pending.inS != null ? formatDuration(pending.inS) : '—'} &rarr;{' '}
          {pending.outS != null ? formatDuration(pending.outS) : '—'}
        </span>
        <button
          type="button"
          disabled={pending.inS == null || pending.outS == null}
          onClick={() => exportBin.addPendingRange(trip.id)}
          className="rounded-md border border-neutral-800 px-2 py-1 text-neutral-300 disabled:opacity-40 hover:border-neutral-700"
        >
          Add range
        </button>
        <span className="text-neutral-700">
          {exportBin.state.ranges.length} range(s), {exportBin.state.segmentIds.length} segment(s) in export bin
        </span>
        <span className="ml-auto text-neutral-700">
          space play/pause &middot; J/K/L shuttle &middot; ,/. frame step &middot; I/O in/out &middot; S add segment
          &middot; D overlay &middot; F fullscreen &middot; 1/2/3 layout &middot; &larr;/&rarr; &plusmn;5s &middot;
          shift+&larr;/&rarr; segment &middot; esc clear
        </span>
      </div>
    </div>
  )
}
