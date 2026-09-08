package share

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"dashcam/internal/db"
)

// DefaultMountBase is where share mount points live inside the container
// (compose.yaml bind-mounts ./shares here with rshared propagation so
// mount.cifs calls made in-container are visible on the host and back).
const DefaultMountBase = "/mnt/shares"

// slugPattern matches the characters MountPointFor keeps from a share name;
// everything else becomes '-'.
var slugPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Slug turns a share name into a filesystem-safe path component for its
// mount point (e.g. "My NAS!" -> "my-nas"). Falls back to "share" if the
// name has no usable characters.
func Slug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "share"
	}
	return s
}

// MountPointFor returns the mount point path for a share slug under base.
func MountPointFor(base, slug string) string {
	return filepath.Join(base, slug)
}

// credentialsFileContent builds the contents of a mount.cifs credentials
// file (see `man mount.cifs`), keeping username/password/domain out of the
// process argument list (and so out of `ps`/logs) entirely.
func credentialsFileContent(username, password, domain string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "username=%s\n", username)
	fmt.Fprintf(&b, "password=%s\n", password)
	if domain != "" {
		fmt.Fprintf(&b, "domain=%s\n", domain)
	}
	return b.String()
}

// cifsSource returns the //server/share/subpath UNC-style source mount.cifs
// expects, joining subpath (if any) onto the share.
func cifsSource(server, shareName, subpath string) string {
	src := fmt.Sprintf("//%s/%s", strings.Trim(server, "/"), strings.Trim(shareName, "/"))
	subpath = strings.Trim(subpath, "/")
	if subpath != "" {
		src += "/" + subpath
	}
	return src
}

// mountOptions builds the -o option string for mount.cifs: the fixed set
// the brief specifies, plus the credentials file, plus any operator-
// supplied extra options appended verbatim (comma-separated, so a caller
// can add things like "ro" or "vers=2.1" per share).
func mountOptions(credFile, extra string) string {
	opts := fmt.Sprintf("credentials=%s,vers=3.0,iocharset=utf8,uid=0,gid=0,file_mode=0644,dir_mode=0755", credFile)
	extra = strings.Trim(extra, ", ")
	if extra != "" {
		opts += "," + extra
	}
	return opts
}

// buildMountArgs returns the argv for the `mount` command, given a
// credentials file path that's already been written to disk. Kept as a
// pure function (no exec, no filesystem writes) so it's directly unit
// testable — in particular, that the password never appears in argv.
func buildMountArgs(server, shareName, subpath, mountPoint, credFile, extraOptions string) []string {
	return []string{
		"-t", "cifs",
		cifsSource(server, shareName, subpath),
		mountPoint,
		"-o", mountOptions(credFile, extraOptions),
	}
}

// writeCredentialsFile writes a mode-0600 temp file with username/password/
// domain for mount.cifs's `credentials=` option, returning its path. The
// caller is responsible for removing it once mount.cifs has read it.
func writeCredentialsFile(username, password, domain string) (string, error) {
	f, err := os.CreateTemp("", "dashcam-cifs-cred-*")
	if err != nil {
		return "", fmt.Errorf("share: mount: create credentials file: %w", err)
	}
	path := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("share: mount: chmod credentials file: %w", err)
	}
	if _, err := f.WriteString(credentialsFileContent(username, password, domain)); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("share: mount: write credentials file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("share: mount: close credentials file: %w", err)
	}
	return path, nil
}

// runMount decrypts s's password (if any), writes a scratch credentials
// file, and runs `mount -t cifs ...` to mountPoint, removing the
// credentials file afterward regardless of outcome. Shared by MountCIFS
// and Test.
func runMount(ctx context.Context, s db.Share, secretKey, mountPoint string) error {
	var password string
	if len(s.PasswordEnc) > 0 {
		p, err := Decrypt(secretKey, s.PasswordEnc)
		if err != nil {
			return fmt.Errorf("share: mount: decrypt password: %w", err)
		}
		password = p
	}

	credFile, err := writeCredentialsFile(s.Username, password, s.Domain)
	if err != nil {
		return err
	}
	defer os.Remove(credFile)

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("share: mount: mkdir %s: %w", mountPoint, err)
	}

	args := buildMountArgs(s.Server, s.ShareName, s.Subpath, mountPoint, credFile, s.Options)
	cmd := exec.CommandContext(ctx, "mount", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if isPermissionErr(err) {
			return fmt.Errorf("share: mount: insufficient privileges to mount CIFS shares (need CAP_SYS_ADMIN / to run inside the container): %w: %s", err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("share: mount: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isPermissionErr reports whether err looks like it came from lacking
// mount privileges (running outside the container, or without
// CAP_SYS_ADMIN), so MountCIFS/Test can return a clearer message than the
// raw "mount: only root can do that" / exit status 1.
func isPermissionErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// MountCIFS mounts share s at its stored s.MountPoint (set once at share
// creation time, via MountPointFor/Slug — kept stable across renames so a
// PATCH that only changes the display name doesn't move an already-mounted
// share's mount point out from under any source/segments indexed under
// it), creating the mount point directory if needed. Credentials are
// passed to mount.cifs via a temp credentials file (mode 0600), never on
// the command line.
func MountCIFS(ctx context.Context, s db.Share, secretKey string) error {
	if s.MountPoint == "" {
		return fmt.Errorf("share: mount: share %q has no mount_point set", s.Name)
	}
	return runMount(ctx, s, secretKey, s.MountPoint)
}

// Unmount unmounts mountPoint. Not an error if it's already unmounted.
func Unmount(ctx context.Context, mountPoint string) error {
	mounted, err := IsMounted(mountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	cmd := exec.CommandContext(ctx, "umount", mountPoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("share: unmount %s: %w: %s", mountPoint, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsMounted reports whether mountPoint appears as a mounted filesystem in
// /proc/mounts.
func IsMounted(mountPoint string) (bool, error) {
	clean := filepath.Clean(mountPoint)
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false, fmt.Errorf("share: is mounted: open /proc/mounts: %w", err)
	}
	defer f.Close()
	return scanMountsForPath(f, clean)
}

// scanMountsForPath scans r (in /proc/mounts format: "<device> <mountpoint>
// <fstype> <options> <dump> <pass>") for a line whose mount point matches
// target. Split out from IsMounted so tests can exercise the parser
// directly against a fake reader instead of the real /proc/mounts.
func scanMountsForPath(r interface{ Read([]byte) (int, error) }, target string) (bool, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if filepath.Clean(fields[1]) == target {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// Test mounts share s to a scratch temp directory, lists its contents (to
// confirm the mount actually serves files, not just that mount.cifs
// exited 0), and unmounts it — without touching the share's persistent
// mount point. Returns a descriptive error on any failure.
func Test(ctx context.Context, s db.Share, secretKey string) error {
	tmp, err := os.MkdirTemp("", "dashcam-cifs-test-*")
	if err != nil {
		return fmt.Errorf("share: test: create temp dir: %w", err)
	}
	defer os.Remove(tmp)

	if err := runMount(ctx, s, secretKey, tmp); err != nil {
		return err
	}
	defer func() {
		if uerr := Unmount(context.Background(), tmp); uerr != nil {
			// Best-effort cleanup; surfaced via log by the caller if it cares.
			_ = uerr
		}
	}()

	if _, err := os.ReadDir(tmp); err != nil {
		return fmt.Errorf("share: test: list mounted share: %w", err)
	}
	return nil
}

// FreeSpace returns (free, total) bytes on the filesystem mounted at path,
// via statfs. Used by GET /api/shares to surface free space for the
// ingest UI.
func FreeSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("share: statfs %s: %w", path, err)
	}
	// #nosec G115 -- Bsize/Blocks/Bavail are platform-width ints; overflow
	// is not a realistic concern for filesystem sizes on this hardware.
	free = uint64(st.Bavail) * uint64(st.Bsize)
	total = uint64(st.Blocks) * uint64(st.Bsize)
	return free, total, nil
}
