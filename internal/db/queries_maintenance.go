package db

import (
	"context"
	"fmt"
)

// SegmentIDsForTrip returns every segment id belonging to tripID, for the
// "trip" scope of POST /api/maintenance/thumbs/regenerate.
func (d *DB) SegmentIDsForTrip(ctx context.Context, tripID int64) ([]int64, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT id FROM segments WHERE trip_id = ?`, tripID)
	if err != nil {
		return nil, fmt.Errorf("db: segment ids for trip %d: %w", tripID, err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: segment ids for trip %d: %w", tripID, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
