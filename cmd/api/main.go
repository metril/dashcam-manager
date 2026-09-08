// Command api serves the dashcam-manager JSON API and the embedded React SPA.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/scan"
)

// version is set by build tooling (ldflags); defaults to "dev" otherwise.
var version = "dev"

//go:embed all:dist
var distFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags)

	scanPath := flag.String("scan", "", "scan this root path once, print stats, and exit (for benchmarking; does not start the HTTP server)")
	scanFull := flag.Bool("full", false, "with -scan, force re-processing of every segment instead of skipping unchanged ones")
	flag.Parse()

	cfg := config.Load()
	cfg.Validate()

	store, err := db.Open(cfg.DB)
	if err != nil {
		log.Fatalf("[api] open db: %v", err)
	}
	defer store.Close()

	initCtx, cancelInit := context.WithTimeout(context.Background(), 30*time.Second)
	if err := store.InitSchema(initCtx); err != nil {
		cancelInit()
		log.Fatalf("[api] init schema: %v", err)
	}
	if err := seedDefaultSettings(initCtx, store, cfg); err != nil {
		cancelInit()
		log.Fatalf("[api] seed settings: %v", err)
	}
	cancelInit()

	if *scanPath != "" {
		runCLIScan(store, *scanPath, *scanFull)
		return
	}

	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := store.SourceEnsure(seedCtx, "ssd", cfg.SSDPath, "SSD"); err != nil {
		log.Printf("[api] seed ssd source: %v", err)
	}
	cancelSeed()

	// Created before the job queue and registerRoutes so long-running
	// background work (job workers, SSE connections) derives its context
	// from ctx and is cancelled on shutdown rather than outliving the
	// process's intent to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	broker := newBroker()
	queue := jobs.New(store)
	queue.OnEvent(broker.publishJob)
	registerJobHandlers(queue, store)

	workers := cfg.Workers
	if raw, err := store.SettingsGet(ctx, "workers"); err == nil {
		var w int
		if json.Unmarshal(raw, &w) == nil && w > 0 {
			workers = w
		}
	}
	if err := queue.Start(ctx, workers); err != nil {
		log.Fatalf("[api] start job queue: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, cfg, store, queue, broker)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[api] listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[api] server error: %v", err)
		}
	}()

	<-ctx.Done()

	log.Println("[api] shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[api] shutdown error: %v", err)
	}
}

func registerRoutes(mux *http.ServeMux, cfg config.Config, store *db.DB, queue *jobs.Queue, b *broker) {
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/settings", handleSettingsGet(store))
	mux.HandleFunc("PUT /api/settings", handleSettingsPut(store))
	mux.HandleFunc("GET /api/sources", handleSourcesGet(store, queue))
	mux.HandleFunc("POST /api/sources", handleSourcesPost(store))
	mux.HandleFunc("POST /api/sources/scan", handleSourcesScan(store, queue))
	mux.HandleFunc("GET /api/calendar", handleCalendarGet(store))
	mux.HandleFunc("GET /api/trips", handleTripsGet(store))
	mux.HandleFunc("GET /api/trips/{id}", handleTripGet(store))
	mux.HandleFunc("GET /api/segments/{id}", handleSegmentGet(store))
	mux.HandleFunc("GET /api/segments/{id}/detections", handleSegmentDetectionsGet(store))
	mux.HandleFunc("GET /api/stats", handleStatsGet(store))
	mux.HandleFunc("GET /api/events", handleEventsGet(b))
	mux.HandleFunc("GET /api/jobs", handleJobsGet(queue))
	mux.HandleFunc("GET /api/jobs/{id}", handleJobGet(queue))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", handleJobCancel(queue))
	mux.HandleFunc("DELETE /api/jobs/{id}", handleJobDelete(queue))
	mux.Handle("/", staticHandler())
	_ = cfg // reserved for handlers added in later milestones
}

// runCLIScan runs a single synchronous scan of rootPath for benchmarking
// (`go run ./cmd/api -scan /mnt/dashcam`), printing stats to stdout, and
// exits without starting the HTTP server. It ensures a source row exists
// for rootPath (creating an "ssd" source if needed) so segments/trips land
// under a real source_id.
func runCLIScan(store *db.DB, rootPath string, full bool) {
	ctx := context.Background()
	sourceID, err := store.SourceEnsure(ctx, "ssd", rootPath, "CLI scan")
	if err != nil {
		log.Fatalf("[scan] ensure source: %v", err)
	}

	progress := func(done, total int, msg string) {
		log.Printf("[scan] %d/%d %s", done, total, msg)
	}
	stats, err := scan.Scan(ctx, store, sourceID, rootPath, scan.Opts{Full: full, Progress: progress})
	if err != nil {
		log.Fatalf("[scan] scan: %v", err)
	}
	if n, err := scan.EnrichFromLegacyDB(ctx, store, sourceID, rootPath); err != nil {
		log.Printf("[scan] legacy db enrichment: %v", err)
	} else if n > 0 {
		log.Printf("[scan] legacy db enrichment: %d segments", n)
	}

	fmt.Printf("found=%d indexed=%d skipped=%d removed=%d trips_upserted=%d duration=%s\n",
		stats.Found, stats.Indexed, stats.Skipped, stats.Removed, stats.TripsUpserted, stats.Duration)
}

// seedDefaultSettings inserts the initial settings values derived from
// environment config on first boot only (SettingsSeedDefaults never
// overwrites a key a user has already changed via PUT /api/settings).
func seedDefaultSettings(ctx context.Context, store *db.DB, cfg config.Config) error {
	defaults := map[string]json.RawMessage{
		"ssd_path":        mustJSON(cfg.SSDPath),
		"qsv_device":      mustJSON(cfg.QSVDevice),
		"max_ffmpeg_jobs": mustJSON(2),
		"workers":         mustJSON(cfg.Workers),
	}
	return store.SettingsSeedDefaults(ctx, defaults)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustJSON: %v", err))
	}
	return b
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version,
	})
}

// staticHandler serves the embedded SPA build.
func staticHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("[api] embedded dist: %v", err)
	}
	return spaHandler(sub)
}

// spaHandler serves static files out of root, falling back to index.html
// for any path that isn't a real static asset and isn't under /api (i.e.
// client-side routes), refusing to list directory contents, and returning a
// JSON 404 for unmatched /api/* paths that reach it (registerRoutes should
// already handle every real API route, so this only fires for typos/removed
// endpoints). If index.html itself is missing — e.g. a checkout where the
// frontend hasn't been built yet — every non-API request gets a 503 rather
// than a confusing empty response.
func spaHandler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSONNotFound(w)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			// Root always serves the app shell directly (never a directory
			// listing, even of an empty tree).
			serveIndex(w, root)
			return
		}

		info, statErr := fs.Stat(root, name)
		switch {
		case statErr != nil:
			// Not a real file — SPA client-side route (or the file was
			// removed/renamed); fall back to the app shell.
			serveIndex(w, root)
			return
		case info.IsDir():
			// Never expose directory listings (e.g. /assets/); fall back to
			// the app shell instead.
			serveIndex(w, root)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/assets/") {
			// Vite fingerprints these filenames by content hash, so a given
			// URL's content never changes — safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, root fs.FS) {
	f, err := root.Open("index.html")
	if err != nil {
		http.Error(w, `{"error":"service unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("[api] serve index.html: %v", err)
	}
}

func writeJSONNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
}
