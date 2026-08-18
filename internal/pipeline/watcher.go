package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/indexer"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// watchDebounce is how long indexer.Watch waits for a path to go quiet
// before firing an event -- editors flush in bursts, and one callback per
// real save is what we want, not one per write syscall.
const watchDebounce = 500 * time.Millisecond

// WatcherSupervisor runs one indexer.Watch per watched storage location on
// the server's signal context. Each watched location gets a long-lived
// scan_jobs row with kind='WATCH' that stays RUNNING until shutdown (-
// > CANCELLED) or the watcher dies (-> FAILED). Tier 3 locations are never
// watched -- main.go filters before Start.
//
// Every location's events are processed by a single consumer goroutine
// reading an unbuffered channel, so hashing/Commit run one at a time per
// location instead of in unbounded parallel goroutines. The consumer makes
// no ordering assumptions about events: the debouncer's per-path
// time.AfterFunc goroutines can deliver them in any order, and inotify on
// Linux actually delivers a rename's IN_MOVED_TO before its IN_MOVED_FROM,
// so rename move detection is deliberately order-independent (see
// rebaseIfMoved). A file-level rename IS move-detected; a recursive
// DIRECTORY rename is not (the fsnotify watch moves with the directory, so
// no per-file remove/create events fire) -- files under it land as fresh
// nodes on the next full scan and the old nodes sweep to MISSING, the same
// fidelity gap as the pure-scan path: self-healing, no data loss. Batching
// Commit calls the way a full scan does (see scan.go's batchSize/
// batchInterval) would be a reasonable follow-up; the per-location
// serialization itself is deliberate.
//
// Watch-path events run processFile, so videos on a watched location go
// through probe.FFProbe under the same probeTimeout budget as the scan path
// and degrade gracefully when ffprobe fails or is absent -- a deliberate
// consistency with the scan path, not an oversight. A video burst serialized
// behind one consumer can stall ingestion and re-probe content-identical
// touches; a shorter watch-path budget is a possible future refinement.
type WatcherSupervisor struct {
	deps      ScanDeps
	log       *slog.Logger
	nudge     func() // guest-side SSE hub.Broadcast, wired from main.go; may be nil
	wg        sync.WaitGroup
	startOnce sync.Once
}

func NewWatcherSupervisor(deps ScanDeps, nudge func()) *WatcherSupervisor {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &WatcherSupervisor{deps: deps, log: log, nudge: nudge}
}

// Start begins one watch goroutine per location and returns immediately.
// Goroutines block until ctx is cancelled (-> CANCELLED) or their watcher
// dies (-> FAILED). A second call is a no-op and logs a warning.
func (w *WatcherSupervisor) Start(ctx context.Context, locations []storage.Location, debounce time.Duration) {
	if debounce <= 0 {
		debounce = watchDebounce
	}
	started := false
	w.startOnce.Do(func() {
		started = true
		for _, loc := range locations {
			w.wg.Add(1)
			go func(l storage.Location) {
				defer w.wg.Done()
				w.watchLocation(ctx, l, debounce)
			}(loc)
		}
	})
	if !started {
		w.log.Warn("pipeline: WatcherSupervisor.Start called after the first start; ignoring", "locations", len(locations))
	}
}

// Wait blocks until every watch goroutine has returned. Called during
// shutdown, after the fsnotify context is cancelled and before pool.Drain
// and db.Close (see main.go).
func (w *WatcherSupervisor) Wait() { w.wg.Wait() }

// watchItem is one thing that happened under a watched location: either a
// file event (rec) or a removal (remove=true, only path is meaningful).
type watchItem struct {
	remove bool
	path   string
	rec    indexer.Record
}

// watchWork is the handoff between indexer.Watch's debounce callbacks and a
// location's consumer goroutine. The callbacks only ever enqueue; the
// consumer is the sole reader and the only code that touches the database.
// enqueue blocks until the consumer takes the item, which doubles as
// backpressure: while the consumer is busy hashing, further events wait
// instead of spawning more goroutines. The order items land on the channel
// carries no semantic weight -- the debouncer fires one time.AfterFunc
// goroutine per path and Go does not guarantee their order, so consumers
// must not assume any event ordering (rename move detection is deliberately
// order-independent, see rebaseIfMoved).
//
// close() records the intent to stop under the same mutex enqueue uses
// before the channel itself is closed, so a blocked send can never race the
// close: it either hands off or sees closed and bails. The caller must join
// the consumer (consumerWG.Wait) after close() and before any finalization.
type watchWork struct {
	mu     sync.Mutex
	closed bool
	ch     chan watchItem
}

func (q *watchWork) enqueue(item watchItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.ch <- item
}

func (q *watchWork) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	close(q.ch)
}

// watchLocation owns one location's watch lifecycle: creates the WATCH
// scan_jobs row, runs indexer.Watch feeding a serialized consumer, and
// finalizes the job on exit.
func (w *WatcherSupervisor) watchLocation(ctx context.Context, loc storage.Location, debounce time.Duration) {
	var job sqlcgen.ScanJob
	if err := w.deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
		j, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: loc.ID, Valid: true},
			Kind:              "WATCH",
		})
		if err != nil {
			return err
		}
		job = j
		return nil
	}); err != nil {
		w.log.Error("pipeline: create watch job", "location", loc.RootPath, "err", err)
		return
	}

	work := &watchWork{ch: make(chan watchItem)}
	var consumerWG sync.WaitGroup
	consumerWG.Add(1)
	go func() {
		defer consumerWG.Done()
		var seen, hashed, failed atomic.Int32
		for item := range work.ch {
			// Every item updates the job's counters -- a failure must still
			// be persisted (files_failed), not dropped with the callback.
			// bump only when handleWatchItem reports the item was handled: a
			// nudge means "something changed" (a failed event or a removal
			// with nothing to mark is not a change).
			bumped := w.handleWatchItem(ctx, loc, item, &seen, &hashed, &failed)
			w.updateJob(job.ID, &seen, &hashed, &failed)
			if bumped {
				w.bump()
			}
		}
	}()

	watchErr := indexer.Watch(ctx, loc.RootPath, debounce, w.log,
		func(rec indexer.Record) error {
			if rec.IsSymlink {
				return nil
			}
			work.enqueue(watchItem{rec: rec})
			return nil
		},
		func(path string) error {
			work.enqueue(watchItem{remove: true, path: path})
			return nil
		},
	)

	// Drain the queue and join the consumer before touching the job row: the
	// consumer must never run after finalize. Enqueues past close() are
	// dropped -- the fsnotify context is done by now, there is nothing left
	// to process.
	work.close()
	consumerWG.Wait()

	state, reason := "CANCELLED", "shutting down"
	if watchErr != nil && !errors.Is(watchErr, context.Canceled) && ctx.Err() == nil {
		state, reason = "FAILED", watchErr.Error()
		w.log.Error("pipeline: watcher died", "location", loc.RootPath, "err", watchErr)
	}
	// context.Background() so a cancelled event ctx can't orphan the row.
	if ferr := w.deps.DB.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		if state == "FAILED" {
			return q.FailScanJob(context.Background(), sqlcgen.FailScanJobParams{
				ID: job.ID, LastError: sql.NullString{String: reason, Valid: true},
			})
		}
		return q.CancelScanJob(context.Background(), job.ID)
	}); ferr != nil {
		w.log.Warn("pipeline: finalize watch job", "jobID", job.ID, "err", ferr)
	}
}

// handleWatchItem processes one watchItem to completion, returning whether
// the frontend should be nudged. Events and removals each count against
// files_seen; a handled item bumps, a failure does not (and a removal with
// nothing to mark is not a nudge -- nothing changed).
func (w *WatcherSupervisor) handleWatchItem(ctx context.Context, loc storage.Location, item watchItem, seen, hashed, failed *atomic.Int32) bool {
	if item.remove {
		return w.handleRemoval(ctx, item.path, seen, failed)
	}
	seen.Add(1)
	result, err := processFile(ctx, w.deps, loc, item.rec)
	if err != nil {
		failed.Add(1)
		w.log.Warn("pipeline: watch process file", "path", item.rec.Path, "err", err)
		return false
	}
	// A rename's create event can arrive before its removal event (inotify
	// emits IN_MOVED_TO before IN_MOVED_FROM on Linux); when the old file is
	// already gone from disk this rebases the old node instead of inserting a
	// duplicate. See rebaseIfMoved.
	moved, err := w.rebaseIfMoved(ctx, loc, result)
	if err != nil {
		failed.Add(1)
		return false
	}
	if moved {
		hashed.Add(1)
	} else {
		stats, err := Commit(ctx, w.deps.DB, loc.ID, []Result{*result}, w.log)
		if err != nil {
			failed.Add(1)
			w.log.Warn("pipeline: watch commit", "path", item.rec.Path, "err", err)
			return false
		}
		hashed.Add(int32(stats.Inserted + stats.Touched + stats.VersionCollisions + stats.Moved))
	}
	if w.deps.Engine != nil {
		resolveEdgesForBatch(ctx, w.deps, []Result{*result}, w.log)
	}
	return true
}

// rebaseIfMoved recognizes a rename when the create event reaches the
// consumer before the removal event does. Commit's own move detection only
// matches nodes already marked MISSING, but here the old node is still
// ACTIVE because its removal hasn't been processed yet. The distinguishing
// fact is the filesystem: a live node at a DIFFERENT path with the same
// fast_hash whose file no longer exists on disk is the moved-from node, so
// it is rebased onto the new path in place (id and node_uuid never change,
// every edge survives -- same semantics as Pillar 5 move detection). A node
// whose file is still present is a genuine same-content duplicate or a T1
// fast_hash collision and is deliberately left alone.
func (w *WatcherSupervisor) rebaseIfMoved(ctx context.Context, loc storage.Location, result *Result) (bool, error) {
	// A live node at this exact path means Commit will touch it or version-
	// collide it, never move -- skip.
	if _, err := w.deps.DB.Reader.GetLiveNodeByPath(ctx, result.Path); err == nil {
		return false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("get live node by path: %w", err)
	}
	nodes, err := w.deps.DB.Reader.ListLiveNodesByFastHash(ctx, &result.FastHash)
	if err != nil {
		return false, fmt.Errorf("list live nodes by fast hash: %w", err)
	}
	for _, n := range nodes {
		if n.FilePath == result.Path {
			continue
		}
		if _, err := os.Lstat(n.FilePath); !errors.Is(err, fs.ErrNotExist) {
			continue // still on disk: duplicate or collision, not a move
		}
		if n.FullHash != nil && result.FullHash != "" && *n.FullHash != result.FullHash {
			continue // genuinely different content sharing a fast_hash (T1)
		}
		// TOCTOU: the GetLiveNodeByPath pre-check above read on the reader
		// pool, this rebase writes later on the single writer connection. A
		// concurrent full scan committing a node at result.Path in between
		// trips the partial unique index on file_path, RebaseMissingNodePath
		// fails, and the event is counted failed and picked up by the next
		// scan. Fail-safe, never corrupting: the worst case is a deferred
		// (re-)index, not a wrong row.
		if err := w.deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
			if err := q.RebaseMissingNodePath(ctx, sqlcgen.RebaseMissingNodePathParams{
				ID:                n.ID,
				FilePath:          result.Path,
				FileName:          result.FileName,
				StorageLocationID: loc.ID,
				MtimeUnix:         result.ModTime.Unix(),
			}); err != nil {
				return err
			}
			// Same backfill as Commit's own touched/rebase branches -- see
			// commit.go's persistAllMetadata doc comment and #86.
			return persistAllMetadata(ctx, q, n.ID, *result, w.log)
		}); err != nil {
			return false, fmt.Errorf("rebase moved node %d: %w", n.ID, err)
		}
		return true, nil
	}
	return false, nil
}

// handleRemoval marks the live node at path MISSING, returning whether the
// frontend should be nudged. It is a no-op when there is nothing to mark
// (never indexed, or already MISSING -- e.g. a rename whose node was already
// rebound by rebaseIfMoved); only a real ACTIVE -> MISSING transition bumps.
func (w *WatcherSupervisor) handleRemoval(ctx context.Context, path string, seen, failed *atomic.Int32) bool {
	seen.Add(1)
	node, err := w.deps.DB.Reader.GetLiveNodeByPath(ctx, path)
	if errors.Is(err, sql.ErrNoRows) {
		return false // never indexed, or already rebound -- nothing changed
	}
	if err != nil {
		failed.Add(1)
		w.log.Warn("pipeline: watch removal lookup", "path", path, "err", err)
		return false
	}
	if node.LifecycleState == "MISSING" {
		return false // already missing -- nothing to transition
	}
	if err := w.deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkNodeMissing(ctx, node.ID)
	}); err != nil {
		failed.Add(1)
		w.log.Warn("pipeline: watch removal", "path", path, "err", err)
		return false
	}
	return true
}

func (w *WatcherSupervisor) updateJob(jobID int64, seen, hashed, failed *atomic.Int32) {
	// EdgesCreated is left at its zero value here -- UpdateScanJobProgress
	// SETs an absolute value, not an increment, and the watch path never
	// calls graph.Engine.ResolveAndCommit, so 0 is correct today. If a
	// future change wires edge resolution into the watcher, this call site
	// will need its own accumulated count threaded in the same way
	// runScan's drainAndCommit does (see scan.go), or every watch-job
	// progress update will silently reset edges_created back to 0.
	if err := w.deps.DB.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		return q.UpdateScanJobProgress(context.Background(), sqlcgen.UpdateScanJobProgressParams{
			ID: jobID, FilesSeen: int64(seen.Load()), FilesHashed: int64(hashed.Load()), FilesFailed: int64(failed.Load()),
		})
	}); err != nil {
		w.log.Warn("pipeline: update watch job progress", "jobID", jobID, "err", err)
	}
}

func (w *WatcherSupervisor) bump() {
	if w.nudge != nil {
		w.nudge()
	}
}
