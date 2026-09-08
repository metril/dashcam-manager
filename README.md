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

### Host prep

Before the first `docker compose up`, two one-time host setup steps:

- **Render GID**: find it with `getent group render` and set `RENDER_GID` in
  `.env` accordingly (defaults to `110`) — this lets the container's user
  access `/dev/dri` for QSV/VAAPI.
- **Shares bind-mount propagation**: `compose.yaml` bind-mounts `./shares` at
  `/mnt/shares` with `rshared` propagation, so that `mount.cifs` calls made
  inside the container (M9) become visible on the host, and vice versa. A
  plain host directory doesn't have shared propagation by default, so run
  once (and again after every host reboot, unless added to `/etc/fstab`):
  ```sh
  sudo mount --bind ./shares ./shares
  sudo mount --make-rshared ./shares
  ```
- **CIFS mount capabilities**: `mount.cifs` inside the container needs both
  `cap_add: [SYS_ADMIN, DAC_READ_SEARCH]` (already set in `compose.yaml`) —
  `SYS_ADMIN` alone is not enough; without `DAC_READ_SEARCH` too,
  `mount.cifs` fails early with "Unable to apply new capability set." before
  it ever reaches the network, which looks like a permissions/seccomp
  problem but is actually just a missing capability. No host action needed
  here beyond keeping both entries in `compose.yaml`.

## Roadmap / deferred

Per-share retention policies, "interesting moments" auto-flagging (hard
braking, person detected, high-confidence plate), saved searches, export
presets, Prometheus metrics, VAAPI fallback on the second GPU
(`/dev/dri/renderD129`) if QSV underperforms.
