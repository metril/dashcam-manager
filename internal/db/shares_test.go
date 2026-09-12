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
