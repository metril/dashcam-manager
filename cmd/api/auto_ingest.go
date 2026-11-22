package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"dashcam/internal/db"
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
)

// autoIngestShareID reads settings.ssd_ingest_target_share_id (0 = off).
func autoIngestShareID(ctx context.Context, store *db.DB) int64 {
	raw, err := store.SettingsGet(ctx, "ssd_ingest_target_share_id")
	if err != nil {
		return 0
	}
	var id int64
	if json.Unmarshal(raw, &id) != nil || id < 0 {
		return 0
	}
	return id
}

// autoIngestMu serializes the active-ingest check and the enqueue.
var autoIngestMu sync.Mutex

// maybeEnqueueAutoIngest enqueues an ingest of sourceID into the
// configured auto-ingest share unless the setting is off, the share is
// unavailable, or an ingest for that source is already queued or running.
// Cleanup is never chained here.
func maybeEnqueueAutoIngest(ctx context.Context, store *db.DB, q *jobs.Queue, sourceID int64) {
	shareID := autoIngestShareID(ctx, store)
	if shareID == 0 || q == nil {
		return
	}
	if _, err := ingest.ResolveTargetMount(ctx, store, shareID); err != nil {
		log.Printf("[jobs] auto-ingest: target share %d unavailable: %v", shareID, err)
		return
	}
	autoIngestMu.Lock()
	defer autoIngestMu.Unlock()
	active, err := activeJobsBySourceOfType(ctx, q, map[string]bool{"ingest": true})
	if err != nil {
		log.Printf("[jobs] auto-ingest: active jobs: %v", err)
		return
	}
	if _, busy := active[sourceID]; busy {
		return
	}
	if _, err := q.Enqueue(ctx, "ingest", ingestJobParams{
		SourceID: sourceID, TargetShareID: shareID, Verify: ingest.VerifySHA256,
	}, 0); err != nil {
		log.Printf("[jobs] auto-ingest: enqueue: %v", err)
	}
}
