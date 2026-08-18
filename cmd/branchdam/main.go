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
	"github.com/s3ntin3l8/branchdam/internal/immich"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/sync"
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
	// dbUnsafeToClose is set below if a shutdown wait times out -- a
	// background goroutine may still hold the writer connection, and
	// closing out from under it is worse than leaving it for process exit
	// to reclaim (SQLite's WAL recovery is designed for exactly that;
	// sql.DB.Close() racing an in-flight write is not).
	var dbUnsafeToClose bool
	defer func() { closeDatabase(log, database, dbUnsafeToClose) }()

	if _, err := reconcileOrphanedScanJobs(ctx, database, log); err != nil {
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

	startImmichWorker(ctx, &cfg, database, log)

	watched := watchedFromConfig(cfg.StorageLocations)
	swept := sweptFromConfig(cfg.StorageLocations)
	warnOverlappingWatchAndSweep(log, watched, swept)

	var supervisor *pipeline.WatcherSupervisor
	if len(watched) > 0 {
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

	var sweeper *pipeline.SweeperSupervisor
	if len(swept) > 0 {
		sweptLocs, err := resolveSweptLocations(ctx, database, swept)
		if err != nil {
			log.Error("resolve swept locations", "err", err)
			os.Exit(1)
		}
		sweeper = pipeline.NewSweeperSupervisor(pipeline.ScanDeps{
			DB: database, Guard: guard, Prober: prober, Pool: pool, Engine: engine,
			FullHashPolicy: cfg.Workers.FullHashPolicy, DisablePerceptualHash: !cfg.Workers.PerceptualHash, Log: log,
			Nudge: func() { hub.Broadcast() }, Shutdown: ctx.Done(),
		})
		sweeper.Start(ctx, sweptLocs)
	}

	spa, err := web.Dist()
	if err != nil {
		log.Error("embed spa", "err", err)
		os.Exit(1)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Deliberately NOT os.Exit(1) here: a scan or watcher goroutine
		// started before shutdown began may still be running and holding
		// the writer connection (pipeline.ScanTracker / WatcherSupervisor),
		// and os.Exit skips every deferred function including the
		// closeDatabase call above -- exiting here would tear the process
		// down mid-write exactly like an unclean crash, the thing this
		// whole sequence exists to avoid. httpServer.Shutdown has two
		// distinct failure modes, not one: an early listener-Close error
		// (returned while shutdownCtx still has most of its budget left),
		// or -- the realistic case -- shutdownCtx's own deadline firing,
		// which means requests/connections were still active when the
		// timeout hit. Either way, joining the background work below is
		// exactly what should happen next, not a reason to skip past it.
		log.Error("shutdown error, continuing to join background work before exit", "err", err)
	}

	// The joins below get their OWN fresh deadline, deliberately NOT
	// shutdownCtx: in the realistic failure case above, shutdownCtx is
	// already expired by the time Shutdown returns its error, so reusing it
	// here would give every waitBounded call effectively zero real time
	// before giving up -- silently defeating the whole point of falling
	// through instead of exiting immediately, while still logging
	// "timed out" as if each component had gotten a real drain window.
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()
	if supervisor != nil {
		// Watchers already stopped on ctx.Done; Wait() joins each location's
		// consumer goroutine, which holds the writer DB and calls Commit
		// directly (never Pool.Submit), so it must finish before db.Close.
		if !waitBounded(joinCtx, log, "supervisor.Wait()", supervisor.Wait) {
			dbUnsafeToClose = true
		}
	}
	if sweeper != nil {
		// Sweepers already stopped scheduling new passes on ctx.Done; Wait()
		// joins each location's in-flight pass (runOneSweep), which holds
		// the writer DB, so it must finish before db.Close.
		if !waitBounded(joinCtx, log, "sweeper.Wait()", sweeper.Wait) {
			dbUnsafeToClose = true
		}
	}
	// scanTracker.Wait() joins every in-flight RunScan goroutine (started
	// via POST /api/v1/scan) before the database closes.
	if !waitBounded(joinCtx, log, "scanTracker.Wait()", scanTracker.Wait) {
		dbUnsafeToClose = true
	}
	// Drain waits for worker goroutines to finish their current job before the database closes.
	if !waitBounded(joinCtx, log, "pool.Drain()", pool.Drain) {
		dbUnsafeToClose = true
	}
	log.Info("server stopped")
}

// waitBounded blocks on waitFunc until it returns or ctx is done, whichever
// comes first, returning whether waitFunc completed within ctx's deadline.
// The goroutine running waitFunc is leaked if ctx's deadline wins (it may
// still be blocked on real work) -- acceptable here since this only runs
// once, immediately before process exit.
func waitBounded(ctx context.Context, log *slog.Logger, name string, waitFunc func()) bool {
	done := make(chan struct{})
	go func() {
		waitFunc()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		log.Warn("shutdown wait timed out", "component", name, "err", ctx.Err())
		return false
	}
}

// closeDatabase is main()'s single database.Close() call site, skipped
// when unsafe is true (see dbUnsafeToClose's doc comment above).
func closeDatabase(log *slog.Logger, database *db.DB, unsafe bool) {
	if unsafe {
		log.Error("shutdown: skipping database close -- a background goroutine may still hold the writer connection")
		return
	}
	if err := database.Close(); err != nil {
		log.Error("close database", "err", err)
	}
}

// startImmichWorker wires the Immich sync worker and returns it, or nil when
// Immich is not configured. Runs RecoverStalePushing at startup so a crashed
// mid-push doesn't strand nodes in PUSHING forever.
func startImmichWorker(ctx context.Context, cfg *config.Config, database *db.DB, log *slog.Logger) *sync.Worker {
	// Empty apiUrl is the documented off switch; an unresolved ${VAR} left as
	// literal text by config's expandEnv (unset environment variable) is also
	// treated as disabled rather than pointed at a bogus host.
	if cfg.Immich.APIURL == "" || strings.Contains(cfg.Immich.APIURL, "${") {
		return nil
	}
	immichClient := immich.New(immich.Config{
		APIURL: cfg.Immich.APIURL, APIKey: cfg.Immich.APIKey, LibraryID: cfg.Immich.LibraryID,
	})
	syncManager := sync.NewManager(database, log)
	if n, err := syncManager.RecoverStalePushing(ctx, sync.RemoteImmich, 5*time.Minute); err != nil {
		log.Warn("sync: recover stale pushing", "err", err)
	} else if n > 0 {
		log.Warn("sync: recovered stale PUSHING rows", "count", n)
	}
	exportPath := strings.TrimRight(cfg.Immich.ExportPath, "/")
	if exportPath == "" {
		exportPath = "/storage/exports/immich"
	}
	worker := sync.NewWorker(syncManager, sync.RemoteImmich, exportPath, 16, 10*time.Second,
		func(ctx context.Context, nodes []sync.Node) error {
			return immichClient.TriggerScan(ctx)
		}, log)
	go worker.Run(ctx)
	log.Info("sync: immich worker started", "libraryID", cfg.Immich.LibraryID, "exportPath", exportPath)
	return worker
}

// reconcileOrphanedScanJobs moves every scan_jobs row still RUNNING to
// FAILED before this process creates any scan_jobs row of its own --
// neither seedStorageLocations nor anything before this call writes
// scan_jobs, and the three producers that do (WatcherSupervisor.Start,
// SweeperSupervisor.Start, POST /api/v1/scan) all run later in startup --
// so every RUNNING row found here unambiguously predates this process. It
// was left behind by a crash (SIGKILL, OOM-kill, container hard-stop,
// power loss), not genuinely still in flight: a WATCH row is RUNNING for
// its entire process lifetime by design, and a FULL_SCAN/INCREMENTAL row
// only reaches a terminal state via the same process that created it. Must
// run before WatcherSupervisor.Start/SweeperSupervisor.Start below, or a
// reconciled row and a fresh row for the same location could momentarily
// both claim to represent "the" watch/sweep state for it.
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

// sweptFromConfig returns the config locations to sweep: opt-in via config
// (`sweep: true`) and never Tier 3, regardless of config. Unlike
// watchedFromConfig's rationale (continuous ingest is for working tiers),
// the reason here is structural: Tier 3 is read_only=1 by schema CHECK, so
// nothing on it can ever be ingested, and a differential sweep there would
// only ever drive the MISSING sweep -- which a manual POST /api/v1/scan
// already covers.
func sweptFromConfig(cfgs []config.StorageLocation) []config.StorageLocation {
	out := make([]config.StorageLocation, 0, len(cfgs))
	for _, c := range cfgs {
		if c.Sweep && c.Tier != "TIER3_MASTER_ARCHIVE" {
			out = append(out, c)
		}
	}
	return out
}

// resolveSweptLocations mirrors resolveWatchedLocations for swept
// locations, pairing each resolved storage.Location with its configured
// (or zero, meaning "use SweeperSupervisor's default") sweep interval.
func resolveSweptLocations(ctx context.Context, database *db.DB, cfgs []config.StorageLocation) ([]pipeline.SweptLocation, error) {
	out := make([]pipeline.SweptLocation, 0, len(cfgs))
	for _, c := range cfgs {
		row, err := database.Reader.GetStorageLocationByPath(ctx, c.RootPath)
		if err != nil {
			return nil, fmt.Errorf("swept location %q (%s): %w", c.Name, c.RootPath, err)
		}
		out = append(out, pipeline.SweptLocation{
			Location: storage.Location{
				ID: row.ID, Name: row.Name, RootPath: row.RootPath, Tier: row.Tier, ReadOnly: row.ReadOnly != 0,
			},
			Interval: time.Duration(c.SweepIntervalSecs) * time.Second,
		})
	}
	return out, nil
}

// warnOverlappingWatchAndSweep logs when a location has both fsnotify
// watching and the differential sweep enabled. Not a config error: the
// watcher bypasses the worker pool entirely (handleWatchItem calls
// processFile/Commit directly on its own consumer goroutine, never
// Pool.Submit), so the two mechanisms' per-key dedup windows never overlap
// -- a location can legitimately want both, e.g. while migrating a share
// from local disk to SMB. The combination is merely wasteful, not
// corrupting, for a quiescent file: the writer connection is
// SetMaxOpenConns(1), so both Commits serialize and the second sees a
// matching fast_hash and takes the Touch branch, not a version collision.
// A file still being copied mid-write is a pre-existing watcher exposure
// either way (handleWatchItem already hashes mid-copy files) -- not
// something this combination introduces.
func warnOverlappingWatchAndSweep(log *slog.Logger, watched, swept []config.StorageLocation) {
	sweptPaths := make(map[string]bool, len(swept))
	for _, c := range swept {
		sweptPaths[c.RootPath] = true
	}
	for _, c := range watched {
		if sweptPaths[c.RootPath] {
			log.Warn("pipeline: location has both watch and sweep enabled -- wasteful but not corrupting for a quiescent file (see CLAUDE.md)",
				"location", c.Name, "rootPath", c.RootPath)
		}
	}
}
