package sync

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/immich"
)

// immichParams is the subset of config.Config.Immich that determines
// whether the running Worker needs to be replaced. Reload no-ops when a
// settings write changed something unrelated (e.g. logLevel) and none of
// these moved, rather than tearing down and restarting the worker on every
// settings.Store.Apply call regardless of what it actually changed.
type immichParams struct {
	apiURL, apiKey, libraryID, exportPath string
}

func immichParamsFrom(cfg *config.Config) immichParams {
	return immichParams{
		apiURL:     cfg.Immich.APIURL,
		apiKey:     cfg.Immich.APIKey,
		libraryID:  cfg.Immich.LibraryID,
		exportPath: cfg.Immich.ExportPath,
	}
}

// Supervisor owns the lifecycle of the single Immich sync Worker across
// config changes, so a settings.Store.Subscribe callback (see cmd/branchdam)
// can swap it live instead of requiring a process restart. Only one worker
// instance ever runs at a time -- Reload cancels and joins the current one
// before starting its replacement, so there is never a window where two
// workers race the same remote_sync_state rows.
type Supervisor struct {
	database *db.DB
	log      *slog.Logger

	mu      sync.Mutex
	rootCtx context.Context // set by Start; every worker's context is derived from this
	cancel  context.CancelFunc
	worker  *Worker
	running immichParams // params of the currently running (or last attempted) worker
}

func NewSupervisor(database *db.DB, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{database: database, log: log}
}

// Start begins the initial worker, if Immich is configured, under ctx.
// ctx is retained as the parent for every future Reload-driven restart, so
// a process-shutdown cancellation of ctx also stops whatever worker is
// running at the time -- the same contract Worker.Start itself has.
func (sv *Supervisor) Start(ctx context.Context, cfg *config.Config) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.rootCtx = ctx
	sv.startLocked(cfg)
}

// Reload is the settings.Store.Subscribe callback: stops the currently
// running worker and starts a replacement built from the new config, or
// leaves it stopped if apiUrl/libraryId resolve empty -- the same
// off-switch rule the original startImmichWorker enforced. A no-op if
// Start hasn't been called yet, or if none of the Immich fields that
// matter actually changed.
//
// Runs synchronously inside settings.Store.Apply (see Subscribe's doc
// comment), so a PUT /api/v1/settings that changes an Immich field blocks
// until the old worker's current drain tick observes ctx cancellation and
// returns. Bounded, not instant: cancelling ctx aborts any in-flight
// TriggerScan immediately (immich.Client threads ctx into the request via
// http.NewRequestWithContext, and its own http.Client.Timeout is 15s), so
// the worst case is one full drain-and-push cycle, not an unbounded wait.
// This is the same trade-off Subscribe's contract already accepts (a slow
// subscriber only ever delays the response, it cannot fail the write,
// which has already committed).
func (sv *Supervisor) Reload(cfg *config.Config) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.rootCtx == nil {
		return
	}
	if immichParamsFrom(cfg) == sv.running {
		return
	}
	sv.stopLocked()
	sv.startLocked(cfg)
}

// startLocked assumes mu is held and sv.rootCtx has already been set by
// Start.
func (sv *Supervisor) startLocked(cfg *config.Config) {
	params := immichParamsFrom(cfg)
	sv.running = params

	// A Reload can be driven by an in-flight PUT /api/v1/settings request
	// that is still executing when process shutdown begins: run()'s own
	// shutdown comment (cmd/branchdam/main.go) names httpServer.Shutdown's
	// deadline firing with requests still active as the realistic case, not
	// the exception. If that happens, sv.rootCtx (main's shutdown-bound ctx)
	// is already cancelled by the time this Reload gets here, but nothing
	// stops the call from reaching this point regardless. Starting a new
	// worker now -- including the RecoverStalePushing write below -- would
	// risk writer activity after run()'s join sequence has already Wait()ed
	// this supervisor and moved on to pool.Drain()/db.Close(), the exact
	// hazard dbUnsafeToClose exists to guard against. This narrows the
	// race window to the gap between this check and worker.Start below,
	// rather than eliminating it -- the same "bounded, not zero" posture
	// prune.Execute's own TOCTOU checks take (see CLAUDE.md).
	if sv.rootCtx.Err() != nil {
		return
	}

	// Empty apiUrl is the documented off switch; an unresolved ${VAR} left
	// as literal text by config's expandEnv (unset environment variable) is
	// also treated as disabled rather than pointed at a bogus host.
	if params.apiURL == "" || strings.Contains(params.apiURL, "${") {
		return
	}
	// An empty libraryId would call POST /api/libraries//scan, which 404s
	// forever -- every push failing, retried until the retry_count bound
	// kicks in, then permanently stuck PUSH_FAILED with no way to recover
	// short of a config fix (#182). Refuse to start rather than run a
	// worker that can never succeed.
	if params.libraryID == "" || strings.Contains(params.libraryID, "${") {
		sv.log.Warn("sync: immich.libraryId is empty or unresolved, not starting the sync worker", "libraryID", params.libraryID)
		return
	}

	immichClient := immich.New(immich.Config{
		APIURL: params.apiURL, APIKey: params.apiKey, LibraryID: params.libraryID,
	})
	syncManager := NewManager(sv.database, sv.log)
	if n, err := syncManager.RecoverStalePushing(sv.rootCtx, RemoteImmich, 5*time.Minute); err != nil {
		sv.log.Warn("sync: recover stale pushing", "err", err)
	} else if n > 0 {
		sv.log.Warn("sync: recovered stale PUSHING rows", "count", n)
	}
	exportPath := strings.TrimRight(params.exportPath, "/")
	if exportPath == "" {
		exportPath = "/storage/exports/immich"
	}
	worker := NewWorker(syncManager, RemoteImmich, exportPath, 16, 10*time.Second,
		func(ctx context.Context, nodes []Node) error {
			return immichClient.TriggerScan(ctx)
		}, sv.log)

	ctx, cancel := context.WithCancel(sv.rootCtx)
	worker.Start(ctx)
	sv.worker = worker
	sv.cancel = cancel
	sv.log.Info("sync: immich worker started", "libraryID", params.libraryID, "exportPath", exportPath)
}

// stopLocked assumes mu is held. Cancels and joins the currently running
// worker, if any. Reload must join here, before starting a replacement, so
// the old and new workers never race the same remote_sync_state rows.
func (sv *Supervisor) stopLocked() {
	if sv.cancel != nil {
		sv.cancel()
	}
	if sv.worker != nil {
		sv.worker.Wait()
	}
	sv.worker = nil
	sv.cancel = nil
}

// Wait blocks until the currently running worker, if any, has fully
// stopped. Call during shutdown, after the process's shutdown context
// (the same one passed to Start) is cancelled and before the database
// closes -- mirrors Worker.Wait's own contract. Safe to call even if no
// worker is currently running (Immich never configured, or Reload left it
// stopped).
func (sv *Supervisor) Wait() {
	sv.mu.Lock()
	worker := sv.worker
	sv.mu.Unlock()
	if worker != nil {
		worker.Wait()
	}
}
