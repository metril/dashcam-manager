package db

import (
	"context"
	"testing"
)

// insertTestJob inserts a minimal jobs row (exports.job_id has a foreign
// key against it) and returns its id.
func insertTestJob(t *testing.T, d *DB) int64 {
	t.Helper()
	var id int64
	err := d.Write().QueryRowContext(context.Background(),
		`INSERT INTO jobs (type, state) VALUES ('export', 'queued') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert test job: %v", err)
	}
	return id
}

// insertTestShare inserts a minimal shares row (exports.share_id has a
// foreign key against it) and returns its id.
func insertTestShare(t *testing.T, d *DB) int64 {
	t.Helper()
	var id int64
	err := d.Write().QueryRowContext(context.Background(),
		`INSERT INTO shares (name, server, share) VALUES ('test', 'nas.local', 'footage') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert test share: %v", err)
	}
	return id
}

func TestExportCreateGetList(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	exp, err := d.ExportCreate(ctx, "my-clip")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	if exp.Name != "my-clip" || exp.JobID != nil || exp.Path != nil {
		t.Errorf("ExportCreate = %+v, want fresh row with name set, job_id/path nil", exp)
	}

	jobID := insertTestJob(t, d)
	if err := d.ExportSetJobID(ctx, exp.ID, jobID); err != nil {
		t.Fatalf("ExportSetJobID: %v", err)
	}
	got, err := d.ExportGet(ctx, exp.ID)
	if err != nil {
		t.Fatalf("ExportGet: %v", err)
	}
	if got.JobID == nil || *got.JobID != jobID {
		t.Errorf("ExportGet.JobID = %v, want %d", got.JobID, jobID)
	}

	list, err := d.ExportsList(ctx)
	if err != nil {
		t.Fatalf("ExportsList: %v", err)
	}
	if len(list) != 1 || list[0].ID != exp.ID {
		t.Errorf("ExportsList = %+v, want one row with id %d", list, exp.ID)
	}
}

func TestExportGetNotFound(t *testing.T) {
	d := openTest(t)
	if _, err := d.ExportGet(context.Background(), 999); err != ErrNotFound {
		t.Errorf("ExportGet(missing) err = %v, want ErrNotFound", err)
	}
}

func TestExportFinishSetsPathBytesExpiry(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	exp, err := d.ExportCreate(ctx, "clip")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	expiry := "2026-09-15T00:00:00.000Z"
	if err := d.ExportFinish(ctx, exp.ID, "/cache/exports/1.mp4", nil, 12345, &expiry); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}
	got, err := d.ExportGet(ctx, exp.ID)
	if err != nil {
		t.Fatalf("ExportGet: %v", err)
	}
	if got.Path == nil || *got.Path != "/cache/exports/1.mp4" {
		t.Errorf("ExportGet.Path = %v, want /cache/exports/1.mp4", got.Path)
	}
	if got.Bytes != 12345 {
		t.Errorf("ExportGet.Bytes = %d, want 12345", got.Bytes)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != expiry {
		t.Errorf("ExportGet.ExpiresAt = %v, want %v", got.ExpiresAt, expiry)
	}
}

func TestExportsExpiredOnlyReturnsPastExpiry(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()

	past, err := d.ExportCreate(ctx, "old")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	pastExpiry := "2000-01-01T00:00:00.000Z"
	if err := d.ExportFinish(ctx, past.ID, "/cache/exports/old.mp4", nil, 1, &pastExpiry); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}

	future, err := d.ExportCreate(ctx, "new")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	futureExpiry := "2999-01-01T00:00:00.000Z"
	if err := d.ExportFinish(ctx, future.ID, "/cache/exports/new.mp4", nil, 1, &futureExpiry); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}

	// A share-target export never expires (expires_at stays NULL).
	shared, err := d.ExportCreate(ctx, "shared")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	shareID := insertTestShare(t, d)
	if err := d.ExportFinish(ctx, shared.ID, "/mnt/shares/x/exports/shared.mp4", &shareID, 1, nil); err != nil {
		t.Fatalf("ExportFinish: %v", err)
	}

	expired, err := d.ExportsExpired(ctx)
	if err != nil {
		t.Fatalf("ExportsExpired: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != past.ID {
		t.Errorf("ExportsExpired = %+v, want only export %d", expired, past.ID)
	}
}

func TestExportDeleteRemovesRow(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	exp, err := d.ExportCreate(ctx, "clip")
	if err != nil {
		t.Fatalf("ExportCreate: %v", err)
	}
	if err := d.ExportDelete(ctx, exp.ID); err != nil {
		t.Fatalf("ExportDelete: %v", err)
	}
	if _, err := d.ExportGet(ctx, exp.ID); err != ErrNotFound {
		t.Errorf("ExportGet after delete err = %v, want ErrNotFound", err)
	}
}
