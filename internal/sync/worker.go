package sync

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// Worker is the periodic loop that drains remote_sync_state for one remote,
// driving NOT_QUEUED -> PENDING_CLOUD_PUSH -> PUSHING -> PUSHED/PUSH_FAILED
// via Manager. Each drain tick (1) re-claims PUSH_FAILED rows old enough to
// retry, (2) enqueues live nodes under exportPath that aren't yet tracked for
// this remote, and (3) processes one PENDING batch through the injected
// PushFunc. The PushFunc is the actual outbound call (e.g. the Immich
// client's TriggerScan); the worker itself stays HTTP-free.
//
// PUSHED means "the remote was asked to process the node" (for Immich, the
// library scan was requested) -- NOT that the remote has finished indexing it.
// With zero-copy disk indexing there is no completion signal to wait on, so
// PUSHED is set at scan-trigger time.
type Worker struct {
	manager    *Manager
	remote     string
	exportPath string
	batchSize  int
	interval   time.Duration
	// retryWindow is how old a PUSH_FAILED row must be before the worker
	// re-claims it (bounded retry frequency -- not a hot loop).
	retryWindow time.Duration
	// maxRetries bounds how many times RecoverFailedPushes will re-claim the
	// same row before leaving it PUSH_FAILED permanently (#182).
	maxRetries int
	push       PushFunc
	log        *slog.Logger
	// wg tracks the run goroutine started by Start so Wait can join it before
	// the database closes during shutdown.
	wg sync.WaitGroup
	// triggeredThisRun tracks whether push has already fired during the
	// current contiguous run of non-empty drain ticks (#183). The underlying
	// operation (e.g. Immich's TriggerScan) isn't scoped to one batch's
	// nodes -- it makes the remote (re-)discover everything already on the
	// shared mount -- so re-invoking it on every tick of a long backfill is
	// one full remote operation per batchSize nodes for no added benefit.
	// Reset to false whenever a tick finds nothing to push, so a later burst
	// of newly-arrived files still gets its own fresh trigger. Only read/
	// written from the single run() goroutine, so it needs no lock.
	triggeredThisRun bool
}

func NewWorker(manager *Manager, remote, exportPath string, batchSize int, interval time.Duration, push PushFunc, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if batchSize <= 0 {
		batchSize = 16
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Worker{manager: manager, remote: remote, exportPath: exportPath,
		batchSize: batchSize, interval: interval, retryWindow: 5 * time.Minute,
		maxRetries: DefaultMaxSyncRetries, push: push, log: log}
}

// SetMaxRetries overrides the default automatic-retry ceiling (#182),
// mirroring internal/agent.Drainer.SetMaxRetries.
func (w *Worker) SetMaxRetries(n int) {
	if n <= 0 {
		return
	}
	w.maxRetries = n
}

// Start launches the drain loop in a goroutine that Wait joins. Safe to call
// once. Returns immediately if there is nothing to run (nil manager or empty
// exportPath) -- in that case Wait has nothing to join.
func (w *Worker) Start(ctx context.Context) {
	if w.manager == nil || w.exportPath == "" {
		return
	}
	w.wg.Add(1)
	go w.run(ctx)
}

func (w *Worker) run(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// Wait blocks until the goroutine started by Start has returned. Call during
// shutdown, after ctx is cancelled, before the database closes.
func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) drain(ctx context.Context) {
	// Re-claim PUSH_FAILED rows old enough to retry, so a transient remote
	// failure doesn't strand a batch forever. Then enqueue brand-new nodes.
	if n, err := w.manager.RecoverFailedPushes(ctx, w.remote, w.retryWindow, w.maxRetries); err != nil {
		w.log.Warn("sync: recover failed pushes", "err", err)
	} else if n > 0 {
		w.log.Info("sync: recovered failed pushes for retry", "remote", w.remote, "count", n)
	}
	w.enqueueUntracked(ctx)
	n, err := w.manager.ProcessPending(ctx, w.remote, w.batchSize, w.coalescedPush)
	if err != nil {
		w.log.Error("sync: drain failed", "remote", w.remote, "err", err)
		return
	}
	if n == 0 {
		// The backlog is drained for now -- the next non-empty tick starts a
		// fresh run and gets its own trigger.
		w.triggeredThisRun = false
		return
	}
	w.log.Info("sync: pushed batch", "remote", w.remote, "count", n)
}

// coalescedPush wraps the injected PushFunc so at most one real trigger
// fires per contiguous run of non-empty drain ticks (#183) -- see
// triggeredThisRun's doc comment for why. A failed push does not set
// triggeredThisRun, so the very next tick retries the real call rather than
// silently treating the failure as "already handled this run".
func (w *Worker) coalescedPush(ctx context.Context, nodes []Node) error {
	if w.triggeredThisRun {
		return nil
	}
	if err := w.push(ctx, nodes); err != nil {
		return err
	}
	w.triggeredThisRun = true
	return nil
}

// enqueueUntracked finds live nodes under exportPath with no row for this
// remote yet and enqueues them. Once a node is PUSHED it drops out of the
// source query, so this never re-queues an already-pushed node.
func (w *Worker) enqueueUntracked(ctx context.Context) {
	rows, err := w.manager.db.Reader.ListLiveNodesForSync(ctx, sqlcgen.ListLiveNodesForSyncParams{
		Remote: w.remote, FilePath: w.exportPath, Limit: int64(w.batchSize),
	})
	if err != nil {
		w.log.Warn("sync: list untracked nodes", "err", err)
		return
	}
	for _, n := range rows {
		node := Node{ID: n.ID}
		if n.FastHash != nil {
			node.Checksum = *n.FastHash
		}
		if err := w.manager.Enqueue(ctx, node, w.remote); err != nil && !errors.Is(err, ErrAlreadyPushed) {
			w.log.Warn("sync: enqueue node", "nodeID", n.ID, "err", err)
		}
	}
}
