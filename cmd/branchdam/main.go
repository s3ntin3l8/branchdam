// Command branchdam runs the branchDAM server: it indexes configured storage
// locations into a version node graph, resolves lineage edges between them,
// and serves the SPA + REST/SSE API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/httpapi"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
	"github.com/s3ntin3l8/branchdam/web"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for local builds (see Dockerfile, added in PR 11).
var version = "dev"

func main() {
	cfgPath := flag.String("config", envOr("BRANCHDAM_CONFIG", "config.yaml"), "path to config file")
	debug := flag.Bool("debug", os.Getenv("BRANCHDAM_DEBUG") != "", "enable debug logging")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit (for container HEALTHCHECK)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).Error("load config", "err", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel, *debug)}))

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.ListenAddr))
	}

	log.Info("loaded config", "version", version, "listenAddr", cfg.ListenAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.Database.Path)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Error("close database", "err", err)
		}
	}()

	if err := reconcileOrphanedScanJobs(ctx, database, log); err != nil {
		log.Error("reconcile orphaned scan jobs", "err", err)
		os.Exit(1)
	}

	if err := seedStorageLocations(ctx, database, cfg.StorageLocations); err != nil {
		log.Error("seed storage locations", "err", err)
		os.Exit(1)
	}

	guard, err := storage.LoadGuard(ctx, database)
	if err != nil {
		log.Error("load storage guard", "err", err)
		os.Exit(1)
	}

	prober := probe.New()
	if !prober.HasExiftool() {
		log.Warn("exiftool not found on PATH -- EXIF/XMP extraction disabled, falling back to fast_hash indexing per spec directive 9.4")
	}
	if !prober.HasFFProbe() {
		log.Warn("ffprobe not found on PATH -- video stream inspection disabled")
	}

	hashWorkers := cfg.Workers.HashWorkers
	if hashWorkers <= 0 {
		hashWorkers = min(4, runtime.NumCPU())
	}
	pool := workers.New[string](hashWorkers, 1024)
	pool.Run(ctx)

	engine := graph.NewEngine(database, log, graph.NewProjectSidecarResolver(cfg.PathRewrites), graph.XMPOriginalDocumentIDResolver{}, graph.FilenameStemResolver{}, graph.HeuristicSpatialTemporalResolver{})
	hub := sse.New()
	scanTracker := &pipeline.ScanTracker{}

	var supervisor *pipeline.WatcherSupervisor
	if watched := watchedFromConfig(cfg.StorageLocations); len(watched) > 0 {
		watchedLocs, err := resolveWatchedLocations(ctx, database, watched)
		if err != nil {
			log.Error("resolve watched locations", "err", err)
			os.Exit(1)
		}
		supervisor = pipeline.NewWatcherSupervisor(pipeline.ScanDeps{
			DB: database, Guard: guard, Prober: prober, Pool: pool, Engine: engine,
			FullHashPolicy: cfg.Workers.FullHashPolicy, DisablePerceptualHash: !cfg.Workers.PerceptualHash, Log: log,
		}, func() { hub.Broadcast() })
		supervisor.Start(ctx, watchedLocs, 0)
	}

	spa, err := web.Dist()
	if err != nil {
		log.Error("embed spa", "err", err)
		os.Exit(1)
	}

	srv := httpapi.New(httpapi.Deps{
		Config: &cfg, Log: log, DB: database, Guard: guard, Prober: prober,
		Pool: pool, Engine: engine, Hub: hub, SPA: spa, Version: version,
		Tracker: scanTracker,
	})
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.HTTP.ReadTimeoutSecs) * time.Second,
		WriteTimeout:      time.Duration(cfg.HTTP.WriteTimeoutSecs) * time.Second,
	}

	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown error", "err", err)
		os.Exit(1)
	}
	if supervisor != nil {
		// Watchers already stopped on ctx.Done; Wait() joins each location's
		// consumer goroutine, which holds the writer DB and calls Commit
		// directly (never Pool.Submit), so it must finish before db.Close.
		supervisor.Wait()
	}
	// scanTracker.Wait() joins every in-flight RunScan goroutine (started
	// via POST /api/v1/scan) before the database closes, the same guarantee
	// supervisor.Wait() above already gives the watch path. This must come
	// before pool.Drain(): a scan's own wg.Wait() (internal/pipeline/scan.go)
	// depends on the pool's workers resolving every submitted job one way or
	// another -- either running it or, since #92, calling its OnAbandon hook
	// once the pool's own ctx (this same ctx) is done -- so a scan can only
	// finish once the pool has started winding down, which is already true
	// here since ctx is Done. Draining first would just make this wait
	// redundant, not wrong, but ordering it this way mirrors the watcher
	// case above and keeps "join background scan work" as one clear step.
	scanTracker.Wait()
	// ctx (the signal context) is already Done by this point, which is what
	// tells the pool's worker goroutines to stop after their current job --
	// Drain waits for that to actually finish before the database closes.
	pool.Drain()
	log.Info("server stopped")
}

// reconcileOrphanedScanJobs moves every scan_jobs row still RUNNING to
// FAILED before this process creates any scan_jobs row of its own --
// neither seedStorageLocations nor anything before this call writes
// scan_jobs, and the two producers that do (WatcherSupervisor.Start,
// POST /api/v1/scan) both run later in startup -- so every RUNNING row
// found here unambiguously predates this process. It was left behind by a
// crash (SIGKILL, OOM-kill, container hard-stop, power loss), not
// genuinely still in flight: a WATCH row is RUNNING for its entire process
// lifetime by design, and a FULL_SCAN row only reaches a terminal state
// via the same process that created it. Must run before
// WatcherSupervisor.Start below, or a reconciled row and a fresh row for
// the same location could momentarily both claim to represent "the" watch
// state for it.
func reconcileOrphanedScanJobs(ctx context.Context, database *db.DB, log *slog.Logger) error {
	var n int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.ReconcileOrphanedScanJobs(ctx, sql.NullString{
			String: "reconciled at startup: process restarted while this job was still RUNNING",
			Valid:  true,
		})
		return err
	})
	if err != nil {
		return err
	}
	if n > 0 {
		log.Warn("pipeline: reconciled orphaned scan_jobs rows from a previous process", "count", n)
	}
	return nil
}

// seedStorageLocations applies config.yaml's storageLocations list
// idempotently on every startup (UpsertStorageLocation, keyed on
// root_path's UNIQUE constraint) -- so branchDAM never depends on an
// operator running a separate migration/seed step when a mount is added or
// a tier is reconfigured.
func seedStorageLocations(ctx context.Context, database *db.DB, locations []config.StorageLocation) error {
	if len(locations) == 0 {
		return nil
	}
	return database.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, loc := range locations {
			readOnly := int64(0)
			if loc.ReadOnly {
				readOnly = 1
			}
			prunable := int64(0)
			if loc.Prunable {
				prunable = 1
			}
			if _, err := q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
				Name: loc.Name, RootPath: loc.RootPath, Tier: loc.Tier,
				ReadOnly: readOnly, Prunable: prunable,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// logLevel resolves the slog level from config, letting -debug override it.
func logLevel(cfgLevel string, debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return parseLogLevel(cfgLevel)
}

// watchedFromConfig returns the config locations to watch: opt-in via config
// (`watch: true`) and never Tier 3, regardless of config -- the master
// archive is not watched (spec Pillar 3's continuous ingest is for working
// tiers).
func watchedFromConfig(cfgs []config.StorageLocation) []config.StorageLocation {
	out := make([]config.StorageLocation, 0, len(cfgs))
	for _, c := range cfgs {
		if c.Watch && c.Tier != "TIER3_MASTER_ARCHIVE" {
			out = append(out, c)
		}
	}
	return out
}

// resolveWatchedLocations looks up each watched config entry's storage_locations
// row (upserted by seedStorageLocations a moment earlier) and builds the
// storage.Location slice the supervisor needs. A watched entry with no row is
// a configuration error, surfaced immediately.
func resolveWatchedLocations(ctx context.Context, database *db.DB, cfgs []config.StorageLocation) ([]storage.Location, error) {
	out := make([]storage.Location, 0, len(cfgs))
	for _, c := range cfgs {
		row, err := database.Reader.GetStorageLocationByPath(ctx, c.RootPath)
		if err != nil {
			return nil, fmt.Errorf("watched location %q (%s): %w", c.Name, c.RootPath, err)
		}
		out = append(out, storage.Location{
			ID: row.ID, Name: row.Name, RootPath: row.RootPath, Tier: row.Tier, ReadOnly: row.ReadOnly != 0,
		})
	}
	return out, nil
}
