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

// shutdownBudget is the total time SIGTERM/SIGINT gets to produce process
// exit: httpServer.Shutdown plus every join below share this one deadline
// (#98), not each getting their own -- an operator/orchestrator cares about
// one contract ("this process exits within N seconds of the signal"), not
// the internal breakdown. 30s is generous for httpServer.Shutdown's typical
// sub-second case while still bounding a genuinely slow in-flight
// processFile (a large-file full hash, or a stalled network mount).
const shutdownBudget = 30 * time.Second

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

	if err := run(context.Background(), cfg, log); err != nil {
		log.Error("run", "err", err)
		os.Exit(1)
	}
}

// run owns the server's full lifecycle: setup, serve, and an orderly
// shutdown once ctx's signal fires. Extracted from main so shutdown's
// timeout behavior (#98) is testable without going through flag parsing
// and config loading -- see runShutdownSequence and closeDatabase below,
// which this delegates to and which a test can drive directly with a short
// budget and fake dependencies.
func run(parent context.Context, cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	// dbUnsafeToClose is set by runShutdownSequence below if a wait timed
	// out -- a background goroutine may still hold the writer connection,
	// and closing out from under it is worse than leaving it for process
	// exit to reclaim (SQLite's WAL recovery is designed for exactly that;
	// sql.DB.Close() racing an in-flight write is not). This single defer
	// is the ONLY place database.Close() is called in this function --
	// every setup failure below returns through it too, which is correct:
	// none of those paths have started any long-lived goroutine that could
	// be holding the writer, so closing immediately is always safe there.
	var dbUnsafeToClose bool
	defer func() { closeDatabase(log, database, dbUnsafeToClose) }()

	if _, err := reconcileOrphanedScanJobs(ctx, database, log); err != nil {
		return fmt.Errorf("reconcile orphaned scan jobs: %w", err)
	}

	if err := seedStorageLocations(ctx, database, cfg.StorageLocations); err != nil {
		return fmt.Errorf("seed storage locations: %w", err)
	}

	guard, err := storage.LoadGuard(ctx, database)
	if err != nil {
		return fmt.Errorf("load storage guard: %w", err)
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
			return fmt.Errorf("resolve watched locations: %w", err)
		}
		supervisor = pipeline.NewWatcherSupervisor(pipeline.ScanDeps{
			DB: database, Guard: guard, Prober: prober, Pool: pool, Engine: engine,
			FullHashPolicy: cfg.Workers.FullHashPolicy, DisablePerceptualHash: !cfg.Workers.PerceptualHash, Log: log,
		}, func() { hub.Broadcast() })
		supervisor.Start(ctx, watchedLocs, 0)
	}

	spa, err := web.Dist()
	if err != nil {
		return fmt.Errorf("embed spa: %w", err)
	}

	srv := httpapi.New(httpapi.Deps{
		Config: &cfg, Log: log, DB: database, Guard: guard, Prober: prober,
		Pool: pool, Engine: engine, Hub: hub, SPA: spa, Version: version,
		Tracker: scanTracker, Shutdown: ctx.Done(),
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	shutdownErr := runShutdownSequence(shutdownCtx, log, httpServer, supervisor, scanTracker, pool, &dbUnsafeToClose)
	log.Info("server stopped")
	return shutdownErr
}

// runShutdownSequence performs the ordered post-signal shutdown -- HTTP
// server shutdown, then joining the watcher supervisor, the scan tracker,
// and the worker pool -- each bounded by shutdownCtx's remaining budget via
// waitWithin, rather than the previously-unbounded direct calls. Every wait
// runs regardless of whether an earlier one timed out: a later one may
// still complete and shrink how much work is left unresolved. Sets
// *dbUnsafeToClose (never clears it) if any wait didn't complete in time.
//
// Extracted from run() specifically so a test can drive it directly with a
// short shutdownCtx budget and minimal/fake dependencies (a never-served
// *http.Server, a real workers.Pool with a deliberately slow job, a nil
// supervisor), without needing run()'s full config-load/db.Open/httpapi.New
// setup -- see main_test.go.
func runShutdownSequence(shutdownCtx context.Context, log *slog.Logger, httpServer *http.Server, supervisor *pipeline.WatcherSupervisor, scanTracker *pipeline.ScanTracker, pool *workers.Pool[string], dbUnsafeToClose *bool) error {
	var shutdownErr error
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown: http server", "err", err)
		shutdownErr = err
	}

	if supervisor != nil {
		// Watchers already stopped on ctx.Done; Wait() joins each location's
		// consumer goroutine, which holds the writer DB and calls Commit
		// directly (never Pool.Submit), so it must finish before db.Close.
		if !waitWithin(shutdownCtx, supervisor.Wait) {
			log.Error("shutdown: watcher supervisor wait timed out")
			*dbUnsafeToClose = true
		}
	}
	// scanTracker.Wait() joins every in-flight RunScan goroutine (started
	// via POST /api/v1/scan) before the database closes, the same guarantee
	// supervisor.Wait() above already gives the watch path. This must come
	// before pool.Drain(): a scan's own wg.Wait() (internal/pipeline/scan.go)
	// depends on the pool's workers resolving every submitted job one way or
	// another -- either running it or, since #92, calling its OnAbandon hook
	// once the pool's own ctx (the same signal ctx) is done -- so a scan can
	// only finish once the pool has started winding down, which is already
	// true here since that ctx is Done. Draining first would just make this
	// wait redundant, not wrong, but ordering it this way mirrors the
	// watcher case above and keeps "join background scan work" as one clear
	// step.
	if !waitWithin(shutdownCtx, scanTracker.Wait) {
		log.Error("shutdown: scan tracker wait timed out")
		*dbUnsafeToClose = true
	}
	// The signal ctx is already Done by this point, which is what tells the
	// pool's worker goroutines to stop after their current job -- Drain
	// waits for that to actually finish before the database closes.
	if !waitWithin(shutdownCtx, pool.Drain) {
		log.Error("shutdown: worker pool drain timed out")
		*dbUnsafeToClose = true
	}
	return shutdownErr
}

// waitWithin blocks on wait until it returns or ctx is done, whichever
// comes first, returning whether wait completed within ctx's deadline. The
// goroutine running wait is leaked if ctx's deadline wins (wait may still
// be blocked on real work) -- acceptable here since this only runs once,
// immediately before process exit, and is the same tradeoff the alternative
// (no timeout at all, per #98's original bug) already made permanently.
func waitWithin(ctx context.Context, wait func()) bool {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// closeDatabase is run()'s single database.Close() call site, gated on
// unsafe (set by runShutdownSequence when a wait timed out). Extracted so a
// test can exercise the skip decision directly against a real *db.DB
// without booting the rest of run().
func closeDatabase(log *slog.Logger, database *db.DB, unsafe bool) {
	if unsafe {
		log.Error("shutdown: skipping database close -- a background goroutine may still hold the writer connection")
		return
	}
	if err := database.Close(); err != nil {
		log.Error("close database", "err", err)
	}
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
//
// This assumes exclusive single-process ownership of the database file --
// there is no flock/pid guard anywhere in this codebase, and SQLite's WAL
// mode plus busy_timeout make concurrent access from two branchdam
// instances workable at the driver level, just not safe at this
// invariant's level: a second instance started against the same DB (e.g.
// running `go run ./cmd/branchdam` against a database a running container
// already owns) would mark the first instance's genuinely-live rows
// FAILED out from under it. Documented as a deployment assumption
// (single instance per database file), not enforced in code.
func reconcileOrphanedScanJobs(ctx context.Context, database *db.DB, log *slog.Logger) (int64, error) {
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
		return 0, err
	}
	if n > 0 {
		log.Warn("pipeline: reconciled orphaned scan_jobs rows from a previous process", "count", n)
	}
	return n, nil
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
