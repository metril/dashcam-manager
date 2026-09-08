package share

import (
	"bufio"
	"context"
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
// mount point (e.g. "My NAS!" -> "my-nas"). id is only used as the
// fallback when name has no usable characters (or sanitizes down to "."
// or ".." — which would otherwise let MountPointFor resolve to base
// itself or its parent, a path-traversal hazard since slugs feed
// directly into a filesystem path used for a privileged mount), so two
// different names still normally produce two different slugs. Callers
// (db.ShareCreateWithSource) call this exactly once, at creation, and
// store the result as the share's mount_point rather than ever
// recomputing it — so a share's mount point stays fixed even if it's
// later renamed via PATCH, regardless of what Slug(newName, id) would
// produce.
func Slug(name string, id int64) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" || s == "." || s == ".." {
		return fmt.Sprintf("share-%d", id)
	}
	return s
}

// MountPointFor returns the mount point path for a share slug under base.
// Defense in depth against Slug ever producing something that escapes
// base (it shouldn't — Slug already rejects "."/".."/empty — but a mount
// point is a privileged operation, so this never trusts a single layer of
// sanitization alone): if the joined path doesn't resolve under base, a
// fixed fallback under base is used instead of ever mounting outside it.
func MountPointFor(base, slug string) string {
	clean := filepath.Clean(base)
	mp := filepath.Join(clean, slug)
	// Valid iff mp is strictly under clean — mp == clean (e.g. slug ""
	// or ".", which filepath.Join/Clean collapse away entirely) is just
	// as unsafe as escaping it outright, since it would mount directly
	// onto the shares base directory itself.
	if mp == clean || !strings.HasPrefix(mp, clean+string(filepath.Separator)) {
		return filepath.Join(clean, "invalid-slug")
	}
	return mp
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
// can add things like "ro" or "vers=2.1" per share). Callers must run
// extra through ValidateOptions first — this function trusts it's already
// been checked.
func mountOptions(credFile, extra string) string {
	opts := fmt.Sprintf("credentials=%s,vers=3.0,iocharset=utf8,uid=0,gid=0,file_mode=0644,dir_mode=0755", credFile)
	extra = strings.Trim(extra, ", ")
	if extra != "" {
		opts += "," + extra
	}
	return opts
}

// deniedOptionKeys are mount.cifs option keys (and their short synonyms —
// see `man mount.cifs`) that would let a caller override credentials
// handling: smuggling a plaintext password via the "extra options" field
// instead of the dedicated password field, or clobbering the
// credentials=/username=/domain= options this package always sets from
// the share's own fields. Matched against the key only (the part of each
// comma-separated "key=value" option before "="), not by substring, so a
// legitimate option that merely contains one of these words doesn't false-
// positive.
var deniedOptionKeys = map[string]bool{
	"password": true, "pass": true,
	"credentials": true,
	"user":        true, "username": true,
	"domain": true, "dom": true,
}

// ValidateOptions rejects an operator-supplied extra-options string that
// tries to set credentials directly (mount.cifs accepts the last
// occurrence of a repeated option, so a smuggled "password=" here would
// silently override the credentials file). Called by the /api/shares
// handlers before persisting Options, and defensively again by runMount.
func ValidateOptions(extra string) error {
	extra = strings.Trim(extra, ", ")
	if extra == "" {
		return nil
	}
	for _, part := range strings.Split(extra, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := part
		if i := strings.Index(part, "="); i >= 0 {
			key = part[:i]
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if deniedOptionKeys[key] {
			return fmt.Errorf("share: options must not set %q; use the username/password/domain fields instead", key)
		}
	}
	return nil
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
	if err := ValidateOptions(s.Options); err != nil {
		return err
	}

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
		return classifyMountError(err, string(out))
	}
	return nil
}

// mountErrorKind classifies a failed mount.cifs invocation so callers (and
// the /api/shares handlers) can surface something more actionable than the
// raw exit status.
type mountErrorKind int

const (
	mountErrUnknown mountErrorKind = iota
	// mountErrPrivilege: the calling process itself lacks the
	// capabilities mount.cifs needs (CAP_SYS_ADMIN and CAP_DAC_READ_SEARCH
	// — see compose.yaml's cap_add and the README's "CIFS mount
	// capabilities" section), or isn't running inside the container at
	// all. mount.cifs fails before ever contacting the server in this
	// case.
	mountErrPrivilege
	// mountErrAuth: the server was reached but rejected the
	// username/password/domain (mount.cifs exits 32 with "Permission
	// denied" / "error(13)").
	mountErrAuth
	// mountErrUnreachable: the server/network couldn't be reached at all
	// (connection refused/no route/timed out).
	mountErrUnreachable
)

// classifyMountOutput inspects mount(.cifs)'s combined stdout+stderr to
// classify why it failed. Kept separate from classifyMountError so it's
// directly unit-testable against captured output strings without needing
// to fabricate an *exec.ExitError.
func classifyMountOutput(output string) mountErrorKind {
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "only root can do that"),
		strings.Contains(lower, "unable to apply new capability set"),
		strings.Contains(lower, "operation not permitted"),
		strings.Contains(lower, "permission denied") && !strings.Contains(output, "error(13)"):
		// The bare "permission denied" (no error(13) code attached) case
		// covers mount(8) itself refusing to run the mount syscall, as
		// opposed to mount.cifs's own error(13) which means the *server*
		// rejected the credentials (see mountErrAuth below).
		return mountErrPrivilege
	case strings.Contains(output, "error(13)"):
		return mountErrAuth
	case strings.Contains(output, "error(113)"), // no route to host
		strings.Contains(output, "error(110)"), // connection timed out
		strings.Contains(output, "error(111)"), // connection refused
		strings.Contains(output, "error(115)"): // operation now in progress (connect never completed)
		return mountErrUnreachable
	default:
		return mountErrUnknown
	}
}

// classifyMountError wraps a failed mount(8) invocation's error with a
// classification-specific, actionable message; execErr is preserved via
// %w so callers can still errors.As it if needed.
func classifyMountError(execErr error, output string) error {
	msg := strings.TrimSpace(output)
	switch classifyMountOutput(output) {
	case mountErrPrivilege:
		return fmt.Errorf("share: mount: insufficient privileges to mount CIFS shares (container needs cap_add: [SYS_ADMIN, DAC_READ_SEARCH]; see README): %w: %s", execErr, msg)
	case mountErrAuth:
		return fmt.Errorf("share: mount: authentication failed — check username/password/domain: %w: %s", execErr, msg)
	case mountErrUnreachable:
		return fmt.Errorf("share: mount: server unreachable — check the server address and network: %w: %s", execErr, msg)
	default:
		return fmt.Errorf("share: mount: %w: %s", execErr, msg)
	}
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
