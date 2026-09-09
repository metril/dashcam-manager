package ssd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// privateDevNode is where DefaultResolveDevice creates a private block
// device node for the SSD, rather than relying on the real device node
// existing in the container's own /dev — see DefaultResolveDevice's doc
// comment for why. A package var (not a const) so tests can point it at
// a scratch path instead of writing into the real container /dev.
var privateDevNode = "/dev/dashcam-ssd"

// blockNamePattern is the allowed shape of a kernel block device name
// (e.g. "sda1", "nvme0n1p1") — validated before it's ever joined onto
// /sys/class/block or used to look anything else up, same reasoning as
// config.ValidSSDUUID: defense against a crafted symlink target walking
// somewhere it shouldn't via "/" or "..".
var blockNamePattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// DefaultResolveDevice resolves uuid's by-uuid symlink to a private block
// device node suitable for mounting.
//
// compose.yaml bind-mounts only /dev/disk (read-only) into the container,
// not all of /dev, so as to not hand the container every host device
// node. That means the by-uuid symlink's target (a relative path like
// "../../sdb1", i.e. /dev/sdb1) does not exist inside the container —
// only /dev/disk/by-uuid/<uuid> itself does. To get something mountable
// without widening that bind, this reads the symlink just far enough to
// get the target's basename (e.g. "sdb1"), looks up that block device's
// major:minor from sysfs (which is unaffected by the narrower /dev bind),
// and creates a private block device node under the container's own
// /dev (privateDevNode) with that major:minor — the same device the host
// would mount, without exposing any other device node to the container.
// Requires CAP_MKNOD, which is part of Docker's default capability set.
func DefaultResolveDevice(uuid string) (string, error) {
	link := filepath.Join(byUUIDDir, uuid)
	rawTarget, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("ssd: readlink %s: %w", link, err)
	}
	name := filepath.Base(rawTarget)
	if !blockNamePattern.MatchString(name) {
		return "", fmt.Errorf("ssd: refusing suspicious block device name %q (from %s -> %s)", name, link, rawTarget)
	}

	major, minor, err := sysfsBlockDevNumbers(name)
	if err != nil {
		return "", err
	}

	if err := os.Remove(privateDevNode); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("ssd: remove stale %s: %w", privateDevNode, err)
	}
	dev := int(unix.Mkdev(major, minor))
	if err := unix.Mknod(privateDevNode, unix.S_IFBLK|0o660, dev); err != nil {
		return "", fmt.Errorf("ssd: mknod %s (%d:%d): %w", privateDevNode, major, minor, err)
	}
	return privateDevNode, nil
}

// sysfsBlockDevNumbers reads /sys/class/block/<name>/dev (format
// "MAJOR:MINOR\n") for the major/minor device numbers of block device
// name.
func sysfsBlockDevNumbers(name string) (major, minor uint32, err error) {
	path := filepath.Join("/sys/class/block", name, "dev")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("ssd: read %s: %w", path, err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(raw)), ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("ssd: malformed %s contents %q", path, string(raw))
	}
	maj, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("ssd: parse major from %s (%q): %w", path, string(raw), err)
	}
	min, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("ssd: parse minor from %s (%q): %w", path, string(raw), err)
	}
	return uint32(maj), uint32(min), nil
}

// mountData builds the exfat mount data string: uid/gid of the running
// process (so files are owned by, and browsable to, the API process's
// user rather than defaulting to root) plus nothing else — noatime is
// passed as a mount flag, not a data option, for the syscall path, and as
// an -o option for the exec fallback (see buildFallbackArgs).
func mountData(uid, gid int) string {
	return fmt.Sprintf("uid=%d,gid=%d", uid, gid)
}

// buildFallbackArgs returns the argv for the `mount` command fallback,
// kept as a pure function (no exec) so it's directly unit testable —
// mirrors share.buildMountArgs's approach for the CIFS case.
func buildFallbackArgs(dev, target, data string) []string {
	return []string{"-t", "exfat", "-o", "noatime," + data, dev, target}
}

// DefaultMount mounts dev at target as exfat via the unix.Mount syscall
// (MS_NOATIME, uid/gid of the running process), falling back to `mount -t
// exfat` (needs util-linux + exfatprogs in the runtime image — see
// Dockerfile) if the syscall fails, mirroring share.runMount's exec-based
// approach for the CIFS case. Creates target if it doesn't already exist.
func DefaultMount(dev, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("ssd: mkdir %s: %w", target, err)
	}

	data := mountData(os.Getuid(), os.Getgid())
	if err := unix.Mount(dev, target, "exfat", unix.MS_NOATIME, data); err == nil {
		return nil
	} else {
		cmd := exec.Command("mount", buildFallbackArgs(dev, target, data)...)
		out, execErr := cmd.CombinedOutput()
		if execErr != nil {
			return fmt.Errorf("syscall mount: %v; mount(8) fallback: %v: %s", err, execErr, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// DefaultUnmount performs a lazy (MNT_DETACH) unmount of target — used on
// device removal, when the underlying block device is already gone and a
// normal unmount could otherwise hang or fail outright.
func DefaultUnmount(target string) error {
	if err := unix.Unmount(target, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// Eject performs a clean (non-lazy) unmount of target, for a
// user-initiated eject (POST /api/ingest/eject) or graceful shutdown.
// Returns unix.EBUSY (wrapped, so errors.Is still works) if something is
// still using the mount — callers decide from there whether to refuse
// (e.g. a scan job is running on the SSD source) or fall back to
// ForceEject.
func Eject(target string) error {
	if err := unix.Unmount(target, 0); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil // already unmounted
		}
		return fmt.Errorf("ssd: eject %s: %w", target, err)
	}
	return nil
}

// ForceEject performs a lazy unmount of target — used when Eject reports
// EBUSY and the caller has confirmed it's safe to detach anyway (no
// active scan job on the SSD source).
func ForceEject(target string) error {
	if err := DefaultUnmount(target); err != nil {
		return fmt.Errorf("ssd: force eject %s: %w", target, err)
	}
	return nil
}
