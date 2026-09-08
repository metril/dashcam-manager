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
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
COPY --from=frontend /src/cmd/api/dist ./cmd/api/dist
RUN CGO_ENABLED=0 go build -o /out/dashcam-api ./cmd/api

# --- runtime ----------------------------------------------------------
# jellyfin/jellyfin is too heavy for an API-only container; instead install
# jellyfin-ffmpeg (has QSV/VAAPI support baked in) from the Jellyfin apt repo
# on top of a plain Debian base.
FROM debian:bookworm-slim AS runtime

# intel-media-va-driver-non-free lives in Debian's contrib/non-free(-firmware)
# components, which the base image doesn't enable by default — add them
# before installing it.
RUN sed -i 's/Components: main/Components: main contrib non-free non-free-firmware/' \
        /etc/apt/sources.list.d/debian.sources \
    && apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        tzdata \
        cifs-utils \
    && install -d -m 0755 /etc/apt/keyrings \
    && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg arch=amd64] https://repo.jellyfin.org/debian bookworm main" \
        > /etc/apt/sources.list.d/jellyfin.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        jellyfin-ffmpeg7 \
        intel-media-va-driver-non-free \
        libmfx-gen1.2 \
    && rm -rf /var/lib/apt/lists/* \
    && apt-get purge -y gnupg curl \
    && apt-get autoremove -y

ENV PATH="/usr/lib/jellyfin-ffmpeg:${PATH}"
ENV LIBVA_DRIVER_NAME=iHD

COPY --from=build /out/dashcam-api /usr/local/bin/dashcam-api

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dashcam-api"]
