package share

import (
	"strings"
	"testing"
)

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
	cases := map[string]string{
		"My NAS!":      "my-nas",
		"garage-cam":   "garage-cam",
		"  Spaces  ":   "spaces",
		"":             "share",
		"!!!":          "share",
		"Nas_01.local": "nas_01.local",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
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
