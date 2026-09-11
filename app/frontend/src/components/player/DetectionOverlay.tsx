// DetectionOverlay draws normalized-bbox detections (objects + plates) for
// the current playback position onto a canvas stacked over a <video>
// element, honoring the video's object-fit: contain letterboxing so boxes
// stay pinned to the actual picture rect rather than the element's box.
//
// Drawing is driven directly off the video element's own frame clock —
// `requestVideoFrameCallback` (falling back to `requestAnimationFrame` +
// `video.currentTime` where rVFC isn't available) — rather than the
// `currentTimeS` prop, which only updates at the `timeupdate` rate (~4Hz)
// and would otherwise make boxes visibly lag the picture. Per-frame time
// lives in refs, never React state, so the loop doesn't fight React's
// render cycle. Detections themselves are sparse (30-150ms apart), so the
// actual box for a given instant comes from detectionInterp's per-track
// interpolation rather than "whichever raw sample is nearest."
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { defaultDetectionFilter, getSegmentDetections, type DetectionFilter } from '../../lib/api'
import { THEME_CHANGE_EVENT } from '../../lib/theme'
import { boxesAt, buildTrackIndex, type TrackIndex } from './detectionInterp'
import type { DetObject, DetPlate, DetVehicle } from '../../types'

// Object classes bucket into the det-* CSS custom properties defined in
// index.css (vehicle/person/other; sign/plate exist for future classes and
// the plate overlay below). Colors are read via getComputedStyle rather
// than hardcoded hex so the canvas tracks the active theme — index.css's
// [data-theme="light"] override changes these vars, and a canvas draw call
// can't pick that up from a CSS class the way DOM elements do. Resolved
// once per theme change (see colorMap below), not per box per frame.
const CLASS_TOKEN: Record<string, string> = {
  car: '--color-det-vehicle',
  truck: '--color-det-vehicle',
  bus: '--color-det-vehicle',
  motorcycle: '--color-det-vehicle',
  bicycle: '--color-det-vehicle',
  person: '--color-det-person',
}
const DEFAULT_TOKEN = '--color-det-other'
const PLATE_TOKEN = '--color-det-plate'
const VEHICLE_TOKEN = '--color-det-vehicle'

// resolveColor reads a CSS custom property's current computed value (a
// color, since index.css defines these under @theme / [data-theme]) off
// document.documentElement. Falls back to a neutral gray if the var is
// somehow unset (e.g. styles not yet loaded).
function resolveColor(varName: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(varName).trim()
  return v || '#a3a3a3'
}

// All det-* CSS custom property names resolveColor might be asked for,
// resolved once per theme change rather than per box per frame.
const ALL_COLOR_TOKENS = [
  '--color-det-vehicle',
  '--color-det-person',
  '--color-det-other',
  '--color-det-plate',
]

// videoContentRect computes the rendered rect of the actual video picture
// within its element's box, accounting for object-fit: contain
// letterboxing (pillarboxing when the video is narrower than the element,
// letterboxing when it's shorter).
function videoContentRect(video: HTMLVideoElement): { x: number; y: number; w: number; h: number } {
  const elW = video.clientWidth
  const elH = video.clientHeight
  const vw = video.videoWidth || elW
  const vh = video.videoHeight || elH
  if (vw === 0 || vh === 0 || elW === 0 || elH === 0) return { x: 0, y: 0, w: elW, h: elH }

  const elRatio = elW / elH
  const vRatio = vw / vh
  if (vRatio > elRatio) {
    // video is relatively wider than the element -> letterboxed top/bottom
    const h = elW / vRatio
    return { x: 0, y: (elH - h) / 2, w: elW, h }
  }
  const w = elH * vRatio
  return { x: (elW - w) / 2, y: 0, w, h: elH }
}

export default function DetectionOverlay({
  segmentId,
  currentTimeS,
  videoEl,
  visible,
  filter = defaultDetectionFilter,
}: {
  segmentId: number | null
  // Only used as a one-shot fallback (initial paint before the video/loop
  // has produced any frame time yet); the rVFC/rAF loop below is what
  // actually drives drawing during playback.
  currentTimeS: number
  videoEl: HTMLVideoElement | null
  visible: boolean
  /** Which detection boxes to draw — defaults to showing everything above the default confidence floor. */
  filter?: DetectionFilter
}) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  // Bumped by a ResizeObserver whenever the video element's rendered box
  // changes (window resize, layout toggle between front/rear/both, etc.)
  // so the canvas backing store is resized and a paused frame redraws.
  const [resizeTick, setResizeTick] = useState(0)
  // Bumped when the theme toggles (Layout/Settings call setThemeMode, which
  // dispatches this) so a paused frame redraws with the new det-* colors
  // instead of keeping stale ones from before the switch.
  const [themeTick, setThemeTick] = useState(0)

  useEffect(() => {
    const onThemeChange = () => setThemeTick((t) => t + 1)
    window.addEventListener(THEME_CHANGE_EVENT, onThemeChange)
    return () => window.removeEventListener(THEME_CHANGE_EVENT, onThemeChange)
  }, [])

  // Resolve every det-* color once per theme change instead of per box per
  // frame (getComputedStyle is not free at 60fps with many boxes).
  const colorMap = useMemo(() => {
    const map = new Map<string, string>()
    for (const token of ALL_COLOR_TOKENS) map.set(token, resolveColor(token))
    return map
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [themeTick])

  const { data } = useQuery({
    queryKey: ['segment-detections', segmentId],
    queryFn: () => getSegmentDetections(segmentId!),
    enabled: segmentId != null && visible,
    staleTime: Infinity,
  })

  const trackIndex = useMemo<TrackIndex | null>(() => (data ? buildTrackIndex(data.detections) : null), [data])

  useEffect(() => {
    const video = videoEl
    if (!video) return
    const ro = new ResizeObserver(() => setResizeTick((t) => t + 1))
    ro.observe(video)
    return () => ro.disconnect()
  }, [videoEl])

  // Resize the canvas backing store to match the video element's current
  // rendered box + devicePixelRatio. Separate from the actual box-drawing
  // below so a 60fps draw loop isn't also resizing/clearing transforms
  // every frame.
  useEffect(() => {
    const canvas = canvasRef.current
    const video = videoEl
    if (!canvas || !video) return
    const rect = video.getBoundingClientRect()
    const dpr = window.devicePixelRatio || 1
    canvas.width = Math.max(1, Math.round(rect.width * dpr))
    canvas.height = Math.max(1, Math.round(rect.height * dpr))
    const ctx = canvas.getContext('2d')
    if (ctx) ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [videoEl, resizeTick])

  // filter/visible read inside the draw loop via refs, not closed-over
  // props, so `draw` (and the loop effect keyed on it) doesn't need to be
  // recreated on every filter tweak while still always seeing the latest.
  const filterRef = useRef(filter)
  useEffect(() => {
    filterRef.current = filter
  }, [filter])
  const visibleRef = useRef(visible)
  useEffect(() => {
    visibleRef.current = visible
  }, [visible])

  const draw = useCallback(
    (atS: number) => {
      const canvas = canvasRef.current
      const video = videoEl
      if (!canvas || !video) return
      const ctx = canvas.getContext('2d')
      if (!ctx) return
      const dpr = window.devicePixelRatio || 1
      const cssW = canvas.width / dpr
      const cssH = canvas.height / dpr
      ctx.clearRect(0, 0, cssW, cssH)

      if (!visibleRef.current || !trackIndex) return
      const filter = filterRef.current
      const { objects, plates, vehicles } = boxesAt(trackIndex, atS)

      const content = videoContentRect(video)
      const toPx = (bbox: [number, number, number, number]) => {
        const [x1, y1, x2, y2] = bbox
        return {
          x: content.x + x1 * content.w,
          y: content.y + y1 * content.h,
          w: (x2 - x1) * content.w,
          h: (y2 - y1) * content.h,
        }
      }

      ctx.font = '11px ui-sans-serif, system-ui, sans-serif'
      ctx.textBaseline = 'bottom'
      ctx.lineWidth = 1.5

      if (filter.objects) {
        for (const obj of objects as DetObject[]) {
          if (obj.confidence < filter.min_conf) continue
          if (filter.classes.length > 0 && !filter.classes.includes(obj.class)) continue
          const color = colorMap.get(CLASS_TOKEN[obj.class] ?? DEFAULT_TOKEN) ?? '#a3a3a3'
          const { x, y, w, h } = toPx(obj.bbox)
          ctx.strokeStyle = color
          ctx.strokeRect(x, y, w, h)
          const label = `${obj.class} ${Math.round(obj.confidence * 100)}%`
          const labelW = ctx.measureText(label).width + 6
          ctx.fillStyle = color
          ctx.globalAlpha = 0.85
          ctx.fillRect(x, Math.max(0, y - 14), labelW, 14)
          ctx.globalAlpha = 1
          ctx.fillStyle = '#0a0a0a'
          ctx.fillText(label, x + 3, Math.max(14, y))
        }
      }

      if (filter.plates) {
        for (const plate of plates as DetPlate[]) {
          if (plate.confidence < filter.min_conf) continue
          const { x, y, w, h } = toPx(plate.bbox)
          const plateColor = colorMap.get(PLATE_TOKEN) ?? '#a3a3a3'
          ctx.strokeStyle = plateColor
          ctx.strokeRect(x, y, w, h)
          const label = plate.text
          const labelW = ctx.measureText(label).width + 6
          ctx.fillStyle = plateColor
          ctx.globalAlpha = 0.85
          ctx.fillRect(x, y + h, labelW, 14)
          ctx.globalAlpha = 1
          ctx.fillStyle = '#0a0a0a'
          ctx.fillText(label, x + 3, y + h + 13)
        }
      }

      if (filter.vehicles) {
        for (const vehicle of vehicles as DetVehicle[]) {
          if (vehicle.type_confidence < filter.min_conf) continue
          const { x, y, w, h } = toPx(vehicle.bbox)
          const color = colorMap.get(VEHICLE_TOKEN) ?? '#a3a3a3'
          ctx.strokeStyle = color
          ctx.strokeRect(x, y, w, h)
          const label = vehicle.make ? `${vehicle.make} ${vehicle.type}` : vehicle.type
          const labelW = ctx.measureText(label).width + 6
          ctx.fillStyle = color
          ctx.globalAlpha = 0.85
          ctx.fillRect(x, y + h, labelW, 14)
          ctx.globalAlpha = 1
          ctx.fillStyle = '#0a0a0a'
          ctx.fillText(label, x + 3, y + h + 13)
        }
      }
    },
    [videoEl, trackIndex, colorMap],
  )

  // The draw loop itself: while `visible` and the video is actually
  // playing, keep re-drawing off the video's own frame clock. On
  // pause/seek/data-load, draw exactly once from `video.currentTime`
  // instead of looping (nothing to chase when the picture isn't moving).
  useEffect(() => {
    const video = videoEl
    if (!video) return
    let rafId: number | null = null
    let rvfcId: number | null = null
    let cancelled = false
    // Bumped on every stopLoop/startLoop so a loop callback scheduled by a
    // previous generation (e.g. a UA lacking cancelVideoFrameCallback,
    // where stopLoop can't actually cancel an in-flight rVFC) bails instead
    // of running alongside a newly-started chain.
    let generation = 0

    const hasRVFC = typeof video.requestVideoFrameCallback === 'function'

    function loopRVFC(gen: number, _now: number, metadata: VideoFrameCallbackMetadata) {
      if (cancelled || gen !== generation) return
      draw(metadata.mediaTime)
      if (visibleRef.current && !video!.paused && !video!.ended) {
        rvfcId = video!.requestVideoFrameCallback((now, metadata) => loopRVFC(gen, now, metadata))
      }
    }
    function loopRAF(gen: number) {
      if (cancelled || gen !== generation) return
      draw(video!.currentTime)
      if (visibleRef.current && !video!.paused && !video!.ended) {
        rafId = requestAnimationFrame(() => loopRAF(gen))
      }
    }
    function startLoop() {
      if (!visibleRef.current || video!.paused || video!.ended) return
      generation++
      const gen = generation
      if (hasRVFC) {
        rvfcId = video!.requestVideoFrameCallback((now, metadata) => loopRVFC(gen, now, metadata))
      } else {
        rafId = requestAnimationFrame(() => loopRAF(gen))
      }
    }
    function stopLoop() {
      generation++
      if (rvfcId != null && typeof video!.cancelVideoFrameCallback === 'function') {
        video!.cancelVideoFrameCallback(rvfcId)
      }
      rvfcId = null
      if (rafId != null) cancelAnimationFrame(rafId)
      rafId = null
    }
    function drawOnce() {
      draw(video!.currentTime)
    }

    const onPlay = () => {
      stopLoop()
      startLoop()
    }
    const onPause = () => {
      stopLoop()
      drawOnce()
    }
    const onEnded = () => {
      stopLoop()
      drawOnce()
    }
    const onSeeked = () => drawOnce()
    const onLoadedData = () => drawOnce()

    video.addEventListener('play', onPlay)
    video.addEventListener('pause', onPause)
    video.addEventListener('ended', onEnded)
    video.addEventListener('seeked', onSeeked)
    video.addEventListener('loadeddata', onLoadedData)

    startLoop()
    drawOnce()

    return () => {
      cancelled = true
      stopLoop()
      video.removeEventListener('play', onPlay)
      video.removeEventListener('pause', onPause)
      video.removeEventListener('ended', onEnded)
      video.removeEventListener('seeked', onSeeked)
      video.removeEventListener('loadeddata', onLoadedData)
    }
    // visible is read here (not just via visibleRef) so toggling the
    // overlay on during playback restarts the rVFC/rAF loop rather than
    // leaving it stopped from a prior hide.
  }, [videoEl, draw, visible])

  // While paused (the loop above isn't running), a resize/theme/data/filter
  // change still needs a single redraw so the frozen frame doesn't show a
  // stale box/color/scale. Also depends on `visible` so hiding the overlay
  // while paused clears the canvas instead of leaving the last drawn boxes.
  useEffect(() => {
    const video = videoEl
    if (!video) return
    if (video.paused || video.ended) draw(video.currentTime)
  }, [videoEl, draw, resizeTick, themeTick, filter, currentTimeS, visible])

  return (
    <canvas
      ref={canvasRef}
      className="pointer-events-none absolute inset-0 h-full w-full"
      aria-hidden="true"
    />
  )
}
