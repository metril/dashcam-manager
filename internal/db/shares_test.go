package db

import (
	"context"
	"testing"
)

// TestShareCreateDefaultsKindToCIFS covers ShareCreateParams.Kind's default:
// an omitted Kind (the common case for every pre-v9 caller) must still
// store 'cifs', matching the shares.kind column's own DEFAULT — ShareCreate
// lists every column explicitly, which bypasses that column default, so
// normalizeKind has to reproduce it in Go.
func TestShareCreateDefaultsKindToCIFS(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	id, err := d.ShareCreate(ctx, ShareCreateParams{Name: "nas", Server: "10.0.0.5", ShareName: "footage", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	s, err := d.ShareGet(ctx, id)
	if err != nil {
		t.Fatalf("ShareGet: %v", err)
	}
	if s.Kind != KindCIFS {
		t.Errorf("Kind = %q, want %q (default)", s.Kind, KindCIFS)
	}
}

// TestShareCreateStoresLocalKind covers the new kind='local' path end to
// end through ShareCreate/ShareGet.
func TestShareCreateStoresLocalKind(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	id, err := d.ShareCreate(ctx, ShareCreateParams{Name: "local dir", Kind: KindLocal, MountPoint: "/mnt/shares/local-test", Enabled: true})
	if err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	s, err := d.ShareGet(ctx, id)
	if err != nil {
		t.Fatalf("ShareGet: %v", err)
	}
	if s.Kind != KindLocal {
		t.Errorf("Kind = %q, want %q", s.Kind, KindLocal)
	}
	if s.MountPoint != "/mnt/shares/local-test" {
		t.Errorf("MountPoint = %q, want /mnt/shares/local-test", s.MountPoint)
	}
}

// TestShareCreateWithSourceDefaultsKindToCIFS covers the other insert path
// (ShareCreateWithSource), used by production code (e.g. the dashcam pull
// target share).
func TestShareCreateWithSourceDefaultsKindToCIFS(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID, _, err := d.ShareCreateWithSource(ctx, ShareCreateParams{
		Name: "nas", Server: "10.0.0.5", ShareName: "footage", Enabled: true,
	}, func(id int64) string { return "/mnt/shares/nas" }, "test share")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}
	s, err := d.ShareGet(ctx, shareID)
	if err != nil {
		t.Fatalf("ShareGet: %v", err)
	}
	if s.Kind != KindCIFS {
		t.Errorf("Kind = %q, want %q (default)", s.Kind, KindCIFS)
	}
}

// TestShareUpdateLocalSyncsLinkedSourceRootPath covers final-fix-wave item
// 1: PATCHing a local storage's path used to update shares.mount_point
// only, leaving its linked "share"-kind source's root_path pointed at the
// old directory forever. ShareUpdateLocal must update both in the same Tx.
func TestShareUpdateLocalSyncsLinkedSourceRootPath(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	shareID, sourceID, err := d.ShareCreateWithSource(ctx, ShareCreateParams{
		Name: "old-name", Kind: KindLocal, Enabled: true,
	}, func(int64) string { return "/mnt/shares/old-path" }, "old-name")
	if err != nil {
		t.Fatalf("ShareCreateWithSource: %v", err)
	}

	if err := d.ShareUpdateLocal(ctx, shareID, "new-name", "/mnt/shares/new-path", true); err != nil {
		t.Fatalf("ShareUpdateLocal: %v", err)
	}

	src, err := d.SourceGetByShareID(ctx, shareID)
	if err != nil {
		t.Fatalf("SourceGetByShareID: %v", err)
	}
	if src.ID != sourceID {
		t.Fatalf("SourceGetByShareID returned source %d, want the original %d", src.ID, sourceID)
	}
	if src.RootPath != "/mnt/shares/new-path" {
		t.Errorf("source.RootPath = %q, want /mnt/shares/new-path (ShareUpdateLocal should keep it in sync)", src.RootPath)
	}
	if src.Label != "new-name" {
		t.Errorf("source.Label = %q, want new-name", src.Label)
	}

	s, err := d.ShareGet(ctx, shareID)
	if err != nil {
		t.Fatalf("ShareGet: %v", err)
	}
	if s.MountPoint != "/mnt/shares/new-path" {
		t.Errorf("share.MountPoint = %q, want /mnt/shares/new-path", s.MountPoint)
	}
}

// TestSharesListIncludesKind covers SharesList/scanShare's kind column.
func TestSharesListIncludesKind(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if _, err := d.ShareCreate(ctx, ShareCreateParams{Name: "nas", Server: "10.0.0.5", ShareName: "footage"}); err != nil {
		t.Fatalf("ShareCreate: %v", err)
	}
	shares, err := d.SharesList(ctx)
	if err != nil {
		t.Fatalf("SharesList: %v", err)
	}
	if len(shares) != 1 || shares[0].Kind != KindCIFS {
		t.Fatalf("SharesList = %+v, want one %q-kind share", shares, KindCIFS)
	}
}
