package ssd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMountData(t *testing.T) {
	if got, want := mountData(1000, 1000), "uid=1000,gid=1000"; got != want {
		t.Errorf("mountData() = %q, want %q", got, want)
	}
}

func TestBuildFallbackArgs(t *testing.T) {
	got := buildFallbackArgs("/dev/sda1", "/mnt/dashcam", "uid=1000,gid=1000")
	want := []string{"-t", "exfat", "-o", "noatime,uid=1000,gid=1000", "/dev/sda1", "/mnt/dashcam"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildFallbackArgs() = %v, want %v", got, want)
	}
}

func TestBlockNamePattern(t *testing.T) {
	cases := map[string]bool{
		"sda1":       true,
		"nvme0n1p1":  true,
		"":           false,
		"../../etc":  false,
		"sda1/../..": false,
		"sd a1":      false,
	}
	for in, want := range cases {
		if got := blockNamePattern.MatchString(in); got != want {
			t.Errorf("blockNamePattern.MatchString(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSysfsBlockDevNumbersErrorsForNonexistentDevice(t *testing.T) {
	// sysfsBlockDevNumbers reads the fixed path /sys/class/block/<name>/dev
	// (nothing to inject for testing), so the only thing exercisable
	// without real hardware is that a nonexistent block name fails
	// cleanly rather than panicking or returning zero values silently.
	if _, _, err := sysfsBlockDevNumbers("dashcam-test-nonexistent-block-device"); err == nil {
		t.Error("expected an error for a nonexistent block device name")
	}
}

func TestDefaultResolveDeviceRejectsSymlinkOutsideSysfs(t *testing.T) {
	dir := withFakeByUUIDDir(t)
	// filepath.Base already strips any directory components from the
	// symlink target before it's used, so a traversal attempt like this
	// can't walk anywhere — it just becomes a lookup for a block device
	// named "passwd", which doesn't exist in /sys/class/block, so this
	// fails safely rather than resolving to /etc/passwd or similar.
	link := filepath.Join(dir, testUUID)
	if err := os.Symlink("../../../etc/passwd", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := DefaultResolveDevice(testUUID); err == nil {
		t.Error("expected DefaultResolveDevice to fail for a symlink target with no matching real block device")
	}
}
