#!/usr/bin/env bash
# One-time host setup for USB SSD hotplug support.
#
# Why fstab alone gives hotplug: an fstab entry with `nofail` is picked up by
# systemd's fstab generator, which synthesizes a mount unit (e.g.
# mnt-dashcam.mount) AND its backing device unit. The mount unit gets
# `BindsTo=` the device unit, and the device unit gets a `Wants=` the mount
# unit once the device appears. So: plugging the SSD in makes udev announce
# the device unit, which (via Wants=) starts the mount unit automatically;
# unplugging it stops the device unit, which (via BindsTo=) tears the mount
# unit down automatically too. No udev rules or scripts need to run on every
# plug/unplug — systemd does it from the static fstab config alone.
#
# The container side then just needs to see that mount appear/disappear
# live, which is what the rslave bind of /mnt in compose.yaml provides.
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "run with sudo" >&2
  exit 1
fi

UUID="F9B1-23CB"
MNT="/mnt/dashcam"
UID_VAL="${SUDO_UID:-1000}"
GID_VAL="${SUDO_GID:-1000}"

# /mnt itself must stay a plain directory (not a mountpoint), since compose
# bind-mounts /mnt with rslave propagation and expects /mnt/dashcam to be a
# child mount underneath it, not a mount masking /mnt entirely.
if [[ -n "$(findmnt -M /mnt 2>/dev/null || true)" ]]; then
  echo "error: /mnt is itself a mountpoint; expected it to be a plain directory" >&2
  echo "       containing the /mnt/dashcam mount. Refusing to continue." >&2
  exit 1
fi

# compose.yaml binds /mnt into the container with rslave propagation, which
# only receives new mounts from the host if the mount /mnt lives under is
# itself (at least) shared on the host side. Most distros ship the root
# mount as private nowadays, which would silently break hotplug: the mount
# unit would come up on the host but the container would never see it.
propagation="$(findmnt -no PROPAGATION -T /mnt 2>/dev/null || true)"
if [[ "$propagation" != *shared* ]]; then
  echo "error: host mount covering /mnt has propagation '${propagation:-unknown}', not shared." >&2
  echo "       rslave hotplug (compose.yaml's /mnt bind) requires a shared host root." >&2
  echo "       Fix with: sudo mount --make-rshared /" >&2
  exit 1
fi

# The fstab-managed mount unit tears down and remounts /mnt/dashcam
# underneath it, and we're about to unmount the drive out from under
# anything currently using it — both would yank the filesystem out from
# under a running app container.
if [[ "${SKIP_CONTAINER_CHECK:-}" != "1" ]]; then
  if docker ps --format '{{.Names}}' | grep -qx dashcam-manager-app-1; then
    echo "error: dashcam-manager-app-1 is running." >&2
    echo "       Run 'docker compose stop app' first, then re-run this script." >&2
    echo "       (set SKIP_CONTAINER_CHECK=1 to bypass this check)" >&2
    exit 1
  fi
fi

mkdir -p "$MNT"
# compose's /mnt/shares bind target lives on the host's /mnt too (see the
# rslave bind of /mnt in compose.yaml), so it needs to exist as a plain
# directory on the host ahead of time for that bind target to resolve.
mkdir -p /mnt/shares

# Configured means the UUID is present in fstab AND mounted at exactly
# $MNT — findmnt --fstab parses the file the same way systemd's generator
# does, so this can't be fooled by a commented-out line or whitespace
# differences the way a plain grep could.
if findmnt --fstab -S "UUID=${UUID}" -M "$MNT" >/dev/null 2>&1; then
  echo "fstab already configured"
elif grep -q "UUID=${UUID}" /etc/fstab; then
  echo "error: UUID=${UUID} already appears in /etc/fstab but not mounted at ${MNT}" >&2
  echo "       (different mountpoint, or the line is commented out). Refusing to" >&2
  echo "       append a second entry — fix or remove the existing line first." >&2
  exit 1
else
  backup="/etc/fstab.bak.$(date +%s)"
  cp /etc/fstab "$backup"
  echo "backed up /etc/fstab to $backup"
  echo "UUID=${UUID} ${MNT} exfat rw,nofail,noatime,uid=${UID_VAL},gid=${GID_VAL},umask=0002,x-systemd.device-timeout=10s 0 0" >>/etc/fstab
  echo "added fstab entry for UUID=${UUID}"
fi

# Regenerate the fstab-derived mount/device units now that the entry above
# exists, before we touch any existing mount of the drive below.
systemctl daemon-reload

# If the device is already mounted somewhere (e.g. udisks auto-mounted it
# under /media/*, or it's manually mounted), unmount it so the fstab-managed
# mount unit can take it over cleanly. The container will be re-created
# afterwards anyway, so a brief unmount here is harmless.
if by_uuid_mounts="$(findmnt -S "/dev/disk/by-uuid/${UUID}" -n -o TARGET 2>/dev/null || true)" && [[ -n "$by_uuid_mounts" ]]; then
  while IFS= read -r target; do
    [[ -z "$target" ]] && continue
    echo "unmounting existing mount at $target"
    if ! umount "$target"; then
      echo "warning: failed to unmount $target (likely busy)." >&2
      echo "         find what's using it with 'lsof +f -- $target' or 'fuser -vm $target'," >&2
      echo "         stop the app container ('docker compose stop app'), then re-run." >&2
    fi
  done <<<"$by_uuid_mounts"
fi

if [[ -e "/dev/disk/by-uuid/${UUID}" ]]; then
  systemctl start mnt-dashcam.mount
  findmnt "$MNT"
else
  echo "device absent; it will mount automatically when plugged in"
fi

echo
echo "next: docker compose up -d --force-recreate app"
