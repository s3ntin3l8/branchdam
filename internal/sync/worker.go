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
}

// maxSubBatchesPerTick bounds how many batchSize-sized sub-batches drain's
// internal loop will claim and mark-status in one tick (#183), regardless of
// where the PENDING_CLOUD_PUSH rows came from (new discovery or a mass
// PUSH_FAILED recovery) -- see drain's doc comment for why this matters.
// Reuses enqueueDiscoveryMultiplier's value so "how much backlog one tick
// will fully absorb" stays one coherent number (batchSize *
// enqueueDiscoveryMultiplier nodes) rather than two independently-tuned caps.
const maxSubBatchesPerTick = enqueueDiscoveryMultiplier

// enqueueDiscoveryMultiplier makes enqueueUntracked's per-tick discovery
// limit a multiple of batchSize rather than equal to it (#183) -- see
// enqueueUntracked's doc comment for why.
const enqueueDiscoveryMultiplier = 10

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
	// failure doesn't strand a batch forever. Then enqueue brand-new nodes --
	// exactly once, before the loop below, which is what makes that loop's
	// coalescing safe (see its comment).
	if n, err := w.manager.RecoverFailedPushes(ctx, w.remote, w.retryWindow, w.maxRetries); err != nil {
		w.log.Warn("sync: recover failed pushes", "err", err)
	} else if n > 0 {
		w.log.Info("sync: recovered failed pushes for retry", "remote", w.remote, "count", n)
	}
	// Every tick, not just the one that first crosses the bound: a row stuck
	// PUSH_FAILED at maxRetries produces zero output from RecoverFailedPushes
	// above forever after (it drops out of that query's claim set), so this is
	// the only recurring signal an operator gets that a node's sync has
	// permanently stopped short of querying SQLite directly (#182).
	if n, err := w.manager.CountExhaustedPushes(ctx, w.remote, w.maxRetries); err != nil {
		w.log.Warn("sync: count exhausted pushes", "remote", w.remote, "err", err)
	} else if n > 0 {
		w.log.Warn("sync: rows permanently abandoned at retry bound", "remote", w.remote, "count", n, "maxRetries", w.maxRetries)
	}
	w.enqueueUntracked(ctx)

	// Drain every row PENDING as of the enqueueUntracked call above through
	// at most one real push call, claiming in batchSize-sized sub-batches so
	// no single write transaction grows unbounded (#183). This differs from
	// the cross-tick coalescing it replaced: enqueueUntracked runs exactly
	// once per drain() call, before this loop starts, so every row the loop
	// claims was already on disk before the one real push call fires. A
	// node whose file lands after this tick's enqueueUntracked snapshot
	// isn't enqueued yet -- it can't be swept into this tick's push
	// coverage by the noop sub-batches below -- so it simply waits for its
	// own tick's enqueueUntracked call and gets its own real push there.
	//
	// This assumes push is a whole-mount-rescan operation (e.g. Immich's
	// TriggerScan) that doesn't need to know which specific nodes prompted
	// it -- true for every remote wired today. A future per-node PushFunc
	// must not reuse this loop as-is: nodes claimed by the noop sub-batches
	// are marked PUSHED without ever reaching push.
	//
	// The loop is capped at maxSubBatchesPerTick iterations. Without a cap,
	// a large RecoverFailedPushes recovery (ResetRemoteSyncStateFailed has
	// no row limit -- an extended Immich outage can leave thousands of rows
	// PUSH_FAILED with similar last_attempt_at timestamps, all reset to
	// PENDING_CLOUD_PUSH by one tick's call above) would make this loop
	// claim and mark-status thousands of sub-batches back to back, holding
	// the single writer connection against scans and edge confirms for the
	// whole tick. Whatever the cap doesn't finish this tick stays
	// PENDING_CLOUD_PUSH and is drained -- with its own fresh real push --
	// by a later tick; nothing is lost or coalesced away by leaving it.
	triggered := false
	total := 0
	cappedWithMoreLeft := false
	for i := 0; i < maxSubBatchesPerTick; i++ {
		pushFn := w.push
		if triggered {
			pushFn = noopPush
		}
		n, err := w.manager.ProcessPending(ctx, w.remote, w.batchSize, pushFn)
		if err != nil {
			w.log.Error("sync: drain failed", "remote", w.remote, "err", err)
			return
		}
		if n == 0 {
			break
		}
		triggered = true
		total += n
		if n < w.batchSize {
			break // a partial batch means nothing more is claimable right now
		}
		cappedWithMoreLeft = i == maxSubBatchesPerTick-1
	}
	if total > 0 {
		// "count" is how many rows this tick marked PUSHED, not how many
		// individually reached the real push call -- only the first
		// sub-batch did (see the coalescing comment above).
		w.log.Info("sync: marked batch pushed", "remote", w.remote, "count", total)
	}
	if cappedWithMoreLeft {
		// The last allowed iteration claimed a full batch, so more rows may
		// still be PENDING -- not a guarantee (the backlog could have been
		// exactly the cap size), but worth a log line either way so an
		// operator watching a large recovery isn't surprised it spans ticks.
		w.log.Info("sync: drain tick hit its per-tick claim cap, backlog may continue next tick",
			"remote", w.remote, "capNodes", w.batchSize*maxSubBatchesPerTick)
	}
}

// noopPush lets drain's internal loop mark further sub-batches PUSHED
// without a redundant real trigger call, once the tick's one real push has
// already fired -- see drain's doc comment for the safety argument.
func noopPush(context.Context, []Node) error { return nil }

// enqueueUntracked finds live nodes under exportPath with no row for this
// remote yet and enqueues them. Once a node is PUSHED it drops out of the
// source query, so this never re-queues an already-pushed node.
//
// The discovery limit is a multiple of batchSize, not equal to it (#183): a
// large backlog (e.g. a fresh export mount with 100k files) needs to be
// enqueued in as few ticks as possible for drain's internal loop above to
// actually coalesce it into one real push per tick, rather than spending one
// tick -- and one real trigger -- per batchSize newly-discovered nodes as
// before. Enqueue runs its own read+upsert write transaction per node (see
// its doc comment), so the multiplier is kept modest rather than unbounded,
// to bound how long one tick holds the single writer connection against
// other write paths (scans, edge confirms).
func (w *Worker) enqueueUntracked(ctx context.Context) {
	rows, err := w.manager.db.Reader.ListLiveNodesForSync(ctx, sqlcgen.ListLiveNodesForSyncParams{
		Remote: w.remote, FilePath: w.exportPath, Limit: int64(w.batchSize * enqueueDiscoveryMultiplier),
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
