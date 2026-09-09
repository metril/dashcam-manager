package db

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// Task 2's performance target: search must stay fast against a library the
// size of a long-lived real device — tens of thousands of segments, each
// with several detections. seedSearchBenchDB builds one, sized by
// numSegments/detsPerSegment, using a single transaction with prepared
// statements (a naive per-row-implicit-transaction insert of this many rows
// would dominate the benchmark's own setup time far more than the searches
// being measured).
//
// Scale: 20,000 segments x ~10 detections/segment = ~200,000 detections
// (~1M total rows once det_objects/det_plates/det_vehicles/search_terms are
// included) — a scaled-down stand-in for the brief's "~50k segments/500k
// detections" so `go test -bench` finishes in a reasonable time; see
// task-2-report.md for a from-scratch, full-scale timing run against a
// real production database copy, which is what actually validates the
// change against realistic data (this benchmark's job is a repeatable
// regression guard, not the primary evidence).
const (
	benchNumSegments     = 20000
	benchDetsPerSegment  = 10
	benchClasses         = "car,truck,bus,person,bicycle,motorcycle"
	benchMakes           = "Toyota,Ford,Honda,Chevrolet,Nissan,BMW"
	benchPlateAlphabet   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	benchTargetClass     = "car"
	benchTargetMake      = "Toyota"
	benchTargetPlateTerm = "XYZ12" // seeded onto a known subset of plates
)

var benchClassList = []string{"car", "truck", "bus", "person", "bicycle", "motorcycle"}
var benchMakeList = []string{"Toyota", "Ford", "Honda", "Chevrolet", "Nissan", "BMW"}

func randPlate(r *rand.Rand) string {
	b := make([]byte, 6)
	for i := range b {
		b[i] = benchPlateAlphabet[r.Intn(len(benchPlateAlphabet))]
	}
	return string(b)
}

// seedSearchBenchDB builds a large, realistically-shaped search fixture
// (same table shapes as seedSearchFixture, at scale) in a fresh on-disk
// database and returns it. Roughly every 7th detection carries the
// benchmark's target plate substring, and det_objects.track_id is
// populated for ~80% of object/plate rows to exercise the det_tracks
// chip-collapsing path added in task 2 (the other 20% simulates
// not-yet-backfilled data).
func seedSearchBenchDB(b *testing.B) *DB {
	b.Helper()
	dir := b.TempDir()
	d, err := Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()
	if err := d.InitSchema(ctx); err != nil {
		b.Fatalf("InitSchema: %v", err)
	}

	r := rand.New(rand.NewSource(1))

	tx, err := d.Write().BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `INSERT INTO sources (kind, root_path, label) VALUES ('ssd', '/mnt/dashcam', 'SSD')`)
	if err != nil {
		b.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	insTrip := mustPrepare(ctx, tx, b, `INSERT INTO trips (source_id, trip_key, started_at, front_dir, segment_count, bytes) VALUES (?, ?, ?, ?, 1, 1000)`)
	insSeg := mustPrepare(ctx, tx, b, `INSERT INTO segments (source_id, trip_id, camera, rel_path, seq, start_time, end_time, duration_s, bytes, has_detections, has_gps) VALUES (?, ?, 'front', ?, 0, ?, ?, 30, 1000, 1, 0)`)
	insDet := mustPrepare(ctx, tx, b, `INSERT INTO detections (segment_id, pts_offset, speed_mps) VALUES (?, ?, ?)`)
	insTrack := mustPrepare(ctx, tx, b, `INSERT INTO det_tracks (segment_id, kind, class, first_pts, last_pts, n_frames, best_conf, best_text, x1, y1, x2, y2) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0.1, 0.1, 0.5, 0.5)`)
	insObj := mustPrepare(ctx, tx, b, `INSERT INTO det_objects (det_id, class, confidence, track_id, x1, y1, x2, y2) VALUES (?, ?, ?, ?, 0.1, 0.1, 0.5, 0.5)`)
	insPlate := mustPrepare(ctx, tx, b, `INSERT INTO det_plates (det_id, text, confidence, track_id, x1, y1, x2, y2) VALUES (?, ?, ?, ?, 0.2, 0.2, 0.4, 0.4)`)
	insVeh := mustPrepare(ctx, tx, b, `INSERT INTO det_vehicles (det_id, type, make, confidence, track_id, x1, y1, x2, y2) VALUES (?, ?, ?, ?, ?, 0.1, 0.1, 0.5, 0.5)`)
	insTerm := mustPrepare(ctx, tx, b, `INSERT INTO search_terms (segment_id, det_id, kind, text) VALUES (?, ?, ?, ?)`)
	insSummary := mustPrepare(ctx, tx, b, `INSERT INTO segment_summary (segment_id, n_car, n_truck, n_bus, n_person, n_bicycle, n_motorcycle, n_plates) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)

	for s := 0; s < benchNumSegments; s++ {
		tripKey := fmt.Sprintf("recording_bench_%06d", s)
		startTime := fmt.Sprintf("2026-01-01T%02d:%02d:%02d.%09dZ", (s/3600)%24, (s/60)%60, s%60, 0)
		tRes, err := insTrip.ExecContext(ctx, sourceID, tripKey, startTime, tripKey)
		if err != nil {
			b.Fatalf("insert trip: %v", err)
		}
		tripID, _ := tRes.LastInsertId()
		relPath := fmt.Sprintf("front/2026/01/01/%s/segment_00000.mp4", tripKey)
		sRes, err := insSeg.ExecContext(ctx, sourceID, tripID, relPath, startTime, startTime)
		if err != nil {
			b.Fatalf("insert segment: %v", err)
		}
		segID, _ := sRes.LastInsertId()

		classCounts := map[string]int{}
		plateCount := 0

		// A shared track per segment for the target class, so ~80% of its
		// frames collapse into one chip (task 2's grouping path).
		var trackID int64 = -1
		useTrack := r.Float64() < 0.8
		if useTrack {
			tRes, err := insTrack.ExecContext(ctx, segID, "object", benchTargetClass, 0.0, float64(benchDetsPerSegment), benchDetsPerSegment, 0.9, nil)
			if err != nil {
				b.Fatalf("insert track: %v", err)
			}
			trackID, _ = tRes.LastInsertId()
		}

		for i := 0; i < benchDetsPerSegment; i++ {
			pts := float64(i)
			speed := r.Float64() * 30
			dRes, err := insDet.ExecContext(ctx, segID, pts, speed)
			if err != nil {
				b.Fatalf("insert detection: %v", err)
			}
			detID, _ := dRes.LastInsertId()

			class := benchClassList[r.Intn(len(benchClassList))]
			conf := 0.5 + r.Float64()*0.5
			var tid any
			if class == benchTargetClass && useTrack {
				tid = trackID
			}
			if _, err := insObj.ExecContext(ctx, detID, class, conf, tid); err != nil {
				b.Fatalf("insert object: %v", err)
			}
			classCounts[class]++
			if _, err := insTerm.ExecContext(ctx, segID, detID, "class", class); err != nil {
				b.Fatalf("insert term: %v", err)
			}

			make_ := benchMakeList[r.Intn(len(benchMakeList))]
			if _, err := insVeh.ExecContext(ctx, detID, "sedan", make_, conf, nil); err != nil {
				b.Fatalf("insert vehicle: %v", err)
			}
			if _, err := insTerm.ExecContext(ctx, segID, detID, "make", make_); err != nil {
				b.Fatalf("insert term: %v", err)
			}

			// Roughly 1 in 7 detections carries a plate; every 7th of
			// those (~1 in 49 overall) carries the benchmark's target
			// substring so the plate-search benchmark has real matches
			// to find without matching everything.
			if i%7 == 0 {
				var plate string
				if plateCount%7 == 0 {
					plate = benchTargetPlateTerm + randPlate(r)[:3]
				} else {
					plate = randPlate(r)
				}
				if _, err := insPlate.ExecContext(ctx, detID, plate, conf, nil); err != nil {
					b.Fatalf("insert plate: %v", err)
				}
				if _, err := insTerm.ExecContext(ctx, segID, detID, "plate", plate); err != nil {
					b.Fatalf("insert term: %v", err)
				}
				plateCount++
			}
		}

		if _, err := insSummary.ExecContext(ctx, segID,
			classCounts["car"], classCounts["truck"], classCounts["bus"],
			classCounts["person"], classCounts["bicycle"], classCounts["motorcycle"],
			plateCount); err != nil {
			b.Fatalf("insert summary: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return d
}

func mustPrepare(ctx context.Context, tx *sql.Tx, b *testing.B, query string) *sql.Stmt {
	b.Helper()
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		b.Fatalf("prepare %q: %v", query, err)
	}
	return stmt
}

// benchDB seeds the fixture once per benchmark binary run (each Benchmark*
// function below calls this, but testing.B's own re-run/-count machinery
// means it's still worth guarding — b.N loops re-run the timed body, not
// the whole test binary, so this only actually seeds once per Benchmark*
// call regardless).
func benchDB(b *testing.B) (*DB, context.Context) {
	d := seedSearchBenchDB(b)
	return d, context.Background()
}

// BenchmarkSearchClassOnly exercises the segment_summary fast path.
func BenchmarkSearchClassOnly(b *testing.B) {
	d, ctx := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := d.Search(ctx, SearchFilter{Classes: []string{benchTargetClass}}); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

// BenchmarkSearchQAndClass exercises searchQAndClassFastPath.
func BenchmarkSearchQAndClass(b *testing.B) {
	d, ctx := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := d.Search(ctx, SearchFilter{Q: benchTargetMake, Classes: []string{benchTargetClass}}); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

// BenchmarkSearchMake exercises the general correlated-EXISTS path (make
// alone isn't covered by any fast path).
func BenchmarkSearchMake(b *testing.B) {
	d, ctx := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := d.Search(ctx, SearchFilter{Make: benchTargetMake}); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

// BenchmarkSearchPlateSubstring exercises the general correlated-EXISTS
// path for a plate-only filter (fix round 1 removed the dedicated
// plate-only fast path; see task-2-report.md).
func BenchmarkSearchPlateSubstring(b *testing.B) {
	d, ctx := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := d.Search(ctx, SearchFilter{Plate: benchTargetPlateTerm}); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

// BenchmarkSearchMinConf exercises the bare min_conf OR'd three-way EXISTS
// path (no structural filter to attach it to).
func BenchmarkSearchMinConf(b *testing.B) {
	d, ctx := benchDB(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := d.Search(ctx, SearchFilter{MinConf: 0.9}); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}
