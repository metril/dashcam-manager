// DetectionOverlay draws normalized-bbox detections (objects + plates) for
// the current playback position onto a canvas stacked over a <video>
// element, honoring the video's object-fit: contain letterboxing so boxes
// stay pinned to the actual picture rect rather than the element's box.
import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { defaultDetectionFilter, getSegmentDetections, type DetectionFilter } from '../../lib/api'
import { THEME_CHANGE_EVENT } from '../../lib/theme'
import type { DetFrame } from '../../types'

// Object classes bucket into the det-* CSS custom properties defined in
// index.css (vehicle/person/other; sign/plate exist for future classes and
// the plate overlay below). Colors are read live via getComputedStyle
// rather than hardcoded hex so the canvas tracks the active theme —
// index.css's [data-theme="light"] override changes these vars, and a
// canvas draw call can't pick that up from a CSS class the way DOM
// elements do.
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

// nearestFrameWithin100ms returns the DetFrame whose pts_offset is closest
// to atS, or null if every frame is more than 0.1s away (or there are no
// frames) — we'd rather show nothing than a stale box from a distant frame.
function nearestFrameWithin100ms(frames: DetFrame[], atS: number): DetFrame | null {
  let best: DetFrame | null = null
  let bestDelta = Infinity
  for (const f of frames) {
    const delta = Math.abs(f.pts_offset - atS)
    if (delta < bestDelta) {
      best = f
      bestDelta = delta
    }
  }
  if (best == null || bestDelta > 0.1) return null
  return best
}

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
  currentTimeS: number
  videoEl: HTMLVideoElement | null
  visible: boolean
  /** Which detection boxes to draw — defaults to showing everything above the default confidence floor. */
  filter?: DetectionFilter
}) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  // Bumped by a ResizeObserver whenever the video element's rendered box
  // changes (window resize, layout toggle between front/rear/both, etc.)
  // so the draw effect below re-runs and boxes rescale even while paused.
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

  const { data } = useQuery({
    queryKey: ['segment-detections', segmentId],
    queryFn: () => getSegmentDetections(segmentId!),
    enabled: segmentId != null && visible,
    staleTime: Infinity,
  })

  useEffect(() => {
    const video = videoEl
    if (!video) return
    const ro = new ResizeObserver(() => setResizeTick((t) => t + 1))
    ro.observe(video)
    return () => ro.disconnect()
  }, [videoEl])

  useEffect(() => {
    const canvas = canvasRef.current
    const video = videoEl
    if (!canvas || !video) return

    const rect = video.getBoundingClientRect()
    const dpr = window.devicePixelRatio || 1
    canvas.width = Math.max(1, Math.round(rect.width * dpr))
    canvas.height = Math.max(1, Math.round(rect.height * dpr))
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    ctx.clearRect(0, 0, rect.width, rect.height)

    if (!visible || !data) return
    const frame = nearestFrameWithin100ms(data.detections, currentTimeS)
    if (!frame) return

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
      for (const obj of frame.objects) {
        if (obj.confidence < filter.min_conf) continue
        if (filter.classes.length > 0 && !filter.classes.includes(obj.class)) continue
        const color = resolveColor(CLASS_TOKEN[obj.class] ?? DEFAULT_TOKEN)
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
      for (const plate of frame.plates) {
        if (plate.confidence < filter.min_conf) continue
        const { x, y, w, h } = toPx(plate.bbox)
        ctx.strokeStyle = resolveColor(PLATE_TOKEN)
        ctx.strokeRect(x, y, w, h)
        const label = plate.text
        const labelW = ctx.measureText(label).width + 6
        ctx.fillStyle = resolveColor(PLATE_TOKEN)
        ctx.globalAlpha = 0.85
        ctx.fillRect(x, y + h, labelW, 14)
        ctx.globalAlpha = 1
        ctx.fillStyle = '#0a0a0a'
        ctx.fillText(label, x + 3, y + h + 13)
      }
    }

    if (filter.vehicles) {
      for (const vehicle of frame.vehicles) {
        if (vehicle.type_confidence < filter.min_conf) continue
        const { x, y, w, h } = toPx(vehicle.bbox)
        const color = resolveColor(VEHICLE_TOKEN)
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
  }, [data, currentTimeS, visible, videoEl, resizeTick, themeTick, filter])

  return (
    <canvas
      ref={canvasRef}
      className="pointer-events-none absolute inset-0 h-full w-full"
      aria-hidden="true"
    />
  )
}
