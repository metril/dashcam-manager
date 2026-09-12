package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckSourcesShareBackedByLocalKindShareIsAvailable directly covers
// checkSources' kind=local bypass (fix-round-1 item 2): a "share"-kind
// source backed by a kind=local share is available via
// share.LocalDirReadable — a real directory that exists, with no mount and
// no isMountedFn stubbing involved at all.
func TestCheckSourcesShareBackedByLocalKindShareIsAvailable(t *testing.T) {
	d, _ := openTest(t)
	ctx := context.Background()
	root := t.TempDir()

	var shareID int64
	if err := d.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share, mount_point, kind) VALUES ('local-test', '', '', ?, 'local') RETURNING id`,
		root).Scan(&shareID); err != nil {
		t.Fatalf("insert local share: %v", err)
	}
	var sourceID int64
	if err := d.Write().QueryRowContext(ctx,
		`INSERT INTO sources (kind, share_id, root_path, label) VALUES ('share', ?, ?, 'local test share') RETURNING id`,
		shareID, root).Scan(&sourceID); err != nil {
		t.Fatalf("insert share-backed source: %v", err)
	}

	statuses, err := checkSources(ctx, d)
	if err != nil {
		t.Fatalf("checkSources: %v", err)
	}
	if len(statuses) != 1 || !statuses[0].Available {
		t.Fatalf("checkSources = %+v, want exactly one available source", statuses)
	}
}

// TestCheckSourcesShareBackedByLocalKindShareRequiresRealDirectory covers
// the read-only failure path: LocalDirReadable rejects a root_path that
// isn't actually a directory (a plain file), even though the share row
// itself is kind=local.
func TestCheckSourcesShareBackedByLocalKindShareRequiresRealDirectory(t *testing.T) {
	d, _ := openTest(t)
	ctx := context.Background()
	dir := t.TempDir()
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	var shareID int64
	if err := d.Write().QueryRowContext(ctx,
		`INSERT INTO shares (name, server, share, mount_point, kind) VALUES ('local-test', '', '', ?, 'local') RETURNING id`,
		notADir).Scan(&shareID); err != nil {
		t.Fatalf("insert local share: %v", err)
	}
	if _, err := d.Write().ExecContext(ctx,
		`INSERT INTO sources (kind, share_id, root_path, label) VALUES ('share', ?, ?, 'local test share')`,
		shareID, notADir); err != nil {
		t.Fatalf("insert share-backed source: %v", err)
	}

	statuses, err := checkSources(ctx, d)
	if err != nil {
		t.Fatalf("checkSources: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Available {
		t.Fatalf("checkSources = %+v, want exactly one unavailable source (root_path is a file, not a directory)", statuses)
	}
}
