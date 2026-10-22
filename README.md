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
- **Storage**: local directories and SMB/CIFS shares as first-class
  storages (see "Storage" below), with cross-storage duplicate detection
  and a Storage page.
- **Shares**: configure SMB/CIFS destinations, mount them (`mount.cifs`,
  encrypted stored credentials), and scan them like a second source.
- **Ingest & cleanup**: verified (sha256 or size) copy of SSD trips to a
  share, resumable/idempotent, with a dry-run preview before the SSD
  originals are ever deleted.
- **Export**: pick clip ranges across one or more segments (optionally
  spanning both cameras, with an in/out-point export bin built while
  scrubbing the player), stitch them with hardware-accelerated ffmpeg (NVENC or QSV)
  (`-c copy` fast path when no re-encode is needed, full re-encode with
  optional detection-box burn-in otherwise), and download or push the
  result to a share.
- **Hardware acceleration**: Intel Quick Sync (QSV), NVIDIA NVENC/NVDEC, a
  VAAPI fallback, and a software-encode fallback, auto-detected at boot
  (preferring NVENC > QSV > VAAPI > software) and re-checked per job on
  failure.

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

## Storage

Persistent storage comes in three kinds, all first-class rows in `shares`
(`kind = 'cifs' | 'local'`) plus the export cache:

- **`local`**: a directory under `DASHCAM_LOCAL_ROOT` (default
  `/mnt/shares`; `compose.yaml` already bind-mounts `./shares` there, so a
  local storage is just a subdirectory of that same tree — see the comment
  next to the bind).
- **`cifs`**: a mounted SMB/CIFS share, as before (`mount.cifs`, encrypted
  stored credentials).
- **Export cache**: `DASHCAM_CACHE`, a fixed pseudo-storage (no id) for
  exports pushed there instead of a share.

**The recorder's USB SSD is never a storage.** It's an ingest source only —
never listed on the Storage page, never a member of a duplicate group, and
never an export target. A segment still on the SSD with no persistent copy
shows "not ingested"; once ingested, its copies (not the SSD original) are
what's tracked.

**Duplicate detection** combines existing copy records (segments, ingests,
pulls, exports) with a background `storage_files` index built while
scanning `local`/`cifs` storages, unioned as the `file_copies` view. A group
of ≥2 copies on distinct storages is **verified** (sha256 hashes equal —
confirmed identical content) or **probable** (size + filename equal, but at
least one copy still unhashed). Hashing runs as a throttled background
job (`hash_backfill`, auto-enqueued after each scan, also triggerable from
the Storage page); the `hash_throttle_mb_s` setting caps its read rate
(default 40 MB/s, `0` = unthrottled) so it doesn't starve foreground I/O.

The **Storage page** (`/system/storage`) lists every storage with usage,
lets you add/edit `local` directories, and trigger scans/hashing; its
Duplicates section shows Verified/Probable groups with expandable members
and the total reclaimable space across the whole filtered set (not just the
current page). See `GET /api/storages` and `GET /api/duplicates` in the API
summary below.

**Upgrading to schema v9**: back up `/data/dashcam.db` before your first
`docker compose up` on this version. The migration removes the hidden
`allow_local_share_paths` setting; any share that was created under it
(the only way to point a share at a local directory before `kind=local`
existed) is automatically flipped to `kind = 'local'`.

## Releases

Every push to `main` (outside docs/markdown changes) cuts a release via
GitHub Actions. The version comes from
[conventional commits](https://www.conventionalcommits.org/): `feat:` bumps
minor, `fix:` and anything else bumps patch, `!`/`BREAKING CHANGE` bumps major.
The image is published to `ghcr.io/metril/dashcam-manager` (tags `X.Y.Z`,
`X.Y`, `latest`, short sha). `compose.yaml` pulls `:latest` by default:

```sh
docker compose -f compose.yaml -f compose.intel.yaml pull
docker compose -f compose.yaml -f compose.intel.yaml up -d
```

Add `--build` to `up` to build locally instead.

## Development

```sh
# Backend
go build ./... && go test ./...

# Frontend (outputs into cmd/api/dist, embedded by the Go binary)
cd app/frontend && npm install && npm run build

# Container. The base compose.yaml has NO GPU access (software ffmpeg only);
# always layer the override for your GPU vendor, or both on a dual-GPU host.
# Intel QSV/VAAPI:
docker compose -f compose.yaml -f compose.intel.yaml up -d --build
# NVIDIA NVENC/NVDEC (needs nvidia-container-toolkit, see Host prep):
docker compose -f compose.yaml -f compose.nvidia.yaml up -d --build
curl localhost:8080/api/health
docker compose -f compose.yaml -f compose.intel.yaml down

# Tip: put the layering in .env so plain `docker compose up -d` keeps GPU access:
#   COMPOSE_FILE=compose.yaml:compose.intel.yaml
```

Copy `.env.example` to `.env` to override defaults (SSD path, DB/cache
locations, QSV/CUDA device, render GID, worker count, secret key).

### Host prep

Before the first `docker compose up`, a few one-time host setup steps:

- **Render GID** (Intel only, `compose.intel.yaml`): find it with
  `getent group render` and set `RENDER_GID` in `.env` accordingly
  (defaults to `110`) — this lets the container's user access `/dev/dri`
  for QSV/VAAPI.
- **NVIDIA Container Toolkit** (NVIDIA only, `compose.nvidia.yaml`): install
  [nvidia-container-toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
  on the host, then run `sudo nvidia-ctk runtime configure --runtime=docker`
  and restart Docker. jellyfin-ffmpeg (already in the image) dlopens the
  host's CUDA/NVENC libs at runtime, injected by the toolkit — no image
  rebuild needed on the NVIDIA side. If `docker compose up` fails with
  `could not select device driver "nvidia" with capabilities: [[gpu video]]`,
  the toolkit isn't configured for Docker — re-run the `nvidia-ctk`
  command above.
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
- **SSD mount**: the SSD is mounted read-write, entirely from *inside* the
  container — see "USB SSD hotplug" below. No host `fstab`/mount setup is
  needed at all. Safety comes from the app itself: the "clean up verified
  originals from SSD" job always does a dry-run pass and requires explicit
  confirmation before deleting anything.

### USB SSD hotplug

The recorder's USB SSD (exfat) is mounted **in-container** by
`internal/ssd`, which inotify-watches `/dev/disk/by-uuid` (bind-mounted
**read-only** into the container via `compose.yaml`'s `/dev/disk` bind —
not all of `/dev`) for the SSD's filesystem UUID appearing or
disappearing, and mounts/unmounts `DASHCAM_SSD_PATH` (default
`/mnt/dashcam`) accordingly — no host-side `fstab` entry, mount unit, or
setup script required.

Why inotify on a `/dev/disk` bind rather than the old host-fstab +
`rslave` `/mnt` bind: kernel uevents (`NETLINK_KOBJECT_UEVENT`, what a
normal hotplug listener like `udisks`/`udev` rules relies on) are
network-namespace scoped, so a Compose container never receives them
without host networking. A `/dev/disk` bind sidesteps that entirely —
host udev still creates `/dev/disk/by-uuid/<UUID>` on devtmpfs as before,
and since a bind mount just shows the same inodes live, inotify on the
bind-mounted directory fires on create/delete in the container in exact
lockstep with the host, without polling.

Only `/dev/disk` is bound (read-only), not all of `/dev` — the container
never gets a host device node handed to it directly. The by-uuid
symlink's target (e.g. `../../sdb1`, i.e. host `/dev/sdb1`) doesn't exist
in the container, since `/dev` itself isn't bound; `internal/ssd` instead
reads that target's basename, looks up its major:minor from sysfs
(unaffected by the narrower bind), and creates a **private** block device
node under the container's own `/dev` (`/dev/dashcam-ssd`) from that
pair — the same device the host would mount, without exposing any other
device node. This needs `CAP_MKNOD` (already part of Docker's default
capability set) and `device_cgroup_rules: ["b 8:* rmw"]` (already in
`compose.yaml` — major 8 covers the SCSI/USB-attached-disk devices a USB
SSD enumerates as).

Setup: find the SSD's filesystem UUID and set it as `DASHCAM_SSD_UUID` in
`.env` (or the environment), then restart the container:

```sh
lsblk -f  # find the UUID of the exfat partition, e.g. F9B1-23CB
echo "DASHCAM_SSD_UUID=F9B1-23CB" >> .env
docker compose -f compose.yaml -f compose.intel.yaml up -d --force-recreate app
```

Migrating from the old host-mounted setup: the host no longer needs to
mount the drive. Unmount it once and drop any leftover `/etc/fstab` line so
the container is the only mounter:

```sh
sudo umount /mnt/dashcam
sudo sed -i '/\/mnt\/dashcam/d' /etc/fstab   # only if a line exists
```

(Both mounted at once is still safe, since the kernel shares one superblock
per block device, but the host mount is redundant.)

Leaving `DASHCAM_SSD_UUID` unset disables the watcher entirely (legacy
behaviour) — `DASHCAM_SSD_PATH` is then expected to already be mounted by
whatever put it there. `DASHCAM_SSD_UUID` is also editable from
**Settings**, stored the same way as other settings, but (like hardware
mode / worker count) only read once at container startup.

To verify hotplug after unplugging/replugging the drive:

```sh
docker exec dashcam-manager-app-1 findmnt /mnt/dashcam       # container's mount
curl -s localhost:8090/api/ssd/status                        # app's view (mount_source/last_event)
```

Use **Ingest**'s **Eject SSD** button (or `POST /api/ingest/eject`) before
physically unplugging the drive: it does a clean unmount, refusing with
409 if a scan job is currently running against the SSD source, otherwise
falling back to a lazy (detach) unmount if something else still has it
open.

### First run

1. `docker compose -f compose.yaml -f compose.intel.yaml up -d` (or the
   nvidia override), then open `http://localhost:8080` (or whatever
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
| `GET /storages`, `POST/PATCH/DELETE /storages{,/{id}}`, `POST /storages/{id}/scan`, `POST /storages/hash` | local + CIFS storages (unified `shares` view), scan/hash triggers |
| `GET /duplicates`, `POST /duplicates/refresh` | cross-storage duplicate report (verified/probable groups + totals) |
| `GET /calendar`, `GET /trips`, `GET /trips/{id}`, `GET /trips/{id}/timeline` | browse |
| `GET /segments/{id}`, `GET /segments/{id}/detections`, `GET /segments/{id}/media` | segment metadata, detections, streamed video (Range) |
| `GET /segments/{id}/thumb.jpg`, `/sprite.jpg`, `/sprite.vtt` | thumbnails & hover-scrub sprite |
| `GET /search` | full-text + filtered detection search, grouped by segment |
| `GET /gps/track?trip_id=` | GeoJSON LineString + per-point speed for the trip map |
| `GET /stats` | library-wide + per-source stats |
| `GET /ssd/status`, `POST /ingest`, `POST /ingest/cleanup`, `POST /ingest/eject` | SSD status, verified ingest to a share, safe cleanup, safe unmount before unplugging |
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
| `DASHCAM_SSD_UUID` | *(empty)* | SSD filesystem UUID (from `/dev/disk/by-uuid`); enables in-container hotplug mount — see "USB SSD hotplug" |
| `DASHCAM_QSV_DEVICE` | `/dev/dri/renderD128` | DRI render node for Quick Sync — only used with `compose.intel.yaml` |
| `DASHCAM_CUDA_DEVICE` | `0` | CUDA device index for NVENC/NVDEC — only used with `compose.nvidia.yaml` |
| `DASHCAM_SECRET_KEY` | *(empty)* | encrypts stored share credentials — set before configuring shares |
| `DASHCAM_LOCAL_ROOT` | `/mnt/shares` | directory `kind=local` storage paths must live under — see "Storage" |
| `DASHCAM_WORKERS` | `4` | background job worker concurrency |
| `RENDER_GID` | `110` | host `render` group GID, for `/dev/dri` access in the container — only used with `compose.intel.yaml` |
| `COMPOSE_FILE` | *(unset)* | e.g. `compose.yaml:compose.intel.yaml` so a plain `docker compose up -d` keeps GPU access; use `compose.nvidia.yaml` (or both, colon-separated) on NVIDIA / dual-GPU hosts |

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
- **No audio**: verified with `ffprobe` against real segments (both
  cameras) on the recorder's own SSD — every `segment_NNNNN.mp4` has
  exactly one stream (HEVC video), no audio track at all. Export's fast
  concat-copy path (`-c copy`) and re-encode path (`-an`) both correctly
  produce audio-less output as a result; there's nothing to preserve. If a
  future recorder firmware starts writing an audio stream, both paths
  (`internal/media/stitch.go`'s `ConcatCopyArgs`/`ReencodeArgs`) will need
  updating to keep it (`-c:a copy` for the fast path, `-c:a aac` for
  re-encode, since burn-in's filter_complex re-encode can't stream-copy
  audio through a video-only filter graph).

## Known limitations

- **The `q` + `class` search fast path matches `q` anywhere in the
  segment, not necessarily on the same detection as the class match.**
  `GET /api/search?q=...&class=...` finds segments that have *some*
  detection matching the class filter and *some* (possibly different)
  detection whose text matched `q` — it does not require the two to be the
  same detection. This trades a small amount of precision (an occasional
  segment where the "hit" shown for the class filter isn't the specific
  vehicle `q` matched) for the query being fast at all — see
  `internal/db/queries_search.go`'s `searchQAndClassFastPath` doc comment
  for the multiple correlated-EXISTS approaches that *were* precise but
  took 30-56s against a real-sized library. Every other filter combination
  (`class`+`make`, `class`+`plate`, ...) remains fully precise (same-
  detection) since they don't hit this fast path.
- **An archived SSD segment and its share-side copy appear twice** in
  browse/trip listings and Stats until the SSD source itself is removed:
  the archived row (SSD source, `archived_at` set, no playable file — see
  the "Database schema resets" section's `archived_at` reference) and the
  share source's row for the ingested copy (a separate `sources` row,
  since a share is scanned as its own source) both reference the same
  underlying footage but are two distinct `segments` rows with no link
  between them. Deduplicating them would need either merging two sources'
  worth of rows at query time or recording provenance between an archived
  row and the share-source row that superseded it — neither is done; this
  is a browse/trip-listing display quirk, not a data-loss or playback-
  correctness issue. **Fixed for Stats** (local-storage plan task 10):
  `GET /api/stats`' `bytes` is now logical — an archived SSD segment and
  its persistent copy count once, and `bytes_physical`/`by_source` still
  expose the old per-row physical total for comparison.

## Roadmap / deferred

Per-share retention policies, "interesting moments" auto-flagging (hard
braking, person detected, high-confidence plate), saved searches, export
presets, Prometheus metrics, per-job GPU selection on dual-GPU hosts (today
one `hw_mode` applies to every job; the second Intel node
`/dev/dri/renderD129` is unused).
