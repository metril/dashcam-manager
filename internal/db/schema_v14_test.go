package db

import (
	"context"
	"testing"
)

func TestMigrateV13ToV14(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	cols := []string{"tire_fl_kpa", "tire_fr_kpa", "tire_rl_kpa", "tire_rr_kpa", "odometer_km", "range_km", "ambient_c"}
	var stmts []string
	for _, c := range cols {
		stmts = append(stmts, `ALTER TABLE segment_obd_points DROP COLUMN `+c)
	}
	stmts = append(stmts, `UPDATE settings SET value_json = '13' WHERE key = 'schema_version'`)
	for _, stmt := range stmts {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v13->v14): %v", err)
	}
	for _, c := range cols {
		var n int
		if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('segment_obd_points') WHERE name = ?`, c).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("segment_obd_points.%s missing", c)
		}
	}
}
