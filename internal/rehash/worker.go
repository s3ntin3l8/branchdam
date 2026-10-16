// Package rehash re-verifies full_hash values that a paired device only
// asserted (#505). An agent event or POST /api/v1/agent/rebase can carry a
// fullHash the server never computed; full_hash_provenance records it as
// 'AGENT', and such a hash authorises neither Tier-1 pruning nor
// node-status "verified". Worker re-reads each such file on a Tier-3 master
// archive location through storage.Guard.OpenRead (the read path: reads
// only, refused outside every configured location) and hashes it with
// hashing.FullHash:
//
//   - a match flips the row to 'SERVER', and the hash starts authorising;
//   - a mismatch is logged and recorded (MISMATCH, with the computed value),
//     and the asserted hash keeps authorising nothing -- the stored
//     media_nodes.full_hash is left as it is for an operator to look at;
//   - a read failure is recorded (ERROR) and retried after RetryAfter.
//
// Bounded: one file at a time, at most BatchSize files per claim query.
// Resumable: every outcome is persisted per row, so a restart skips what is
// already settled and picks up the rest. Shuts down cleanly: Start's loop
// stops on ctx cancellation, an in-flight hash is abandoned mid-read
// without recording anything, and Wait joins the loop before the database
// closes.
package rehash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// DefaultInterval is how long the worker idles between passes once no
// candidate is left. An idle pass is one query over the AGENT partial index.
const DefaultInterval = 5 * time.Minute

// DefaultBatchSize bounds how many files one claim query hands the worker.
const DefaultBatchSize = 16

// DefaultBatchPause is the pause between back-to-back batches while a
// backlog remains (right after the upgrade, every device-created master is
// a candidate), so a large backlog doesn't monopolise the archive's I/O.
const DefaultBatchPause = 2 * time.Second

// DefaultRetryAfter is how long a file that could not be read (unmounted
// share, permission error) waits before it is tried again.
const DefaultRetryAfter = time.Hour

const (
	stateMismatch = "MISMATCH"
	stateError    = "ERROR"
)

// Worker re-hashes agent-asserted Tier-3 full_hash values. See the package
// doc.
type Worker struct {
	db    *db.DB
	guard *storage.Guard
	log   *slog.Logger

	batchSize  int
	batchPause time.Duration
	retryAfter time.Duration
	now        func() time.Time

	// cursor is the last node id handled, so consecutive batches walk the
	// candidates in id order instead of re-claiming a row that just failed.
	// Only the Start goroutine (or a test calling ProcessBatch directly)
	// touches it.
	cursor int64

	wg        sync.WaitGroup
	startOnce sync.Once
}

// Option configures optional Worker behavior.
type Option func(*Worker)

// WithBatchSize overrides DefaultBatchSize.
func WithBatchSize(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.batchSize = n
		}
	}
}

// WithBatchPause overrides DefaultBatchPause.
func WithBatchPause(d time.Duration) Option {
	return func(w *Worker) {
		if d >= 0 {
			w.batchPause = d
		}
	}
}

// WithRetryAfter overrides DefaultRetryAfter.
func WithRetryAfter(d time.Duration) Option {
	return func(w *Worker) {
		if d >= 0 {
			w.retryAfter = d
		}
	}
}

// WithClock overrides time.Now, for tests of the ERROR retry window.
func WithClock(now func() time.Time) Option {
	return func(w *Worker) {
		if now != nil {
			w.now = now
		}
	}
}

// NewWorker creates a re-hash worker. log may be nil (discarding).
func NewWorker(database *db.DB, guard *storage.Guard, log *slog.Logger, opts ...Option) *Worker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	w := &Worker{
		db:         database,
		guard:      guard,
		log:        log,
		batchSize:  DefaultBatchSize,
		batchPause: DefaultBatchPause,
		retryAfter: DefaultRetryAfter,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Stats summarizes one ProcessBatch call.
type Stats struct {
	Matched    int
	Mismatched int
	Errored    int
	// More reports that the batch came back full, so more candidates may
	// be waiting right now.
	More bool
}

// Start begins the background loop and returns immediately. A second call
// is a no-op and logs a warning, mirroring thumbs.Worker.Start.
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
		w.log.Warn("rehash: Worker.Start called after the first start; ignoring")
	}
}

// Wait blocks until the loop started by Start has returned. Called during
// shutdown, after ctx is cancelled and before the database closes. A no-op
// if Start was never called.
func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) run(ctx context.Context, interval time.Duration) {
	for {
		stats, err := w.ProcessBatch(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Warn("rehash: pass failed", "err", err)
		}
		wait := interval
		if err == nil && stats.More {
			wait = w.batchPause
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ProcessBatch claims up to the batch size of candidates after the cursor
// and re-hashes each one. When fewer than a full batch come back, the
// cursor wraps to the start so the next pass sees rows that became
// candidates behind it (and ERROR rows whose retry window has passed).
func (w *Worker) ProcessBatch(ctx context.Context) (Stats, error) {
	var stats Stats
	if w.db == nil || w.guard == nil {
		return stats, errors.New("rehash: database or storage guard not configured")
	}
	rows, err := w.db.Reader.ListRehashCandidates(ctx, sqlcgen.ListRehashCandidatesParams{
		AfterID:     w.cursor,
		RetryBefore: sql.NullInt64{Int64: w.now().Add(-w.retryAfter).Unix(), Valid: true},
		BatchLimit:  int64(w.batchSize),
	})
	if err != nil {
		return stats, fmt.Errorf("list rehash candidates: %w", err)
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		if err := w.processOne(ctx, row, &stats); err != nil {
			return stats, err
		}
		w.cursor = row.ID
	}
	stats.More = len(rows) == w.batchSize
	if !stats.More {
		w.cursor = 0
	}
	return stats, nil
}

func (w *Worker) processOne(ctx context.Context, row sqlcgen.ListRehashCandidatesRow, stats *Stats) error {
	if row.FullHash == nil {
		return nil // the claim query never returns one; defensive
	}
	asserted := *row.FullHash
	computed, hashErr := w.hashFile(ctx, row.FilePath)
	if ctx.Err() != nil {
		// Shutdown mid-read: record nothing, the row stays a candidate.
		return ctx.Err()
	}

	switch {
	case hashErr != nil:
		stats.Errored++
		w.log.Warn("rehash: could not read agent-asserted master; will retry",
			"nodeUuid", row.NodeUuid, "path", row.FilePath, "retryAfter", w.retryAfter, "err", hashErr)
		return w.record(ctx, func(q *sqlcgen.Queries) (int64, error) {
			return q.RecordRehashFailure(ctx, sqlcgen.RecordRehashFailureParams{
				NodeID:      row.ID,
				FullHash:    asserted,
				RehashState: sql.NullString{String: stateError, Valid: true},
			})
		})
	case computed == asserted:
		stats.Matched++
		w.log.Info("rehash: agent-asserted full_hash verified", "nodeUuid", row.NodeUuid, "path", row.FilePath)
		return w.record(ctx, func(q *sqlcgen.Queries) (int64, error) {
			return q.MarkRehashMatched(ctx, sqlcgen.MarkRehashMatchedParams{NodeID: row.ID, FullHash: asserted})
		})
	default:
		stats.Mismatched++
		w.log.Warn("rehash: agent-asserted full_hash does not match the file; it will not authorise pruning",
			"nodeUuid", row.NodeUuid, "path", row.FilePath, "asserted", asserted, "computed", computed)
		return w.record(ctx, func(q *sqlcgen.Queries) (int64, error) {
			return q.RecordRehashFailure(ctx, sqlcgen.RecordRehashFailureParams{
				NodeID:         row.ID,
				FullHash:       asserted,
				RehashState:    sql.NullString{String: stateMismatch, Valid: true},
				RehashComputed: sql.NullString{String: computed, Valid: true},
			})
		})
	}
}

// record applies one outcome in its own short writer transaction. Zero
// rows affected means the row changed while the file was being read (a
// new hash value, or a scan already verified it); that is not an error,
// and the newer state stands.
func (w *Worker) record(ctx context.Context, fn func(*sqlcgen.Queries) (int64, error)) error {
	_, err := w.db.InTxResult(ctx, fn)
	if err != nil {
		return fmt.Errorf("record rehash outcome: %w", err)
	}
	return nil
}

// hashFile opens path through the Guard's read path and hashes it,
// abandoning the read as soon as ctx is cancelled.
func (w *Worker) hashFile(ctx context.Context, path string) (string, error) {
	f, err := w.guard.OpenRead(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return hashing.FullHash(&ctxReader{ctx: ctx, r: f})
}

// ctxReader fails the next Read once ctx is cancelled, so shutdown never
// waits for a multi-gigabyte master to finish hashing.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
