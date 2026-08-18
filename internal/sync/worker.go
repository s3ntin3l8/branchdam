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
	push        PushFunc
	log         *slog.Logger
	// wg tracks the run goroutine started by Start so Wait can join it before
	// the database closes during shutdown.
	wg sync.WaitGroup
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
		push: push, log: log}
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
	if n, err := w.manager.RecoverFailedPushes(ctx, w.remote, w.retryWindow); err != nil {
		w.log.Warn("sync: recover failed pushes", "err", err)
	} else if n > 0 {
		w.log.Info("sync: recovered failed pushes for retry", "remote", w.remote, "count", n)
	}
	w.enqueueUntracked(ctx)
	n, err := w.manager.ProcessPending(ctx, w.remote, w.batchSize, w.push)
	if err != nil {
		w.log.Error("sync: drain failed", "remote", w.remote, "err", err)
		return
	}
	if n > 0 {
		w.log.Info("sync: pushed batch", "remote", w.remote, "count", n)
	}
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
