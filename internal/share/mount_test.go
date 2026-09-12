package share

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeLocalAcceptsWritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := ProbeLocal(dir); err != nil {
		t.Fatalf("ProbeLocal(%q): %v", dir, err)
	}
}

func TestProbeLocalRejectsMissingPath(t *testing.T) {
	if err := ProbeLocal(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("ProbeLocal on a missing path: want error, got nil")
	}
}

func TestProbeLocalRejectsFileNotDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := ProbeLocal(file); err == nil {
		t.Fatal("ProbeLocal on a plain file: want error, got nil")
	}
}

func TestBuildMountArgsNoPasswordInArgv(t *testing.T) {
	args := buildMountArgs("nas.local", "footage", "dashcam", "/mnt/shares/nas", "/tmp/cred123", "ro")

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "hunter2") {
		t.Fatalf("mount args contain a literal password: %v", args)
	}
	for _, a := range args {
		if strings.Contains(strings.ToLower(a), "password=") && !strings.Contains(a, "credentials=") {
			t.Errorf("mount args leak password= outside the credentials file option: %v", args)
		}
	}

	want := []string{
		"-t", "cifs",
		"//nas.local/footage/dashcam",
		"/mnt/shares/nas",
		"-o", "credentials=/tmp/cred123,vers=3.0,iocharset=utf8,uid=0,gid=0,file_mode=0644,dir_mode=0755,ro",
	}
	if len(args) != len(want) {
		t.Fatalf("buildMountArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("buildMountArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestBuildMountArgsNoSubpathOrExtraOptions(t *testing.T) {
	args := buildMountArgs("nas.local", "footage", "", "/mnt/shares/nas", "/tmp/cred", "")
	if args[2] != "//nas.local/footage" {
		t.Errorf("source = %q, want //nas.local/footage", args[2])
	}
	if args[5] != "credentials=/tmp/cred,vers=3.0,iocharset=utf8,uid=0,gid=0,file_mode=0644,dir_mode=0755" {
		t.Errorf("unexpected options: %q", args[5])
	}
}

func TestCredentialsFileContentHasNoTrailingLeakage(t *testing.T) {
	content := credentialsFileContent("alice", "hunter2", "WORKGROUP")
	if !strings.Contains(content, "username=alice\n") {
		t.Errorf("missing username line: %q", content)
	}
	if !strings.Contains(content, "password=hunter2\n") {
		t.Errorf("missing password line: %q", content)
	}
	if !strings.Contains(content, "domain=WORKGROUP\n") {
		t.Errorf("missing domain line: %q", content)
	}
}

func TestCredentialsFileContentOmitsEmptyDomain(t *testing.T) {
	content := credentialsFileContent("alice", "hunter2", "")
	if strings.Contains(content, "domain=") {
		t.Errorf("expected no domain= line for empty domain, got: %q", content)
	}
}

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		id   int64
		want string
	}{
		{"basic sanitization", "My NAS!", 7, "my-nas"},
		{"already clean", "garage-cam", 7, "garage-cam"},
		{"surrounding whitespace", "  Spaces  ", 7, "spaces"},
		{"empty falls back to id", "", 7, "share-7"},
		{"punctuation-only falls back to id", "!!!", 7, "share-7"},
		{"dots and underscore preserved", "Nas_01.local", 7, "nas_01.local"},
		{"exactly . falls back to id", ".", 3, "share-3"},
		{"exactly .. falls back to id", "..", 3, "share-3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Slug(c.in, c.id); got != c.want {
				t.Errorf("Slug(%q, %d) = %q, want %q", c.in, c.id, got, c.want)
			}
		})
	}
}

func TestSlugFallsBackConsistentlyForSameID(t *testing.T) {
	// The fallback path (name sanitizes to nothing usable) must be
	// deterministic per id, so MountPointFor/ShareCreateWithSource never
	// see it change between the two Slug(name, id) calls a single create
	// would only ever make once anyway, but determinism matters for
	// testability.
	if got, want := Slug("!!!", 42), "share-42"; got != want {
		t.Errorf("Slug(%q, 42) = %q, want %q", "!!!", got, want)
	}
	if got, want := Slug("???", 42), "share-42"; got != want {
		t.Errorf("Slug(%q, 42) = %q, want %q", "???", got, want)
	}
}

func TestMountPointForStaysUnderBase(t *testing.T) {
	// Slug() should already prevent a "."/".."-only slug from reaching
	// here, but MountPointFor is the last line of defense for a
	// privileged mount target, so it's tested directly against the
	// dangerous inputs too.
	cases := []struct {
		slug string
		want string
	}{
		{"my-nas", "/mnt/shares/my-nas"},
		{".", "/mnt/shares/invalid-slug"},
		{"..", "/mnt/shares/invalid-slug"},
	}
	for _, c := range cases {
		got := MountPointFor("/mnt/shares", c.slug)
		if got != c.want {
			t.Errorf("MountPointFor(%q) = %q, want %q", c.slug, got, c.want)
		}
		if !strings.HasPrefix(got, "/mnt/shares/") {
			t.Errorf("MountPointFor(%q) = %q escapes /mnt/shares", c.slug, got)
		}
	}
}

func TestValidateOptions(t *testing.T) {
	valid := []string{"", "ro", "ro,vers=2.1", "noserverino"}
	for _, v := range valid {
		if err := ValidateOptions(v); err != nil {
			t.Errorf("ValidateOptions(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"password=hunter2",
		"ro,PASSWORD=hunter2",
		"credentials=/tmp/other",
		"user=admin",
		"username=admin",
		"pass=hunter2",
	}
	for _, v := range invalid {
		if err := ValidateOptions(v); err == nil {
			t.Errorf("ValidateOptions(%q) = nil, want an error", v)
		}
	}
}

func TestClassifyMountOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   mountErrorKind
	}{
		{"capability set failure", "Unable to apply new capability set.", mountErrPrivilege},
		{"only root", "mount: only root can do that", mountErrPrivilege},
		{"auth rejected", "mount error(13): Permission denied\nRefer to the mount.cifs(8) manual page", mountErrAuth},
		{"no route to host", "mount error(113): No route to host", mountErrUnreachable},
		{"connection timed out", "mount error(110): Connection timed out", mountErrUnreachable},
		{"connection refused", "mount error(111): Connection refused", mountErrUnreachable},
		{"operation in progress", "mount error(115): Operation now in progress", mountErrUnreachable},
		{"unrecognized", "something unexpected happened", mountErrUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyMountOutput(c.output); got != c.want {
				t.Errorf("classifyMountOutput(%q) = %v, want %v", c.output, got, c.want)
			}
		})
	}
}

func TestScanMountsForPath(t *testing.T) {
	proc := strings.NewReader(strings.Join([]string{
		"/dev/sda1 / ext4 rw,relatime 0 0",
		"//nas.local/footage /mnt/shares/nas cifs rw,vers=3.0 0 0",
		"tmpfs /mnt/shares/other/ tmpfs rw 0 0",
		"",
	}, "\n"))

	found, err := scanMountsForPath(proc, "/mnt/shares/nas")
	if err != nil {
		t.Fatalf("scanMountsForPath: %v", err)
	}
	if !found {
		t.Error("expected /mnt/shares/nas to be found mounted")
	}
}

func TestScanMountsForPathTrailingSlashNormalized(t *testing.T) {
	proc := strings.NewReader("tmpfs /mnt/shares/other/ tmpfs rw 0 0\n")

	found, err := scanMountsForPath(proc, "/mnt/shares/other")
	if err != nil {
		t.Fatalf("scanMountsForPath: %v", err)
	}
	if !found {
		t.Error("expected /mnt/shares/other (no trailing slash) to match the /mnt/shares/other/ mounts line")
	}
}

func TestScanMountsForPathNotFound(t *testing.T) {
	proc := strings.NewReader("/dev/sda1 / ext4 rw,relatime 0 0\n")

	found, err := scanMountsForPath(proc, "/mnt/shares/nope")
	if err != nil {
		t.Fatalf("scanMountsForPath: %v", err)
	}
	if found {
		t.Error("expected /mnt/shares/nope to not be found mounted")
	}
}
