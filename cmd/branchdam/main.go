// Command branchdam runs the branchDAM server: it indexes configured storage
// locations into a version node graph, resolves lineage edges between them,
// and serves the SPA + REST/SSE API.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/agent"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/httpapi"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/prune"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/sync"
	"github.com/s3ntin3l8/branchdam/internal/thumbs"
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

	// BRANCHDAM_SECRET_KEY encrypts settings values that must not sit in
	// app_settings as plaintext (immich.apiKey, agent.apiKey). Unset is a
	// normal, supported state -- internal/settings falls back to the
	// config/env base value for any secret field rather than failing to
	// boot (see internal/secrets' package doc). A *set but invalid* key is
	// instead an operator mistake worth failing loudly on.
	secretBox, err := secrets.NewBox(os.Getenv("BRANCHDAM_SECRET_KEY"))
	if err != nil {
		log.Error("invalid BRANCHDAM_SECRET_KEY", "err", err)
		os.Exit(1)
	}
	if secretBox == nil {
		log.Warn("BRANCHDAM_SECRET_KEY not set -- UI-configured secrets (e.g. Immich API key) are unavailable until it is")
	}

	settingsStore, err := settings.NewStore(ctx, database, cfg, secretBox, log)
	if err != nil {
		log.Error("load settings overrides", "err", err)
		os.Exit(1)
	}
	// From here on, cfg IS the resolved config: config.yaml/.env as loaded,
	// with any app_settings override applied on top (settings.Resolve's
	// precedence rule). Every existing cfg.X read below picks this up for
	// free -- see docs/configuration.md's precedence section. This is a
	// boot-time snapshot only; live reconfiguration of a running process
	// (Immich, see internal/sync) is wired separately via
	// settingsStore.Subscribe.
	cfg = *settingsStore.Effective()

	// StorageLocations is not a registered Field (it's a dynamic list
	// keyed by rootPath, not the registry's fixed-key model), so
	// Effective() above left it as the raw config.yaml list -- resolve its
	// own storageLocation.* overrides on top separately, still before
	// validatePruneConfig/seedStorageLocations/watchedFromConfig/
	// sweptFromConfig below, all of which must see the same effective
	// list. This is why ResolveStorageLocations validates cacheTtlHours
	// itself rather than leaving a since-invalid override for
	// validatePruneConfig to fatal on -- see its doc comment.
	storageLocationOverrides, err := settings.LoadStorageLocationOverrides(ctx, database.Reader)
	if err != nil {
		log.Error("load storage location overrides", "err", err)
		os.Exit(1)
	}
	cfg.StorageLocations = settings.ResolveStorageLocations(cfg.StorageLocations, storageLocationOverrides, log)

	if err := validatePruneConfig(cfg.StorageLocations); err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(1)
	}
	// dbUnsafeToClose is set by run() if a shutdown wait times out -- a
	// background goroutine may still hold the writer connection, and
	// closing out from under it is worse than leaving it for process exit
	// to reclaim (SQLite's WAL recovery is designed for exactly that;
	// sql.DB.Close() racing an in-flight write is not).
	//
	// Deliberately not a defer: run() already joins every background
	// goroutine before it returns, so by the time we get here it's safe --
	// and necessary -- to close explicitly on both the success and the
	// error path, rather than skip it via os.Exit(1) below (a deferred
	// closeDatabase would never run once os.Exit is called). This doesn't
	// change behavior for the os.Exit(1) calls further up in the setup
	// sequence above (config/DB-open/guard/etc. failures) -- those already
	// ran before run() existed and always skipped a deferred closeDatabase
	// too, for the same reason; they're unrelated to this fix and still
	// rely on process exit for cleanup, same as before.
	var dbUnsafeToClose bool

	// restartRequested is set by the POST /api/v1/restart handler's
	// RequestRestart hook (via requestRestart below) before it calls stop()
	// -- the same cancellation SIGTERM/SIGINT trigger, so the shutdown
	// sequence run() runs is identical either way. Only the post-run()
	// branch at the bottom of main differs: a restart re-execs the process
	// in place instead of letting it exit. atomic because the hook runs on
	// an HTTP handler's goroutine, read from main's goroutine after run()
	// returns.
	var restartRequested atomic.Bool
	requestRestart := func() {
		restartRequested.Store(true)
		stop()
	}

	if _, err := reconcileOrphanedScanJobs(ctx, database, log); err != nil {
		log.Error("reconcile orphaned scan jobs", "err", err)
		os.Exit(1)
	}

	if err := seedStorageLocations(ctx, database, cfg.StorageLocations); err != nil {
		log.Error("seed storage locations", "err", err)
		os.Exit(1)
	}

	// A single unresolvable mount (M6) does not abort startup -- see
	// storage.LoadGuard's doc comment. skippedLocationIDs are the
	// locations LoadGuard excluded; deactivateStorageLocations persists
	// that (non-fatal if it fails: is_active is an observability field,
	// not a safety gate -- LoadGuard already excluded them from the Guard
	// regardless of whether this write lands).
	guard, skippedLocationIDs, err := storage.LoadGuard(ctx, database, log)
	if err != nil {
		log.Error("load storage guard", "err", err)
		os.Exit(1)
	}
	if len(skippedLocationIDs) > 0 {
		if err := deactivateStorageLocations(ctx, database, skippedLocationIDs); err != nil {
			log.Error("mark unresolvable storage locations inactive", "err", err)
		}
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

	sidecarResolver := graph.NewProjectSidecarResolver(cfg.PathRewrites)
	settingsStore.Subscribe(func(newCfg *config.Config) {
		sidecarResolver.SetRewrites(newCfg.PathRewrites)
	})
	engine := graph.NewEngine(database, log, sidecarResolver, graph.XMPOriginalDocumentIDResolver{}, graph.FilenameStemResolver{}, graph.HeuristicSpatialTemporalResolver{})
	hub := sse.New()
	scanTracker := &pipeline.ScanTracker{}

	// Supervisor starts the worker once here and again on every subsequent
	// settings.Store.Apply that changes an Immich field, via Subscribe --
	// see internal/sync.Supervisor's doc comment. Registered before any
	// scan/watch/sweep goroutine starts so a settings write racing early
	// startup can't be missed.
	immichSupervisor := sync.NewSupervisor(database, log)
	immichSupervisor.Start(ctx, &cfg)
	settingsStore.Subscribe(immichSupervisor.Reload)

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

	// Drains event_queue rows POST /api/v1/agent/events persists -- issue
	// #166: the drain logic (internal/agent, Phase 8) had zero production
	// callers until now. Always started, unconditionally: an empty queue
	// costs one indexed COUNT query per DefaultDrainInterval tick, the same
	// shape as WatcherSupervisor/SweeperSupervisor above, which are the
	// ones gated on config (there's no per-location config for the agent
	// queue to gate on -- it's one global queue, not per-storage-location).
	drainer := agent.NewDrainer(database, guard, log,
		agent.WithNudge(func() { hub.Broadcast() }),
		agent.WithEngine(engine),
		agent.WithImmichScanner(immichSupervisor))
	drainer.Start(ctx, 0)

	trashWorker := prune.NewTrashWorker(guard, func() int {
		return settingsStore.Effective().Trash.RetentionDays
	}, log)
	trashWorker.Start(ctx, 0)

	thumbWorker, thumbCache := startThumbWorker(ctx, &cfg, database, guard, prober, log, hub)

	spa, err := web.Dist()
	if err != nil {
		log.Error("embed spa", "err", err)
		os.Exit(1)
	}

	srv := httpapi.New(httpapi.Deps{
		Config: &cfg, Settings: settingsStore, Log: log, DB: database, Guard: guard, Prober: prober,
		Pool: pool, Engine: engine, Hub: hub, SPA: spa, Version: version,
		Tracker: scanTracker, Shutdown: ctx.Done(), ThumbCache: thumbCache,
		RequestRestart: requestRestart,
	})
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.HTTP.ReadTimeoutSecs) * time.Second,
		WriteTimeout:      time.Duration(cfg.HTTP.WriteTimeoutSecs) * time.Second,
	}

	runErr := run(ctx, stop, log, httpServer, supervisor, sweeper, scanTracker, immichSupervisor, drainer, thumbWorker, pool, &dbUnsafeToClose)
	closeDatabase(log, database, dbUnsafeToClose)
	if runErr != nil {
		log.Error("run", "err", runErr)
		os.Exit(1)
	}

	// A restart request (POST /api/v1/restart) reached here via the exact
	// same shutdown path as SIGTERM -- run() has already returned cleanly
	// and the database is closed. Re-exec in place rather than falling off
	// the end of main: unlike relying on a supervisor to restart an exited
	// process (compose.yaml's restart: unless-stopped), this works
	// identically under Docker, systemd, and a bare `go run`/`make dev-api`
	// with no supervisor at all. execve replaces the whole process image
	// (all threads, not just this goroutine), so a goroutine leaked by a
	// timed-out waitBounded join inside run() cannot survive into the new
	// process -- strictly safer than the plain-exit path, which merely
	// races the OS reaping the same leak. os.Executable(), not os.Args[0],
	// which can be a relative path that a changed working directory would
	// resolve wrong. Inherits the current environment: config.yaml IS
	// re-read from disk, but .env is NOT (Compose injects it only at
	// container start) -- see docs/operations.md.
	if restartRequested.Load() {
		log.Info("restarting")
		exe, err := os.Executable()
		if err == nil {
			err = syscall.Exec(exe, os.Args, os.Environ())
			// syscall.Exec only returns on failure -- success replaces this
			// process entirely and nothing below ever runs.
		}
		log.Error("restart: exec failed, exiting for a supervisor to restart instead", "err", err)
		os.Exit(1)
	}
}

// run starts httpServer, blocks until ctx is cancelled (SIGTERM/SIGINT, or a
// serve-time error below calling stop()), then runs the bounded shutdown
// sequence: httpServer.Shutdown followed by joining every background
// goroutine that might still hold the writer DB connection. It returns a
// non-nil error when ListenAndServe fails for a reason other than a normal
// Shutdown-triggered close (e.g. the configured listenAddr is already in
// use) -- previously such a failure logged an error and fell through this
// exact same shutdown path with a nil result, so the process exited 0 even
// though it never successfully started serving (#123).
func run(ctx context.Context, stop context.CancelFunc, log *slog.Logger, httpServer *http.Server,
	supervisor *pipeline.WatcherSupervisor, sweeper *pipeline.SweeperSupervisor, scanTracker *pipeline.ScanTracker,
	immichSupervisor *sync.Supervisor, drainer *agent.Drainer, thumbWorker *thumbs.Worker, pool *workers.Pool[string], dbUnsafeToClose *bool,
) error {
	// Buffered so the goroutine never blocks sending its result, whether or
	// not run() is still around to receive it by the time it does.
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			serveErr <- err
			stop()
			return
		}
		serveErr <- nil
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Deliberately NOT returning here: a scan or watcher goroutine
		// started before shutdown began may still be running and holding
		// the writer connection (pipeline.ScanTracker / WatcherSupervisor),
		// and returning early would skip joining them below -- tearing the
		// process down mid-write exactly like an unclean crash, the thing
		// this whole sequence exists to avoid. httpServer.Shutdown has two
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
			*dbUnsafeToClose = true
		}
	}
	if sweeper != nil {
		// Sweepers already stopped scheduling new passes on ctx.Done; Wait()
		// joins each location's in-flight pass (runOneSweep), which holds
		// the writer DB, so it must finish before db.Close.
		if !waitBounded(joinCtx, log, "sweeper.Wait()", sweeper.Wait) {
			*dbUnsafeToClose = true
		}
	}
	// scanTracker.Wait() joins every in-flight RunScan goroutine (started
	// via POST /api/v1/scan) before the database closes.
	if !waitBounded(joinCtx, log, "scanTracker.Wait()", scanTracker.Wait) {
		*dbUnsafeToClose = true
	}
	if immichSupervisor != nil {
		// The worker immichSupervisor currently owns, if any, is ctx-bound
		// (Start's derived context is cancelled by ctx.Done()); Wait joins
		// it before the database closes, matching the supervisor/scan
		// paths. Wait() itself is also a safe no-op when Immich was never
		// configured or Reload last left it stopped.
		if !waitBounded(joinCtx, log, "immichSupervisor.Wait()", immichSupervisor.Wait) {
			*dbUnsafeToClose = true
		}
	}
	// drainer.Start's loop already stopped scheduling new passes on
	// ctx.Done; Wait() joins its current pass (each event its own
	// transaction against the writer DB) before the database closes.
	if !waitBounded(joinCtx, log, "drainer.Wait()", drainer.Wait) {
		*dbUnsafeToClose = true
	}
	if thumbWorker != nil {
		// Same reasoning as drainer.Wait() above: thumbWorker's own pass
		// holds the writer DB connection (each node's SetThumbState is its
		// own transaction) and must finish before db.Close.
		if !waitBounded(joinCtx, log, "thumbWorker.Wait()", thumbWorker.Wait) {
			*dbUnsafeToClose = true
		}
	}
	// Drain waits for worker goroutines to finish their current job before the database closes.
	if !waitBounded(joinCtx, log, "pool.Drain()", pool.Drain) {
		*dbUnsafeToClose = true
	}
	log.Info("server stopped")

	// By this point httpServer.Shutdown has returned, which per net/http's
	// own contract means ListenAndServe has already (or is about to have)
	// returned too -- so serveErr is either already populated or about to
	// be, well within this bound. A genuine serve-time error (e.g. a bind
	// failure) takes priority over a nil/clean result: it's the reason ctx
	// was cancelled in the first place, and the caller needs to know
	// startup never actually succeeded.
	select {
	case err := <-serveErr:
		return err
	case <-time.After(100 * time.Millisecond):
		return nil
	}
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

// startThumbWorker builds the thumbnail cache and, if thumbnails.enabled,
// the background generation worker. cacheDir is created here (not left to
// Cache.Write's own per-shard MkdirAll) so a misconfigured/unwritable path
// fails loudly at startup rather than on the worker's first pass or a
// client's first GET.
//
// The cache is built regardless of thumbnails.enabled: GET
// /api/v1/assets/{id}/thumbnail (internal/httpapi) must still be able to
// serve thumbnails generated while the worker was previously running, even
// if it's since been turned off -- only new generation is gated on the
// config flag. worker is nil when disabled or the cache dir couldn't be
// created; cache is nil only in the latter case.
func startThumbWorker(ctx context.Context, cfg *config.Config, database *db.DB, guard *storage.Guard, prober *probe.Prober, log *slog.Logger, hub *sse.Hub) (worker *thumbs.Worker, cache *thumbs.Cache) {
	cacheDir := cfg.Thumbnails.CacheDir
	if cacheDir == "" {
		cacheDir = "/data/thumbs"
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Error("thumbs: create cache dir, thumbnails unavailable", "cacheDir", cacheDir, "err", err)
		return nil, nil
	}
	cache = thumbs.New(cacheDir, guard, prober, cfg.Thumbnails.MaxEdgePx)
	if !cfg.Thumbnails.Enabled {
		return nil, cache
	}
	worker = thumbs.NewWorker(database, cache, log,
		thumbs.WithNudge(func() { hub.Broadcast() }),
		thumbs.WithConcurrency(cfg.Thumbnails.Workers))
	worker.Start(ctx, time.Duration(cfg.Thumbnails.IntervalSecs)*time.Second)
	log.Info("thumbs: worker started", "cacheDir", cacheDir)
	return worker, cache
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
		rootPaths := make([]string, 0, len(locations))
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
				CacheTtlHours: int64(loc.CacheTTLHours),
			}); err != nil {
				return err
			}
			rootPaths = append(rootPaths, loc.RootPath)
		}
		// M6: deactivate any previously active location whose root_path is
		// no longer configured -- an operator removing a location from
		// config.yaml should self-heal storage-health/UI state without a
		// manual DB edit, the same as a mount that simply vanished
		// (storage.LoadGuard, below). Safe to call unconditionally here:
		// rootPaths has at least one entry because of the len(locations)
		// == 0 early return above -- see DeactivateStorageLocationsNotIn's
		// doc comment for why an empty array would be dangerous.
		jsonRootPaths, err := json.Marshal(rootPaths)
		if err != nil {
			return fmt.Errorf("marshal configured root paths: %w", err)
		}
		_, err = q.DeactivateStorageLocationsNotIn(ctx, string(jsonRootPaths))
		return err
	})
}

// deactivateStorageLocations marks each given storage_locations row
// inactive (M6) -- called for locations storage.LoadGuard could not
// resolve at startup. A separate transaction from seedStorageLocations'
// since it depends on LoadGuard's result, which itself runs after (and
// depends on) seedStorageLocations having already committed.
func deactivateStorageLocations(ctx context.Context, database *db.DB, ids []int64) error {
	return database.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, id := range ids {
			if err := q.SetStorageLocationActive(ctx, sqlcgen.SetStorageLocationActiveParams{ID: id, IsActive: 0}); err != nil {
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

// validatePruneConfig rejects a config where cacheTtlHours can never
// actually make a location eligible for TTL cache pruning (#61): set on a
// non-prunable location (the schema's own CHECK already restricts prunable
// to TIER1_LOCAL_SCRATCH, so this also catches every non-Tier-1 location),
// or negative. handlePrune's own ttlHours <= 0 check already treats a
// negative value the same as zero ("never eligible"), so a negative value
// would otherwise pass validation and then silently no-op forever --
// caught at startup instead, same as the positive-but-non-prunable case,
// so an operator's typo surfaces as a config error, not a purge that
// quietly never fires.
func validatePruneConfig(cfgs []config.StorageLocation) error {
	for _, c := range cfgs {
		if c.CacheTTLHours < 0 {
			return fmt.Errorf("storage location %q: cacheTtlHours must not be negative", c.Name)
		}
		if c.CacheTTLHours > 0 && !c.Prunable {
			return fmt.Errorf("storage location %q: cacheTtlHours is set but prunable is false", c.Name)
		}
	}
	return nil
}

// sweptFromConfig returns the config locations to sweep: opt-in via config
// (`sweep: true`) and never Tier 3, regardless of config. Unlike
// watchedFromConfig's rationale (continuous ingest is for working tiers),
// Tier 3 is read-only unless `readOnly: false` is configured; the `:ro` mount
// remains a defense-in-depth default, so nothing on it can ever be ingested,
// and a differential sweep there would only ever drive the MISSING sweep --
// which a manual POST /api/v1/scan already covers.
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
			log.Warn("pipeline: location has both watch and sweep enabled -- wasteful but not corrupting for a quiescent file (see AGENTS.md)",
				"location", c.Name, "rootPath", c.RootPath)
		}
	}
}
