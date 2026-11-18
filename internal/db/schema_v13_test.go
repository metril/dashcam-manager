package db

import (
	"context"
	"testing"
)

func TestMigrateV12ToV13(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	trip := []string{"odometer_start_km", "odometer_end_km", "odometer_distance_km", "range_start_km", "range_end_km", "ambient_min_c", "ambient_max_c", "ambient_avg_c", "tire_fl_min_kpa", "tire_fl_max_kpa", "tire_fl_avg_kpa", "tire_fr_min_kpa", "tire_fr_max_kpa", "tire_fr_avg_kpa", "tire_rl_min_kpa", "tire_rl_max_kpa", "tire_rl_avg_kpa", "tire_rr_min_kpa", "tire_rr_max_kpa", "tire_rr_avg_kpa"}
	var stmts []string
	for _, c := range trip {
		stmts = append(stmts, `ALTER TABLE vehicle_trips DROP COLUMN `+c)
	}
	stmts = append(stmts,
		`ALTER TABLE vehicle_events DROP COLUMN wheel`,
		`INSERT INTO sources (kind, root_path, vehicle_trips_cursor, vehicle_events_cursor) VALUES ('dashcam', '/cam', 5, 7)`,
		`UPDATE settings SET value_json = '12' WHERE key = 'schema_version'`,
	)
	for _, stmt := range stmts {
		if _, err := d.Write().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := d.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema (v12->v13): %v", err)
	}
	has := func(table, col string) bool {
		var n int
		if err := d.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	for _, c := range trip {
		if !has("vehicle_trips", c) {
			t.Errorf("vehicle_trips.%s missing", c)
		}
	}
	if !has("vehicle_events", "wheel") {
		t.Error("vehicle_events.wheel missing")
	}
	var tc, ec *int64
	if err := d.Read().QueryRowContext(ctx, `SELECT vehicle_trips_cursor, vehicle_events_cursor FROM sources WHERE kind = 'dashcam'`).Scan(&tc, &ec); err != nil {
		t.Fatal(err)
	}
	if tc != nil || ec != nil {
		t.Errorf("cursors = %v, %v, want NULL", tc, ec)
	}
}
