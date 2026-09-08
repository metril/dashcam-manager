// DetectionOverlay draws normalized-bbox detections (objects + plates) for
// the current playback position onto a canvas stacked over a <video>
// element, honoring the video's object-fit: contain letterboxing so boxes
// stay pinned to the actual picture rect rather than the element's box.
import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getSegmentDetections } from '../../lib/api'
import type { DetFrame } from '../../types'

const CLASS_COLORS: Record<string, string> = {
  car: '#38bdf8',
  truck: '#a78bfa',
  bus: '#f472b6',
  person: '#facc15',
  bicycle: '#4ade80',
  motorcycle: '#fb923c',
}
const DEFAULT_COLOR = '#e5e5e5'
const PLATE_COLOR = '#22d3ee'

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
}: {
  segmentId: number | null
  currentTimeS: number
  videoEl: HTMLVideoElement | null
  visible: boolean
}) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  // Bumped by a ResizeObserver whenever the video element's rendered box
  // changes (window resize, layout toggle between front/rear/both, etc.)
  // so the draw effect below re-runs and boxes rescale even while paused.
  const [resizeTick, setResizeTick] = useState(0)

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

    for (const obj of frame.objects) {
      const color = CLASS_COLORS[obj.class] ?? DEFAULT_COLOR
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

    for (const plate of frame.plates) {
      const { x, y, w, h } = toPx(plate.bbox)
      ctx.strokeStyle = PLATE_COLOR
      ctx.strokeRect(x, y, w, h)
      if (plate.confidence > 0.5) {
        const label = plate.text
        const labelW = ctx.measureText(label).width + 6
        ctx.fillStyle = PLATE_COLOR
        ctx.globalAlpha = 0.85
        ctx.fillRect(x, y + h, labelW, 14)
        ctx.globalAlpha = 1
        ctx.fillStyle = '#0a0a0a'
        ctx.fillText(label, x + 3, y + h + 13)
      }
    }
  }, [data, currentTimeS, visible, videoEl, resizeTick])

  return (
    <canvas
      ref={canvasRef}
      className="pointer-events-none absolute inset-0 h-full w-full"
      aria-hidden="true"
    />
  )
}
