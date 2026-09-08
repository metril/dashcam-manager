// Command api serves the dashcam-manager JSON API and the embedded React SPA.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"dashcam/internal/config"
	"dashcam/internal/db"
)

// version is set by build tooling (ldflags); defaults to "dev" otherwise.
var version = "dev"

//go:embed all:dist
var distFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags)

	cfg := config.Load()

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

	mux := http.NewServeMux()
	registerRoutes(mux, cfg, store)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Println("[api] shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[api] shutdown error: %v", err)
	}
}

func registerRoutes(mux *http.ServeMux, cfg config.Config, store *db.DB) {
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/settings", handleSettingsGet(store))
	mux.HandleFunc("PUT /api/settings", handleSettingsPut(store))
	mux.Handle("/", staticHandler())
	_ = cfg // reserved for handlers added in later milestones
}

// seedDefaultSettings inserts the initial settings values derived from
// environment config on first boot only (SettingsSeedDefaults never
// overwrites a key a user has already changed via PUT /api/settings).
func seedDefaultSettings(ctx context.Context, store *db.DB, cfg config.Config) error {
	workers, err := strconv.Atoi(cfg.Workers)
	if err != nil {
		workers = 4
	}
	defaults := map[string]json.RawMessage{
		"ssd_path":        mustJSON(cfg.SSDPath),
		"qsv_device":      mustJSON(cfg.QSVDevice),
		"max_ffmpeg_jobs": mustJSON(2),
		"workers":         mustJSON(workers),
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

// staticHandler serves the embedded SPA build, falling back to index.html for
// any path that isn't a real static asset and isn't under /api — i.e.
// client-side routes.
func staticHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("[api] embedded dist: %v", err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "."
		}
		if _, err := fs.Stat(sub, name); err != nil {
			r = cloneRequestPath(r, "/")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func cloneRequestPath(r *http.Request, path string) *http.Request {
	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	return r2
}
