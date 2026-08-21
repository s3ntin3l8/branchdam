package thumbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// DefaultInterval is how often Start polls media_nodes for newly PENDING
// thumbnails between passes, when the caller doesn't override it. An idle
// tick costs one indexed query (ListPendingThumbnails, backed by 00007's
// partial index), the same shape as agent.Drainer's DefaultDrainInterval.
const DefaultInterval = 5 * time.Second

// DefaultMaxAttempts bounds how many times the worker retries generating a
// thumbnail for one node before leaving it FAILED for good -- mirrors
// remote_sync_state's retry_count bound (internal/sync).
const DefaultMaxAttempts = 5

// DefaultBatchSize is how many PENDING nodes ListPendingThumbnails claims
// per pass.
const DefaultBatchSize = 20

// Worker generates and persists thumbnails for PENDING media_nodes rows,
// polling on an interval. Modeled directly on internal/agent.Drainer.
type Worker struct {
	db    *db.DB
	cache *Cache
	log   *slog.Logger

	maxAttempts int
	batchSize   int
	concurrency int
	nudge       func()

	wg        sync.WaitGroup
	startOnce sync.Once
}

// WorkerOption configures optional Worker behavior not every caller needs.
type WorkerOption func(*Worker)

// WithNudge sets the callback ProcessPending calls after a pass changes at
// least one node's thumb_state. Typically hub.Broadcast (internal/sse).
func WithNudge(nudge func()) WorkerOption {
	return func(w *Worker) { w.nudge = nudge }
}

// WithMaxAttempts overrides DefaultMaxAttempts.
func WithMaxAttempts(n int) WorkerOption {
	return func(w *Worker) {
		if n > 0 {
			w.maxAttempts = n
		}
	}
}

// WithBatchSize overrides DefaultBatchSize.
func WithBatchSize(n int) WorkerOption {
	return func(w *Worker) {
		if n > 0 {
			w.batchSize = n
		}
	}
}

// WithConcurrency overrides the goroutine count ProcessPending fans a batch
// out across. Generate is CPU/exiftool-subprocess-bound, so this is a real
// speedup; the DB write each node ends with still serializes through the
// writer pool's single connection (docs/schema.md fix #7) regardless, via
// database/sql's own connection-checkout blocking -- no additional locking
// needed here. n <= 0 is ignored (keeps the constructor's default).
func WithConcurrency(n int) WorkerOption {
	return func(w *Worker) {
		if n > 0 {
			w.concurrency = n
		}
	}
}

// NewWorker creates a thumbnail generation worker. log may be nil (defaults
// to discarding); opts are typically WithNudge and WithConcurrency in
// production, omitted in tests that only need ProcessPending's own return
// value. Concurrency defaults to min(4, NumCPU), mirroring
// config.Workers.HashWorkers' "0 = auto" convention.
func NewWorker(database *db.DB, cache *Cache, log *slog.Logger, opts ...WorkerOption) *Worker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := &Worker{
		db:          database,
		cache:       cache,
		log:         log,
		maxAttempts: DefaultMaxAttempts,
		batchSize:   DefaultBatchSize,
		concurrency: min(4, runtime.NumCPU()),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Start begins the background generation loop and returns immediately: a
// ticker fires ProcessPending every interval (DefaultInterval if <= 0)
// until ctx is cancelled. A second call is a no-op and logs a warning,
// mirroring agent.Drainer.Start/pipeline.WatcherSupervisor.Start.
func (w *Worker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	started := false
	w.startOnce.Do(func() {
		started = true
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.run(ctx, interval)
		}()
	})
	if !started {
		w.log.Warn("thumbs: Worker.Start called after the first start; ignoring")
	}
}

// Wait blocks until the background loop started by Start has returned --
// called during shutdown, after ctx is cancelled, before pool.Drain and
// db.Close, mirroring agent.Drainer.Wait. A no-op if Start was never
// called.
func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) run(ctx context.Context, interval time.Duration) {
	// An immediate pass before the ticker's first tick, so a node that
	// became PENDING right before/during startup isn't left waiting up to
	// a full interval before it's first considered.
	if _, err := w.ProcessPending(ctx); err != nil && ctx.Err() == nil {
		w.log.Warn("thumbs: generation pass failed", "err", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.ProcessPending(ctx); err != nil && ctx.Err() == nil {
				w.log.Warn("thumbs: generation pass failed", "err", err)
			}
		}
	}
}

// Stats summarizes one ProcessPending pass.
type Stats struct {
	Generated   int
	Unsupported int
	Failed      int
}

// ProcessPending claims up to the worker's batch size PENDING nodes
// oldest-first and generates a thumbnail for each.
func (w *Worker) ProcessPending(ctx context.Context) (Stats, error) {
	var stats Stats

	var nodes []sqlcgen.ListPendingThumbnailsRow
	err := w.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		nodes, err = q.ListPendingThumbnails(ctx, sqlcgen.ListPendingThumbnailsParams{
			ThumbAttempts: int64(w.maxAttempts),
			Limit:         int64(w.batchSize),
		})
		return err
	})
	if err != nil {
		return stats, fmt.Errorf("list pending thumbnails: %w", err)
	}

	// Generate/encode is CPU- and exiftool-subprocess-bound, so a batch fans
	// out across w.concurrency goroutines; each node's own DB write still
	// serializes through the writer pool's single connection regardless
	// (database/sql's connection-checkout blocking handles that without any
	// extra locking here). ctx cancellation is checked before dispatching
	// each node, not just once up front, so a shutdown mid-batch stops
	// starting new work promptly rather than draining the whole batch.
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, w.concurrency)
		changed   int
		cancelErr error
	)
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			cancelErr = err
			break
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(node sqlcgen.ListPendingThumbnailsRow) {
			defer wg.Done()
			defer func() { <-sem }()

			result := w.processOne(ctx, node)

			mu.Lock()
			defer mu.Unlock()
			switch result {
			case "READY":
				stats.Generated++
				changed++
			case "UNSUPPORTED":
				stats.Unsupported++
				changed++
			case "FAILED":
				stats.Failed++
				changed++
			}
		}(node)
	}
	wg.Wait()

	// Nudge once per batch, not per node -- sse.Hub.Broadcast is already a
	// coalescing signal (internal/sse's own doc comment), so per-node calls
	// would be harmless but wasteful. Only fires when the batch actually
	// changed visible state -- including a partial batch cut short by
	// cancellation below, so a shutdown mid-pass doesn't strand whatever
	// progress it did make unreflected in the SPA until the next pass.
	if w.nudge != nil && changed > 0 {
		w.nudge()
	}

	if cancelErr != nil {
		return stats, cancelErr
	}
	return stats, nil
}

// processOne generates and persists a thumbnail for one node, returning the
// thumb_state it landed in ("READY"/"UNSUPPORTED"/"FAILED"), or "" if the DB
// write itself failed (logged; not counted in Stats, since the node's
// thumb_state may not have actually changed).
func (w *Worker) processOne(ctx context.Context, node sqlcgen.ListPendingThumbnailsRow) string {
	data, genErr := w.cache.Generate(ctx, node.FilePath)
	if genErr == nil {
		if writeErr := w.cache.Write(node.NodeUuid, data); writeErr != nil {
			genErr = fmt.Errorf("write cached thumbnail: %w", writeErr)
		}
	}

	switch {
	case genErr == nil:
		if err := w.setState(ctx, node.ID, "READY", 0); err != nil {
			w.log.Error("thumbs: mark READY in db", "nodeID", node.ID, "err", err)
			return ""
		}
		return "READY"

	case errors.Is(genErr, ErrUnsupported):
		// Attempts reset to 0 -- UNSUPPORTED is terminal (excluded from
		// ListPendingThumbnails by thumb_state alone), not a candidate for
		// the retry-bound FAILED path below, so the count carries no
		// further meaning once here.
		if err := w.setState(ctx, node.ID, "UNSUPPORTED", 0); err != nil {
			w.log.Error("thumbs: mark UNSUPPORTED in db", "nodeID", node.ID, "err", err)
			return ""
		}
		return "UNSUPPORTED"

	default:
		attempts := node.ThumbAttempts + 1
		w.log.Warn("thumbs: generate failed", "nodeID", node.ID, "filePath", node.FilePath,
			"attempts", attempts, "maxAttempts", w.maxAttempts, "err", genErr)
		if err := w.setState(ctx, node.ID, "FAILED", attempts); err != nil {
			w.log.Error("thumbs: mark FAILED in db", "nodeID", node.ID, "err", err)
			return ""
		}
		return "FAILED"
	}
}

func (w *Worker) setState(ctx context.Context, nodeID int64, state string, attempts int64) error {
	return w.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.SetThumbState(ctx, sqlcgen.SetThumbStateParams{
			ID: nodeID, ThumbState: state, ThumbAttempts: attempts,
		})
	})
}
