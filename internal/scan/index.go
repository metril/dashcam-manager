// index.go builds and writes segment/trip rows from parsed sidecars. Scan is
// the entry point: it walks the tree (walker.go), parses changed segments
// with a bounded worker pool, writes them through a single batching writer,
// then rebuilds trips (pair.go) for every date touched by the scan.
package scan

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"dashcam/internal/db"
)

// batchSize is the number of segment writes committed per transaction.
const batchSize = 500

// numWorkers is the size of the bounded pool that stats mp4s and parses
// sidecars concurrently; a single writer goroutine does all DB writes.
const numWorkers = 4

// Opts configures a Scan call.
type Opts struct {
	// Full forces re-processing of every segment, even ones whose (bytes,
	// mtime) already match the stored row.
	Full bool
	// Progress, if non-nil, is called periodically with (done, total) and a
	// short status message.
	Progress func(done, total int, msg string)
}

// Stats summarizes one Scan call.
type Stats struct {
	Found         int
	Indexed       int
	Skipped       int
	Removed       int
	TripsUpserted int
	Duration      time.Duration
}

// segmentRecord is a fully-parsed segment, ready to write.
type segmentRecord struct {
	RelPath       string
	Camera        string
	Seq           int
	StartTime     time.Time
	EndTime       time.Time
	HasEndTime    bool
	DurationS     float64
	Bytes         int64
	Mtime         int64
	HasDetections bool
	HasGPS        bool
	Summary       DetectionSummary
	RawDetGZ      []byte
	GPSPoints     []GPSPoint
}

type existingSeg struct {
	ID    int64
	Bytes int64
	Mtime int64
}

// Scan walks rootPath (the SSD or a mounted share root), parses each
// segment's sidecars, and upserts segments / segment_summary /
// detections_raw / segment_gps rows for sourceID, skipping segments whose
// (bytes, mtime) already match the stored row unless opts.Full. Segments
// that exist in the database but no longer exist on disk are deleted
// (cascading to their dependent rows). Trips are rebuilt for every date
// touched by an add, change, or removal.
//
// Per-frame detections/det_objects/det_plates/det_vehicles rows and the
// search index are NOT populated here — call IndexDetections separately per
// segment (intended to be queued by the jobs system in M7); that keeps a
// full scan fast, deferring the expensive per-frame explosion.
func Scan(ctx context.Context, database *db.DB, sourceID int64, rootPath string, opts Opts) (Stats, error) {
	start := time.Now()
	var stats Stats

	found, err := walkRoot(rootPath)
	if err != nil {
		return stats, fmt.Errorf("scan: walk %s: %w", rootPath, err)
	}
	stats.Found = len(found)

	existing, err := loadExistingSegments(ctx, database, sourceID)
	if err != nil {
		return stats, fmt.Errorf("scan: load existing: %w", err)
	}

	foundByRel := make(map[string]struct{}, len(found))
	var toProcess []foundSegment
	affectedDates := map[string]time.Time{}
	for _, f := range found {
		foundByRel[f.RelPath] = struct{}{}
		if ex, ok := existing[f.RelPath]; ok && !opts.Full && ex.Bytes == f.Size && ex.Mtime == f.MtimeUX {
			stats.Skipped++
			continue
		}
		toProcess = append(toProcess, f)
		affectedDates[dateKey(f.SegPath.Date)] = f.SegPath.Date
	}

	var removed []string
	for rel := range existing {
		if _, ok := foundByRel[rel]; !ok {
			removed = append(removed, rel)
			if sp, err := ParseSegmentPath(rel); err == nil {
				affectedDates[dateKey(sp.Date)] = sp.Date
			}
		}
	}
	stats.Removed = len(removed)

	if opts.Progress != nil {
		opts.Progress(stats.Skipped, stats.Found, fmt.Sprintf(
			"scan: %d to process, %d unchanged, %d removed", len(toProcess), stats.Skipped, len(removed)))
	}

	jobs := make(chan foundSegment)
	results := make(chan segmentRecord)
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				rec, err := buildSegmentRecord(f)
				if err != nil {
					log.Printf("[scan] %s: %v", f.RelPath, err)
					continue
				}
				select {
				case results <- rec:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	go func() {
		defer close(jobs)
		for _, f := range toProcess {
			select {
			case jobs <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	done := stats.Skipped
	var writeErr error
	batch := make([]segmentRecord, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := writeSegmentBatch(ctx, database, sourceID, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rec := range results {
		if writeErr != nil {
			continue // drain so workers/producer don't block on a full channel
		}
		batch = append(batch, rec)
		done++
		stats.Indexed++
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				writeErr = err
			}
		}
		if opts.Progress != nil && done%200 == 0 {
			opts.Progress(done, stats.Found, "indexing segments")
		}
	}
	if writeErr == nil {
		writeErr = flush()
	}
	if writeErr != nil {
		return stats, fmt.Errorf("scan: write segments: %w", writeErr)
	}

	if len(removed) > 0 {
		if err := deleteSegments(ctx, database, sourceID, removed); err != nil {
			return stats, fmt.Errorf("scan: delete removed: %w", err)
		}
	}

	dates := make([]time.Time, 0, len(affectedDates))
	for _, d := range affectedDates {
		dates = append(dates, d)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	for _, d := range dates {
		n, err := rebuildTripsForDate(ctx, database, sourceID, d)
		if err != nil {
			return stats, fmt.Errorf("scan: rebuild trips %s: %w", dateKey(d), err)
		}
		stats.TripsUpserted += n
	}
	if opts.Progress != nil {
		opts.Progress(stats.Found, stats.Found, "scan complete")
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

func dateKey(t time.Time) string { return t.Format("20060102") }

// buildSegmentRecord stats and parses one segment's sidecars. It never
// fails outright for a missing/corrupt sidecar — a missing .timestamp.json
// falls back to an mtime-based heuristic (per the M4 brief) and missing
// .detections.json/.gps.json are simply treated as absent — only an
// unreadable mp4 stat (which can't happen here since walkRoot already
// captured it) or a cancelled context would abort processing of this file.
func buildSegmentRecord(f foundSegment) (segmentRecord, error) {
	base := strings.TrimSuffix(f.AbsPath, ".mp4")
	rec := segmentRecord{
		RelPath: f.RelPath,
		Camera:  f.SegPath.Camera,
		Seq:     f.SegPath.Seq,
		Bytes:   f.Size,
		Mtime:   f.MtimeUX,
	}

	if ts, err := ParseTimestamp(base + ".timestamp.json"); err == nil {
		rec.StartTime = ts.StartTime
		rec.EndTime = ts.EndTime
		rec.HasEndTime = true
		rec.DurationS = ts.EndTime.Sub(ts.StartTime).Seconds()
	} else {
		// No usable timestamp sidecar: fall back to the file's mtime as the
		// end time, assuming a ~30s segment (the recorder's actual segment
		// length) as a rough start time.
		end := time.Unix(f.MtimeUX, 0)
		rec.EndTime = end
		rec.StartTime = end.Add(-30 * time.Second)
		rec.HasEndTime = true
		rec.DurationS = 30
		log.Printf("[scan] %s: missing/unreadable timestamp sidecar (%v); using mtime heuristic", f.RelPath, err)
	}

	if raw, err := os.ReadFile(base + ".detections.json"); err == nil && len(raw) > 0 {
		if d, err := ParseDetectionsBytes(raw); err == nil {
			rec.HasDetections = true
			rec.Summary = d.Summary()
			if gz, err := gzipBytes(raw); err == nil {
				rec.RawDetGZ = gz
			} else {
				log.Printf("[scan] %s: gzip detections: %v", f.RelPath, err)
			}
		} else {
			log.Printf("[scan] %s: parse detections: %v", f.RelPath, err)
		}
	}

	if g, err := ParseGPS(base + ".gps.json"); err == nil {
		rec.HasGPS = true
		rec.GPSPoints = Decimate(g.Points, time.Second)
	}

	return rec, nil
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(gz []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func loadExistingSegments(ctx context.Context, database *db.DB, sourceID int64) (map[string]existingSeg, error) {
	rows, err := database.Read().QueryContext(ctx,
		`SELECT rel_path, id, bytes, COALESCE(mtime, 0) FROM segments WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]existingSeg{}
	for rows.Next() {
		var rel string
		var es existingSeg
		if err := rows.Scan(&rel, &es.ID, &es.Bytes, &es.Mtime); err != nil {
			return nil, err
		}
		out[rel] = es
	}
	return out, rows.Err()
}

// writeSegmentBatch upserts one transaction's worth of parsed segments:
// segments, segment_gps (replace), segment_summary, and detections_raw.
func writeSegmentBatch(ctx context.Context, database *db.DB, sourceID int64, batch []segmentRecord) error {
	return database.Tx(ctx, func(tx *sql.Tx) error {
		upsertSeg, err := tx.PrepareContext(ctx, `
			INSERT INTO segments (source_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, mtime, has_detections, has_gps, indexed_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
			ON CONFLICT(source_id, rel_path) DO UPDATE SET
				camera = excluded.camera, seq = excluded.seq, start_time = excluded.start_time,
				end_time = excluded.end_time, duration_s = excluded.duration_s, bytes = excluded.bytes,
				mtime = excluded.mtime, has_detections = excluded.has_detections, has_gps = excluded.has_gps,
				indexed_at = excluded.indexed_at
			RETURNING id`)
		if err != nil {
			return err
		}
		defer upsertSeg.Close()

		delGPS, err := tx.PrepareContext(ctx, `DELETE FROM segment_gps WHERE segment_id = ?`)
		if err != nil {
			return err
		}
		defer delGPS.Close()

		insGPS, err := tx.PrepareContext(ctx,
			`INSERT INTO segment_gps (segment_id, t, lat, lon, speed_mps, heading) VALUES (?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insGPS.Close()

		upSummary, err := tx.PrepareContext(ctx, `
			INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates, top_classes)
			VALUES (?,?,?,?,?,?,?,?,?)
			ON CONFLICT(segment_id) DO UPDATE SET
				n_car = excluded.n_car, n_truck = excluded.n_truck, n_bus = excluded.n_bus,
				n_person = excluded.n_person, n_bicycle = excluded.n_bicycle, n_motorcycle = excluded.n_motorcycle,
				n_plates = excluded.n_plates, top_classes = excluded.top_classes`)
		if err != nil {
			return err
		}
		defer upSummary.Close()

		upRaw, err := tx.PrepareContext(ctx, `
			INSERT INTO detections_raw (segment_id, gz) VALUES (?, ?)
			ON CONFLICT(segment_id) DO UPDATE SET gz = excluded.gz`)
		if err != nil {
			return err
		}
		defer upRaw.Close()

		for _, rec := range batch {
			var endTime, durS any
			if rec.HasEndTime {
				endTime = rec.EndTime.UTC().Format(time.RFC3339Nano)
				durS = rec.DurationS
			}

			var segID int64
			if err := upsertSeg.QueryRowContext(ctx,
				sourceID, rec.Camera, rec.RelPath, rec.Seq,
				rec.StartTime.UTC().Format(time.RFC3339Nano), endTime, durS,
				rec.Bytes, rec.Mtime, boolToInt(rec.HasDetections), boolToInt(rec.HasGPS),
			).Scan(&segID); err != nil {
				return fmt.Errorf("upsert segment %s: %w", rec.RelPath, err)
			}

			if _, err := delGPS.ExecContext(ctx, segID); err != nil {
				return fmt.Errorf("clear gps %s: %w", rec.RelPath, err)
			}
			for _, p := range rec.GPSPoints {
				var heading any
				if p.Heading != nil {
					heading = *p.Heading
				}
				if _, err := insGPS.ExecContext(ctx, segID, p.T.UTC().Format(time.RFC3339Nano), p.Lat, p.Lon, p.SpeedMps, heading); err != nil {
					return fmt.Errorf("insert gps %s: %w", rec.RelPath, err)
				}
			}

			if rec.HasDetections {
				s := rec.Summary
				topJSON, _ := json.Marshal(s.TopClasses)
				if _, err := upSummary.ExecContext(ctx, segID,
					s.ClassCounts["car"], s.ClassCounts["truck"], s.ClassCounts["bus"],
					s.ClassCounts["person"], s.ClassCounts["bicycle"], s.ClassCounts["motorcycle"],
					s.PlateCount, string(topJSON),
				); err != nil {
					return fmt.Errorf("upsert summary %s: %w", rec.RelPath, err)
				}
				if rec.RawDetGZ != nil {
					if _, err := upRaw.ExecContext(ctx, segID, rec.RawDetGZ); err != nil {
						return fmt.Errorf("upsert raw %s: %w", rec.RelPath, err)
					}
				}
			}
		}
		return nil
	})
}

// deleteSegments removes rows for rel paths no longer present on disk, in
// chunks (SQLite has a default parameter limit well above these chunk
// sizes, but chunking keeps a single statement modest regardless). Deletes
// cascade to segment_gps, segment_summary, detections_raw, detections
// (and transitively det_objects/det_plates/det_vehicles/search_terms).
func deleteSegments(ctx context.Context, database *db.DB, sourceID int64, rels []string) error {
	const chunk = 500
	for i := 0; i < len(rels); i += chunk {
		end := i + chunk
		if end > len(rels) {
			end = len(rels)
		}
		part := rels[i:end]
		if err := database.Tx(ctx, func(tx *sql.Tx) error {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
			args := make([]any, 0, len(part)+1)
			args = append(args, sourceID)
			for _, r := range part {
				args = append(args, r)
			}
			_, err := tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM segments WHERE source_id = ? AND rel_path IN (%s)`, placeholders), args...)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// rebuildTripsForDate re-pairs every front/rear recording on date (from
// this source's current segments rows) and upserts the resulting trips,
// deleting any stale trip for that date that no longer corresponds to a
// current pairing. Returns the number of trips upserted.
func rebuildTripsForDate(ctx context.Context, database *db.DB, sourceID int64, date time.Time) (int, error) {
	datePath := date.Format("2006/01/02")
	dateKeyStr := date.Format("20060102")

	rows, err := database.Read().QueryContext(ctx,
		`SELECT id, camera, rel_path, start_time, end_time, bytes FROM segments
		 WHERE source_id = ? AND (rel_path LIKE ? OR rel_path LIKE ?)`,
		sourceID, "front/"+datePath+"/%", "rear/"+datePath+"/%")
	if err != nil {
		return 0, err
	}

	recsByKey := map[string]*Recording{}
	for rows.Next() {
		var id int64
		var camera, relPath, startRaw string
		var endRaw sql.NullString
		var bytesN int64
		if err := rows.Scan(&id, &camera, &relPath, &startRaw, &endRaw, &bytesN); err != nil {
			rows.Close()
			return 0, err
		}
		sp, err := ParseSegmentPath(relPath)
		if err != nil {
			continue
		}
		key := camera + "|" + sp.RecordingKey
		r, ok := recsByKey[key]
		if !ok {
			r = &Recording{Camera: camera, Key: sp.RecordingKey}
			recsByKey[key] = r
		}
		start, _ := time.Parse(time.RFC3339Nano, startRaw)
		end := start
		if endRaw.Valid && endRaw.String != "" {
			if t, err := time.Parse(time.RFC3339Nano, endRaw.String); err == nil {
				end = t
			}
		}
		if r.Start.IsZero() || start.Before(r.Start) {
			r.Start = start
		}
		if end.After(r.End) {
			r.End = end
		}
		r.Bytes += bytesN
		r.SegmentIDs = append(r.SegmentIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var fronts, rears []Recording
	for _, r := range recsByKey {
		if r.Camera == "front" {
			fronts = append(fronts, *r)
		} else {
			rears = append(rears, *r)
		}
	}
	sort.Slice(fronts, func(i, j int) bool { return fronts[i].Start.Before(fronts[j].Start) })
	sort.Slice(rears, func(i, j int) bool { return rears[i].Start.Before(rears[j].Start) })

	pairs := PairRecordings(fronts, rears)

	n := 0
	err = database.Tx(ctx, func(tx *sql.Tx) error {
		if len(pairs) == 0 {
			_, err := tx.ExecContext(ctx,
				`DELETE FROM trips WHERE source_id = ? AND trip_key LIKE ?`, sourceID, "recording_"+dateKeyStr+"_%")
			return err
		}

		keys := make([]any, 0, len(pairs))
		for _, p := range pairs {
			keys = append(keys, p.TripKey)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
		args := append([]any{sourceID, "recording_" + dateKeyStr + "_%"}, keys...)
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM trips WHERE source_id = ? AND trip_key LIKE ? AND trip_key NOT IN (%s)`, placeholders),
			args...); err != nil {
			return err
		}

		upsertTrip, err := tx.PrepareContext(ctx, `
			INSERT INTO trips (source_id, trip_key, started_at, ended_at, duration_s, front_dir, rear_dir, pair_offset_s, segment_count, bytes, distance_m, max_speed_mps, start_lat, start_lon, end_lat, end_lon)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(source_id, trip_key) DO UPDATE SET
				started_at = excluded.started_at, ended_at = excluded.ended_at, duration_s = excluded.duration_s,
				front_dir = excluded.front_dir, rear_dir = excluded.rear_dir, pair_offset_s = excluded.pair_offset_s,
				segment_count = excluded.segment_count, bytes = excluded.bytes, distance_m = excluded.distance_m,
				max_speed_mps = excluded.max_speed_mps, start_lat = excluded.start_lat, start_lon = excluded.start_lon,
				end_lat = excluded.end_lat, end_lon = excluded.end_lon
			RETURNING id`)
		if err != nil {
			return err
		}
		defer upsertTrip.Close()

		for _, p := range pairs {
			agg, err := tripGPSAggregate(ctx, tx, p)
			if err != nil {
				return fmt.Errorf("gps aggregate %s: %w", p.TripKey, err)
			}

			var frontDir, rearDir, offset any
			if p.FrontDir != "" {
				frontDir = p.FrontDir
			}
			if p.RearDir != "" {
				rearDir = p.RearDir
			}
			if p.PairOffsetS != nil {
				offset = *p.PairOffsetS
			}

			var tripID int64
			if err := upsertTrip.QueryRowContext(ctx,
				sourceID, p.TripKey,
				p.Started.UTC().Format(time.RFC3339Nano), p.Ended.UTC().Format(time.RFC3339Nano), p.DurationS,
				frontDir, rearDir, offset, p.SegmentCount(), p.Bytes,
				agg.distance, agg.maxSpeed, agg.startLat, agg.startLon, agg.endLat, agg.endLon,
			).Scan(&tripID); err != nil {
				return fmt.Errorf("upsert trip %s: %w", p.TripKey, err)
			}

			ids := p.AllSegmentIDs()
			if len(ids) > 0 {
				placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
				args := make([]any, 0, len(ids)+1)
				args = append(args, tripID)
				for _, id := range ids {
					args = append(args, id)
				}
				if _, err := tx.ExecContext(ctx,
					fmt.Sprintf(`UPDATE segments SET trip_id = ? WHERE id IN (%s)`, placeholders), args...); err != nil {
					return fmt.Errorf("link trip segments %s: %w", p.TripKey, err)
				}
			}
			n++
		}
		return nil
	})
	return n, err
}

// gpsAgg holds nullable (any, so sql driver sees NULL when unset) trip-level
// GPS aggregates.
type gpsAgg struct {
	distance, maxSpeed, startLat, startLon, endLat, endLon any
}

// tripGPSAggregate aggregates segment_gps points for a pair's segments
// (preferring the front camera's points when both exist, since front/rear
// usually share one GPS source and summing both would double-count) into
// start/end lat-lon, max speed, and a haversine-summed distance.
func tripGPSAggregate(ctx context.Context, tx *sql.Tx, p Pair) (gpsAgg, error) {
	ids := p.FrontSegmentIDs
	if len(ids) == 0 {
		ids = p.RearSegmentIDs
	}
	if len(ids) == 0 {
		return gpsAgg{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT lat, lon, speed_mps FROM segment_gps WHERE segment_id IN (%s) ORDER BY t`, placeholders), args...)
	if err != nil {
		return gpsAgg{}, err
	}
	defer rows.Close()

	type pt struct{ lat, lon, speed float64 }
	var pts []pt
	var maxSpeed float64
	for rows.Next() {
		var lat, lon float64
		var speed sql.NullFloat64
		if err := rows.Scan(&lat, &lon, &speed); err != nil {
			return gpsAgg{}, err
		}
		pts = append(pts, pt{lat, lon, speed.Float64})
		if speed.Valid && speed.Float64 > maxSpeed {
			maxSpeed = speed.Float64
		}
	}
	if err := rows.Err(); err != nil {
		return gpsAgg{}, err
	}
	if len(pts) == 0 {
		return gpsAgg{}, nil
	}

	var dist float64
	for i := 1; i < len(pts); i++ {
		dist += haversineM(pts[i-1].lat, pts[i-1].lon, pts[i].lat, pts[i].lon)
	}
	return gpsAgg{
		distance: dist, maxSpeed: maxSpeed,
		startLat: pts[0].lat, startLon: pts[0].lon,
		endLat: pts[len(pts)-1].lat, endLon: pts[len(pts)-1].lon,
	}, nil
}

// IndexDetections explodes a segment's stored raw detections (detections_raw)
// into per-frame detections/det_objects/det_plates/det_vehicles rows and
// search_terms entries (kind "class"/"plate"/"make") for full-text search.
// It's deliberately separate from Scan — a full scan stores only the
// gzipped raw JSON and a per-segment summary; this expensive per-frame
// explosion is meant to run as a lower-priority background job (M7). Safe
// to call more than once for the same segment: it clears any previous
// explosion first. A no-op (nil error) if the segment has no detections_raw
// row.
func IndexDetections(ctx context.Context, database *db.DB, segmentID int64) error {
	var gz []byte
	err := database.Read().QueryRowContext(ctx,
		`SELECT gz FROM detections_raw WHERE segment_id = ?`, segmentID).Scan(&gz)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan: index detections %d: read raw: %w", segmentID, err)
	}

	raw, err := gunzipBytes(gz)
	if err != nil {
		return fmt.Errorf("scan: index detections %d: gunzip: %w", segmentID, err)
	}
	d, err := ParseDetectionsBytes(raw)
	if err != nil {
		return fmt.Errorf("scan: index detections %d: parse: %w", segmentID, err)
	}

	return database.Tx(ctx, func(tx *sql.Tx) error {
		// Cascades to det_objects/det_plates/det_vehicles and (via the
		// external-content FTS triggers) search_terms/search_fts.
		if _, err := tx.ExecContext(ctx, `DELETE FROM detections WHERE segment_id = ?`, segmentID); err != nil {
			return err
		}

		insDet, err := tx.PrepareContext(ctx,
			`INSERT INTO detections (segment_id, pts_offset, wall_time, frame_index, lat, lon, speed_mps) VALUES (?,?,?,?,?,?,?) RETURNING id`)
		if err != nil {
			return err
		}
		defer insDet.Close()
		insObj, err := tx.PrepareContext(ctx,
			`INSERT INTO det_objects (det_id, class, confidence, x1, y1, x2, y2) VALUES (?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insObj.Close()
		insPlate, err := tx.PrepareContext(ctx,
			`INSERT INTO det_plates (det_id, text, confidence, x1, y1, x2, y2) VALUES (?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insPlate.Close()
		insVeh, err := tx.PrepareContext(ctx,
			`INSERT INTO det_vehicles (det_id, type, make, confidence, x1, y1, x2, y2) VALUES (?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insVeh.Close()
		insTerm, err := tx.PrepareContext(ctx,
			`INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?,?,?,?)`)
		if err != nil {
			return err
		}
		defer insTerm.Close()

		for _, frame := range d.DetectionFrames {
			var lat, lon, speed, wallTime any
			if frame.GPS != nil {
				lat, lon, speed = frame.GPS.Lat, frame.GPS.Lon, frame.GPS.SpeedMps
			}
			if !frame.WallTimeT.IsZero() {
				wallTime = frame.WallTimeT.UTC().Format(time.RFC3339Nano)
			}

			var detID int64
			if err := insDet.QueryRowContext(ctx, segmentID, frame.PTSOffset, wallTime, frame.FrameIndex, lat, lon, speed).Scan(&detID); err != nil {
				return fmt.Errorf("insert detection frame: %w", err)
			}

			for _, o := range frame.Objects {
				if _, err := insObj.ExecContext(ctx, detID, o.Class, o.Confidence, o.Norm.X1, o.Norm.Y1, o.Norm.X2, o.Norm.Y2); err != nil {
					return fmt.Errorf("insert object: %w", err)
				}
				if o.Class != "" {
					if _, err := insTerm.ExecContext(ctx, segmentID, detID, "class", o.Class); err != nil {
						return fmt.Errorf("insert class term: %w", err)
					}
				}
			}
			for _, pl := range frame.Plates {
				if _, err := insPlate.ExecContext(ctx, detID, pl.Text, pl.Confidence, pl.Norm.X1, pl.Norm.Y1, pl.Norm.X2, pl.Norm.Y2); err != nil {
					return fmt.Errorf("insert plate: %w", err)
				}
				if pl.Text != "" {
					if _, err := insTerm.ExecContext(ctx, segmentID, detID, "plate", pl.Text); err != nil {
						return fmt.Errorf("insert plate term: %w", err)
					}
				}
			}
			for _, v := range frame.Vehicles {
				var make_ any
				if v.Make != "" {
					make_ = v.Make
				}
				if _, err := insVeh.ExecContext(ctx, detID, v.Type, make_, v.TypeConfidence, v.Norm.X1, v.Norm.Y1, v.Norm.X2, v.Norm.Y2); err != nil {
					return fmt.Errorf("insert vehicle: %w", err)
				}
				if v.Make != "" {
					if _, err := insTerm.ExecContext(ctx, segmentID, detID, "make", v.Make); err != nil {
						return fmt.Errorf("insert make term: %w", err)
					}
				}
			}
		}
		return nil
	})
}
