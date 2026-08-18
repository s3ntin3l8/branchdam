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
// reading a bounded, drop-oldest queue (watchWork -- see its own doc
// comment for why bounded and why drop-oldest), so hashing/Commit run one
// at a time per location instead of in unbounded parallel goroutines, and
// a debounce-timer goroutine handing off an event never blocks or parks
// waiting for the consumer. The consumer makes no ordering assumptions
// about events: the debouncer's per-path time.AfterFunc goroutines can
// deliver them in any order, and inotify on Linux actually delivers a
// rename's IN_MOVED_TO before its IN_MOVED_FROM, so rename move detection
// is deliberately order-independent (see rebaseIfMoved). A file-level
// rename IS move-detected; a recursive DIRECTORY rename is not (the
// fsnotify watch moves with the directory, so no per-file remove/create
// events fire) -- files under it land as fresh nodes on the next full scan
// and the old nodes sweep to MISSING, the same fidelity gap as the
// pure-scan path: self-healing, no data loss. Batching Commit calls the way
// a full scan does (see scan.go's batchSize/batchInterval) would be a
// reasonable follow-up; the per-location serialization itself is
// deliberate.
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

// watchQueueCapacity bounds watchWork's backlog. Matches workers.Pool's
// queueDepth default (cmd/branchdam/main.go) for consistency -- both are
// "how much backlog can pile up behind a slower consumer" bounds. Once this
// many items are queued and not yet drained, enqueue evicts the OLDEST
// queued item (see watchWork's doc comment) rather than growing without
// bound or blocking the caller.
const watchQueueCapacity = 1024

// watchWork is the bounded handoff between indexer.Watch's debounce
// callbacks and a location's consumer goroutine. The callbacks only ever
// enqueue; the consumer is the sole reader and the only code that touches
// the database.
//
// enqueue never blocks and never spawns/parks a goroutine: it either
// appends to the backlog (a plain slice under a mutex, not a channel), or,
// once the backlog reaches watchQueueCapacity, evicts the single oldest
// queued item to make room. That was the actual bug this replaces -- the
// prior design held q.mu for the entire duration of a blocking channel
// send, so under sustained backpressure (a burst of, say, 5000 files from
// an rsync, each costing up to three sequential 30s probe budgets to
// drain) every fired debounce timer's goroutine parked on that mutex, one
// per event, unboundedly. Drop-oldest is the deliberate backpressure
// policy, not block-with-a-cap or drop-newest: a full scan is this
// package's existing self-healing catch-up path for anything a watch event
// misses (the same fallback already relied on for un-watched directory
// renames, per this file's package doc), so losing the STALEST queued
// event under sustained overload -- keeping the most recent activity
// flowing -- is preferable to either blocking new fsnotify delivery
// (risking the kernel's own event queue overflowing, see
// indexer.ErrEventOverflow) or discarding the newest event a caller just
// asked to enqueue. Every eviction is counted in dropped (readable via
// droppedCount for tests and any future surfacing) and rate-limited-logged
// -- see enqueue's logDropThreshold/logDropSummaryEvery comment -- not
// silent, but not one WARN line per eviction either.
//
// The order items land in the backlog carries no semantic weight -- the
// debouncer fires one time.AfterFunc goroutine per path and Go does not
// guarantee their order, so consumers must not assume any event ordering
// (rename move detection is deliberately order-independent, see
// rebaseIfMoved).
//
// close() records the intent to stop and closes notify, both under the
// same mutex enqueue's own append-and-signal uses -- so a concurrent
// enqueue's non-blocking send on notify can never race close()'s close of
// that same channel (a send racing a close panics: "send on closed
// channel"). Serializing both through q.mu is what rules that out: either
// enqueue's whole critical section (closed-check, append, notify send)
// completes before close() ever acquires the lock, or close() runs first
// and enqueue's closed-check (also inside the lock) sees it and bails
// before ever touching notify -- there is no third interleaving. dequeue's
// own receive from notify is unaffected either way: a receive from an
// already-closed channel is always safe in Go, unlike a send. The caller
// must join the consumer (consumerWG.Wait) after close() and before any
// finalization.
type watchWork struct {
	mu     sync.Mutex
	closed bool
	// items' pop-front (both here and in dequeue) via items[1:] shrinks the
	// slice's capacity by one each time without reusing that space, so
	// sustained enqueue/dequeue cycling forces a full backing-array
	// reallocation roughly every watchQueueCapacity operations. Bounded and
	// GC'd -- not a leak, not correctness-affecting -- just not truly O(1)
	// amortized the way a ring buffer would be; a possible future
	// refinement if this ever shows up in profiling.
	items    []watchItem
	notify   chan struct{} // buffered 1: coalescing wakeup, not one-signal-per-item
	dropped  atomic.Int64
	log      *slog.Logger
	location string // loc.RootPath, for attributing eviction logs in a multi-location deployment
}

func newWatchWork(log *slog.Logger, location string) *watchWork {
	return &watchWork{notify: make(chan struct{}, 1), log: log, location: location}
}

// logDropThreshold caps how many individual eviction warnings enqueue logs
// before switching to a periodic summary: a burst large enough to evict
// thousands of items (the PR's own 5000-file example, against a 1024
// capacity, evicts ~4000) would otherwise emit one WARN line per eviction --
// itself a form of the log-spam problem this PR's drop-oldest policy exists
// to avoid causing elsewhere. The first few individual lines still show an
// operator concrete dropped paths; beyond that, a running total every
// logDropSummaryEvery evictions stays visible without flooding.
const (
	logDropThreshold    = 10
	logDropSummaryEvery = 100
)

func (q *watchWork) enqueue(item watchItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(q.items) >= watchQueueCapacity {
		oldest := q.items[0]
		q.items = q.items[1:]
		dropped := q.dropped.Add(1)
		if q.log != nil {
			switch {
			case dropped <= logDropThreshold:
				q.log.Warn("pipeline: watch queue full, dropping oldest queued event -- run a full rescan of this location to catch up",
					"location", q.location, "dropped_path", oldest.path, "dropped_rec_path", oldest.rec.Path, "capacity", watchQueueCapacity)
			case dropped%logDropSummaryEvery == 0:
				q.log.Warn("pipeline: watch queue still under sustained pressure, dropping oldest events -- run a full rescan of this location to catch up",
					"location", q.location, "dropped_total_this_run", dropped, "capacity", watchQueueCapacity)
			}
		}
	}
	q.items = append(q.items, item)
	// Send while still holding q.mu -- see the type doc comment for why
	// this, not a send after unlocking, is what makes close() race-free.
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// backlogLen reports how many items are currently queued -- for tests and
// any future observability; not used in enqueue/dequeue's own logic.
func (q *watchWork) backlogLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// dequeue blocks until an item is available, returning ok=false only once
// the queue is both closed and drained.
func (q *watchWork) dequeue() (watchItem, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return watchItem{}, false
		}
		<-q.notify
	}
}

func (q *watchWork) droppedCount() int64 { return q.dropped.Load() }

func (q *watchWork) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	// Closing notify while still holding q.mu, not after unlocking, is what
	// serializes this against enqueue's own notify send -- see the type
	// doc comment.
	close(q.notify)
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

	work := newWatchWork(w.log, loc.RootPath)
	var consumerWG sync.WaitGroup
	consumerWG.Add(1)
	go func() {
		defer consumerWG.Done()
		var seen, hashed, failed atomic.Int32
		for {
			item, ok := work.dequeue()
			if !ok {
				// Persist the final counts, including any items abandoned
				// during a fast shutdown drain -- their bookkeeping wasn't
				// written per-item (see consumeOne).
				w.updateJob(job.ID, &seen, &hashed, &failed)
				return
			}
			bumped, abandoned := w.consumeOne(ctx, loc, item, &seen, &hashed, &failed)
			if abandoned {
				// No per-item updateJob call here -- that would reintroduce
				// the same shutdown-drain delay via DB round-trips instead
				// of file I/O. The final counts land in one updateJob call
				// once the backlog is fully drained, above.
				continue
			}
			// Every item updates the job's counters -- a failure must still
			// be persisted (files_failed), not dropped with the callback.
			// bump only when handleWatchItem reports the item was handled: a
			// nudge means "something changed" (a failed event or a removal
			// with nothing to mark is not a change).
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
	if n := work.droppedCount(); n > 0 {
		w.log.Warn("pipeline: watch queue dropped events under sustained backpressure this run", "location", loc.RootPath, "dropped", n)
	}

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

// consumeOne dispatches one dequeued item: if ctx is already done, it's
// abandoned rather than run through handleWatchItem, returning
// abandoned=true; otherwise handleWatchItem runs normally and its bumped
// result is returned as-is.
//
// The abandon path exists because dequeue can still return up to
// watchQueueCapacity backlogged items after indexer.Watch has already
// returned due to ctx.Done() -- items that were queued before shutdown
// began but not yet processed. Running the full handleWatchItem pipeline
// (real file I/O, hashing that isn't ctx-aware, exiftool/ffprobe
// subprocesses, a DB write) against an already-cancelled context for
// potentially hundreds of items would delay finalization for no benefit.
// Mirrors workers.Pool's OnAbandon (#92) for the same reason: the next
// full scan is this package's existing self-healing catch-up path for
// anything a watch event misses.
func (w *WatcherSupervisor) consumeOne(ctx context.Context, loc storage.Location, item watchItem, seen, hashed, failed *atomic.Int32) (bumped, abandoned bool) {
	if ctx.Err() != nil {
		// Only seen, not failed: this item was never attempted, so counting
		// it as a failure would make files_failed>0 on every ordinary
		// shutdown that happens to catch a backlog, indistinguishable from
		// a real processing failure to an operator or an alert watching
		// that column. The job's own CANCELLED state already says "this
		// didn't run to completion" -- files_failed should stay meaningful
		// as "things that were actually attempted and broke."
		seen.Add(1)
		return false, true
	}
	return w.handleWatchItem(ctx, loc, item, seen, hashed, failed), false
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
			// commit.go's reconcileAllMetadata doc comment, #86, and #105.
			// No *Stats in scope here (inside an InTx closure returning bare
			// error) -- reconcileAllMetadata logs the written count instead.
			return reconcileAllMetadata(ctx, q, n.ID, *result, nil, w.log)
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
