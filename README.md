# dashcam-manager

Self-hosted ingest, archive, browse, search, and GPU-accelerated stitched
export tool for 4K HEVC dashcam footage (front/rear) recorded by an Orange Pi
5 Plus recorder onto a USB SSD.

See `docs/superpowers/specs/2026-09-08-dashcam-manager-design.md` for the full
design and milestone plan.

## Features

- **Scan & index**: walks the SSD (or a mounted SMB/CIFS share) for
  `{front,rear}/YYYY/MM/DD/recording_*/segment_*.mp4` files plus their
  `.timestamp.json` / `.detections.json` / `.gps.json` sidecars, pairs
  front/rear recordings into trips (via filename timestamps and a
  cross-correlated camera offset), and explodes per-frame object/plate/
  vehicle detections into queryable tables.
- **Browse**: a calendar of days with trips/segments/bytes/object counts,
  trip cards with cover thumbnails, and a trip detail page with a
  synced dual-camera player (detection-box overlay, hover-scrub sprite
  thumbnails on the segment strip, keyboard-driven playback) and a GPS
  track map.
- **Search**: full-text (trigram) search over plate reads, vehicle makes/
  types, and object classes, with date/confidence/speed filters, grouped
  by segment with clickable hits that deep-link into the player at the
  matching frame.
- **Stats**: storage per source/share, bytes-by-month, segments-by-camera,
  object-class histogram, and SSD→share ingest coverage.
- **Jobs**: a persistent, worker-pool job queue (scan, detection indexing,
  thumbnails, ingest, cleanup, export) with live progress over SSE and a
  Jobs page.
- **Shares**: configure SMB/CIFS destinations, mount them (`mount.cifs`,
  encrypted stored credentials), and scan them like a second source.
- **Ingest & cleanup**: verified (sha256 or size) copy of SSD trips to a
  share, resumable/idempotent, with a dry-run preview before the SSD
  originals are ever deleted.
- **Export**: pick clip ranges across one or more segments (optionally
  spanning both cameras, with an in/out-point export bin built while
  scrubbing the player), stitch them with QSV-accelerated ffmpeg
  (`-c copy` fast path when no re-encode is needed, full re-encode with
  optional detection-box burn-in otherwise), and download or push the
  result to a share.
- **Hardware acceleration**: Intel Quick Sync (QSV) with a VAAPI and
  software-encode fallback, auto-detected at boot and re-checked per job
  on failure.

## Architecture

- **Backend**: Go (`cmd/api`), stdlib `net/http` mux (Go 1.22+ pattern
  routing), `modernc.org/sqlite` (pure-Go, no cgo) with WAL journaling and
  a single-writer/many-reader connection split (`internal/db`). The React
  SPA build is embedded into the binary via `//go:embed` and served
  alongside the JSON API from one process/port.
- **Scanning & indexing**: `internal/scan` walks the filesystem, parses
  sidecars, and writes segment/trip rows; `scan.IndexDetections` explodes
  a segment's raw detections JSON into `detections`/`det_objects`/
  `det_plates`/`det_vehicles` rows plus `search_terms` (kept in sync with
  an FTS5 trigram `search_fts` index via triggers) — this is deferred out
  of the initial scan and run as a background job so a full SSD scan stays
  fast.
- **Jobs**: `internal/jobs` is a small persistent queue (jobs table +
  worker pool) driving scan, detection indexing, thumbnail generation,
  ingest, cleanup, and export; progress streams to the frontend over
  Server-Sent Events (`GET /api/events`).
- **Media**: `internal/media` wraps ffmpeg/ffprobe for thumbnails, hover
  sprites (WebVTT + tile sheet), hardware-accel probing, and export
  stitching/burn-in (`internal/media/stitch.go`, `overlay.go`).
- **Shares & ingest**: `internal/share` manages SMB/CIFS mounts;
  `internal/ingest` copies+verifies SSD segments to a mounted share and
  safely deletes verified SSD originals.
- **Frontend**: React 18 + Vite + TypeScript + Tailwind + TanStack Query
  v5, in `app/frontend`, built to `cmd/api/dist`.

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

Before the first `docker compose up`, a few one-time host setup steps:

- **Render GID**: find it with `getent group render` and set `RENDER_GID` in
  `.env` accordingly (defaults to `110`) — this lets the container's user
  access `/dev/dri` for QSV/VAAPI.
- **Shares bind-mount propagation**: `compose.yaml` bind-mounts `./shares` at
  `/mnt/shares` with `rshared` propagation, so that `mount.cifs` calls made
  inside the container become visible on the host, and vice versa. A plain
  host directory doesn't have shared propagation by default, so run once
  (and again after every host reboot, unless added to `/etc/fstab`):
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
- **SSD mount for cleanup**: `compose.yaml` bind-mounts the SSD read-only
  (`:ro`) by default, since scanning/browsing/ingesting never need to write
  to it. The "clean up verified originals from SSD" job does need to
  delete files, so before using it, change the SSD mount to read-write in
  `compose.yaml` (drop the `:ro` suffix on the SSD volume line) or via an
  override compose file, then `docker compose up -d` to apply. Leave it
  `:ro` the rest of the time — cleanup refuses to run (with a clear error)
  against a mount it can't write-probe.

### First run

1. `docker compose up -d`, then open `http://localhost:8080` (or whatever
   `DASHCAM_ADDR_PORT` is set to).
2. The SSD source (`DASHCAM_SSD_PATH`) is seeded automatically at boot.
   From **Jobs** (or by hitting `POST /api/sources/scan`), kick off a scan
   — it walks the SSD, upserts segments/trips, and auto-enqueues a
   lower-priority `index_detections` job (explodes per-frame detections
   for search) and then a `thumbs` job (cover/segment thumbnails).
   **The first full `index_detections` run over a large SSD (~39k
   segments) takes roughly an hour** (it parses and writes every frame's
   object/plate/vehicle detections) and grows the database from a few
   hundred MB to several GB — this is expected and only happens once per
   backlog; subsequent scans only index newly-added segments. Search and
   Stats are usable incrementally as the job progresses (segments already
   indexed become searchable right away), and this deferred-indexing
   design (see `internal/scan.Scan` vs `scan.IndexDetections`) is exactly
   why: keeping it out of the main scan is what keeps a fresh SSD scan
   itself fast.
3. Optionally add a share under **Shares** (server/share/credentials),
   mount it, and scan it the same way — useful as an ingest destination
   and as a second browsable source.
4. Once detections are indexed, **Search** and the trip **Map** become
   useful; **Stats** fills in as scanning/indexing progress.
5. Use **Ingest** to copy verified trips from the SSD to a share, and
   (once the SSD mount is `rw` — see above) **clean up** verified
   originals to free SSD space for the recorder to keep recording.

## Keyboard shortcuts

**Player** (trip detail page, while a video has focus):

| Key | Action |
| --- | --- |
| `Space` | play / pause |
| `J` / `L` | rewind / fast-forward, tap again to increase speed |
| `K` | pause and reset speed to 1x |
| `,` / `.` | step one frame back / forward |
| `←` / `→` | seek 5s back / forward |
| `Shift` + `←` / `→` | jump to previous / next segment |
| `1` / `2` / `3` | front / rear / both-camera layout |
| `D` | toggle detection-box overlay |
| `F` | toggle fullscreen |
| `I` / `O` | mark export-bin in-point / out-point at current time |
| `S` | add the current front segment to the export bin |
| `Escape` | clear a pending (unfinished) export-bin range |

**Browse** (calendar):

| Key | Action |
| --- | --- |
| `←` / `→` | previous / next month |

**Search**:

| Key | Action |
| --- | --- |
| `/` | focus the search box from anywhere on the page |

## API summary

All endpoints are under `/api`; the SPA is served from `/`.

| Method & path | Purpose |
| --- | --- |
| `GET /health` | liveness |
| `GET/PUT /settings` | app settings (hw mode, worker count, ...) |
| `GET/POST /sources`, `POST /sources/scan` | scan sources (SSD, shares) |
| `GET/POST/PATCH/DELETE /shares`, `POST /shares/{id}/test\|mount\|unmount` | SMB/CIFS share config |
| `GET /calendar`, `GET /trips`, `GET /trips/{id}`, `GET /trips/{id}/timeline` | browse |
| `GET /segments/{id}`, `GET /segments/{id}/detections`, `GET /segments/{id}/media` | segment metadata, detections, streamed video (Range) |
| `GET /segments/{id}/thumb.jpg`, `/sprite.jpg`, `/sprite.vtt` | thumbnails & hover-scrub sprite |
| `GET /search` | full-text + filtered detection search, grouped by segment |
| `GET /gps/track?trip_id=` | GeoJSON LineString + per-point speed for the trip map |
| `GET /stats` | library-wide + per-source stats |
| `GET /ssd/status`, `POST /ingest`, `POST /ingest/cleanup` | SSD status, verified ingest to a share, safe cleanup |
| `GET /jobs`, `GET /jobs/{id}`, `POST /jobs/{id}/cancel`, `DELETE /jobs/{id}` | job queue |
| `GET /events` | Server-Sent Events stream (job progress) |
| `POST /export`, `GET /exports`, `GET /exports/{id}/download`, `DELETE /exports/{id}` | stitched clip export |

## Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `DASHCAM_ADDR_PORT` | `8080` | host port published by `docker compose` |
| `DASHCAM_DB` | `/data/dashcam.db` | sqlite database path (inside the container) |
| `DASHCAM_CACHE` | `/cache` | thumbnails/sprites/export cache directory |
| `DASHCAM_SSD_PATH` | `/mnt/dashcam` | SSD mount point |
| `DASHCAM_QSV_DEVICE` | `/dev/dri/renderD128` | DRI render node for Quick Sync |
| `DASHCAM_SECRET_KEY` | *(empty)* | encrypts stored share credentials — set before configuring shares |
| `DASHCAM_WORKERS` | `4` | background job worker concurrency |
| `RENDER_GID` | `110` | host `render` group GID, for `/dev/dri` access in the container |

## Security

dashcam-manager has **no authentication** — it's designed to be LAN-trusted,
the same way you'd trust a NAS's web UI on your home network. Every `/api`
endpoint is reachable by anyone who can reach the port. It does refuse the
easy drive-by CSRF vector: every mutating request (`POST`/`PUT`/`PATCH`/
`DELETE`) must carry `Content-Type: application/json` and, if it sends an
`Origin` header at all, that origin must match the request's own `Host` (see
`cmd/api/csrf.go`) — but that's not a substitute for real access control.

**Put it behind a reverse proxy with its own authentication (Caddy/nginx with
basic auth, an OAuth2 proxy, Tailscale/Tailscale Serve with ACLs, ...) for
anything beyond a home LAN** — exposing it directly to the internet, a
shared network, or anyone you don't trust with full read/write/delete access
to your dashcam footage index is not supported.

The container also runs as root (see `compose.yaml`/`Dockerfile`) rather
than a dedicated non-root user: it needs write access to whatever UID/GID
owns the bind-mounted cache/db volumes and `/dev/dri` (varies per host,
notably across the `RENDER_GID` value above) plus `mount.cifs`'s typical
requirement for `CAP_SYS_ADMIN`, and getting all three right for an
arbitrary host under a fixed non-root UID reliably was judged not worth the
complexity for a single-tenant, LAN-trusted home tool. If you run it
somewhere more hostile than a home LAN, treat root-in-container as one more
reason to put it behind a reverse proxy / network boundary rather than
exposing the port directly.

## Database schema resets

`schema_version` (stored in `settings`) is baselined at `3` — see
`internal/db/schema.go`. A database stamped with a version *older* than the
baseline (there shouldn't be any in the wild; this only matters for a
pre-release dev/test database) fails to boot with a clear error rather than
being silently upgraded, since there was never migration DDL written to
carry it forward safely. The fix is simple: **delete the database file and
re-scan** — the SSD/shares it was scanned from are read-only and are never
the ones losing data, so nothing is actually lost, just re-derived. A
version *newer* than the binary's `SchemaVersion` is refused the same way
(upgrade the binary instead).

## Notes

- The frontend is built against **React 19** even though
  `docs/superpowers/specs/2026-09-08-dashcam-manager-design.md` was written
  assuming React 18 — 19 was current and a strictly better default by the
  time the frontend milestones landed, and nothing in the spec depends on
  an 18-specific API.

## Roadmap / deferred

Per-share retention policies, "interesting moments" auto-flagging (hard
braking, person detected, high-confidence plate), saved searches, export
presets, Prometheus metrics, VAAPI fallback on the second GPU
(`/dev/dri/renderD129`) if QSV underperforms.
