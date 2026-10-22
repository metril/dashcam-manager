# syntax=docker/dockerfile:1

# --- frontend build -----------------------------------------------------
FROM node:24 AS frontend
WORKDIR /src/app/frontend
COPY app/frontend/package.json app/frontend/package-lock.json ./
RUN npm ci
COPY app/frontend/ ./
RUN npm run build

# --- go build -------------------------------------------------------------
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/cmd/api/dist ./cmd/api/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/dashcam-api ./cmd/api

# --- runtime ----------------------------------------------------------
# jellyfin/jellyfin is too heavy for an API-only container; instead install
# jellyfin-ffmpeg (has QSV/VAAPI/NVENC support baked in) from the Jellyfin apt
# repo on top of a plain Debian base. Vendor-specific driver bits live in the
# final `intel` / `nvidia` stages; `intel` is last so a plain `docker build`
# produces it, and `--target nvidia` selects the NVIDIA variant.
FROM debian:bookworm-slim AS runtime-base

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        tzdata \
        cifs-utils \
        util-linux \
        exfatprogs \
    && install -d -m 0755 /etc/apt/keyrings \
    && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg arch=amd64] https://repo.jellyfin.org/debian bookworm main" \
        > /etc/apt/sources.list.d/jellyfin.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        jellyfin-ffmpeg7 \
    && apt-get purge -y gnupg \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/usr/lib/jellyfin-ffmpeg:${PATH}"

COPY --from=build /out/dashcam-api /usr/local/bin/dashcam-api

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/api/health || exit 1
ENTRYPOINT ["/usr/local/bin/dashcam-api"]

# --- nvidia variant -----------------------------------------------------
# The host's CUDA/NVENC libs are injected at runtime by the NVIDIA Container
# Toolkit; "video" must be in the capability set for NVENC/NVDEC.
FROM runtime-base AS nvidia
ENV NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility

# --- intel variant (default: must stay the last stage) --------------------
FROM runtime-base AS intel

# intel-media-va-driver-non-free lives in Debian's contrib/non-free(-firmware)
# components, which the base image doesn't enable by default — add them
# before installing it.
RUN sed -i 's/Components: main/Components: main contrib non-free non-free-firmware/' \
        /etc/apt/sources.list.d/debian.sources \
    && apt-get update && apt-get install -y --no-install-recommends \
        intel-media-va-driver-non-free \
        libmfx-gen1.2 \
    && rm -rf /var/lib/apt/lists/*

ENV LIBVA_DRIVER_NAME=iHD
