# dashcam-manager

Self-hosted ingest, archive, browse, search, and GPU-accelerated stitched
export tool for 4K HEVC dashcam footage (front/rear) recorded by an Orange Pi
5 Plus recorder onto a USB SSD.

See `docs/superpowers/specs/2026-09-08-dashcam-manager-design.md` for the full
design and milestone plan.

## Status

M1 (project skeleton) is in place: a Go API with `/api/health`, an embedded
React SPA build, and a QSV-ready Docker image. Everything past that
(scanning, browsing, playback, jobs, thumbnails, shares, ingest, export,
search) is not yet implemented — see the design doc's milestone list.

## Development

```sh
# Backend
go build ./... && go test ./...

# Frontend (outputs into cmd/api/dist, embedded by the Go binary)
cd app/frontend && npm install && npm run build

# Container (QSV-capable ffmpeg via jellyfin-ffmpeg)
docker compose build
docker compose up -d
curl localhost:8080/api/health
docker compose down
```

Copy `.env.example` to `.env` to override defaults (SSD path, DB/cache
locations, QSV device, render GID, worker count, secret key).

## Roadmap / deferred

Per-share retention policies, "interesting moments" auto-flagging (hard
braking, person detected, high-confidence plate), saved searches, export
presets, Prometheus metrics, VAAPI fallback on the second GPU
(`/dev/dri/renderD129`) if QSV underperforms.
