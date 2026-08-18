package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/indexer"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

// batchSize and batchInterval match the build plan's batching policy:
// commit every 64 results or 250ms, whichever comes first, so the single
// writer connection isn't hammered per file.
const (
	batchSize     = 64
	batchInterval = 250 * time.Millisecond
	probeTimeout  = 30 * time.Second
)

// ScanDeps bundles what a scan needs. Pool is expected to already be
// running (Pool.Run called once at server startup, shared across scans) --
// RunScan only submits to it, never starts or stops it. Engine is optional:
// nil skips edge resolution entirely, which existing tests use to isolate
// node-commit behavior from internal/graph.
type ScanDeps struct {
	DB                    *db.DB
	Guard                 *storage.Guard
	Prober                *probe.Prober
	Pool                  *workers.Pool[string]
	Engine                *graph.Engine
	FullHashPolicy        string // "always" | "tier3_and_collision" (default) | "never"
	DisablePerceptualHash bool   // when true, disables pHash extraction for images
	Log                   *slog.Logger

	// WalkFn is the directory-walk function RunScan uses, defaulting to
	// indexer.Walk. Overridable in tests to force a mid-walk error -- the
	// data-loss failure mode the MISSING sweep must never fire on.
	WalkFn func(ctx context.Context, root string, onFile func(indexer.Record) error) error

	// Tracker, if set, is joined via Tracker.Wait() by cmd/branchdam before
	// the deferred db.Close() -- so an in-flight scan's final writes
	// (MISSING sweep, CompleteScanJob) always land on a still-open database,
	// the same guarantee WatcherSupervisor.Wait already gives watch
	// consumers. Optional: nil means untracked, which is fine for tests that
	// don't exercise shutdown ordering.
	Tracker *ScanTracker

	// Nudge, if set, is called by drainAndCommit after any flush that
	// changed something -- a committed batch, or files_seen advancing since
	// the last report -- so the SSE hub can prompt the SPA to re-fetch
	// instead of waiting on the 15s poll safety net. Mirrors
	// WatcherSupervisor's nudge parameter. Optional: nil is a no-op.
	Nudge func()

	// Shutdown, if set, is closed when the server begins an orderly
	// shutdown -- cmd/branchdam wires this to its signal context's Done()
	// channel, the same context passed to Pool.Run. It is deliberately NOT
	// the context runScan does its work under: RunScan wraps the caller's
	// ctx in context.WithoutCancel precisely so a request cancellation (or
	// this scan's own ctx) can't kill a scan, and reusing that ctx here
	// would silently reintroduce the thing WithoutCancel exists to prevent.
	// A plain <-chan struct{} rather than a context makes that mixing
	// structurally impossible -- there's no cancellation to accidentally
	// pass through. Used only to distinguish a shutdown-interrupted scan
	// (terminalized CANCELLED) from ordinary backpressure (terminalized
	// COMPLETED with a nonzero files_failed) -- see runScan's #99 note.
	// Optional: nil means never shutting down, matching every existing test.
	Shutdown <-chan struct{}
}

// ScanTracker joins every in-flight RunScan goroutine, mirroring
// WatcherSupervisor's wg/Wait pattern for the same reason: a background job
// that outlives the request which started it also needs an explicit join
// point during server shutdown, since nothing else waits for it.
type ScanTracker struct {
	wg sync.WaitGroup
}

func (t *ScanTracker) add()  { t.wg.Add(1) }
func (t *ScanTracker) done() { t.wg.Done() }

// Wait blocks until every RunScan goroutine started against this tracker
// has finished -- including its final DB writes. Call after the pool's
// context has been cancelled (so in-flight jobs resolve one way or another,
// see Pool.Submit/OnAbandon) and before closing the database.
func (t *ScanTracker) Wait() { t.wg.Wait() }

// RunScan creates a scan_jobs row and returns its id immediately; the walk
// and all hashing/committing happen in a background goroutine. Callers
// (internal/httpapi, PR 9) are expected to respond 202 with the returned
// job id right away -- this function never blocks on the scan itself.
func RunScan(ctx context.Context, deps ScanDeps, location storage.Location) (int64, error) {
	var job sqlcgen.ScanJob
	err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
		j, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: location.ID, Valid: true},
			Kind:              "FULL_SCAN",
		})
		job = j
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("create scan job: %w", err)
	}

	// The scan is a background job, not a request: it must outlive the HTTP
	// request context that started it, or the first canceled ctx (the walk's
	// per-entry check, any InTx) kills it mid-flight and the job never
	// completes. context.WithoutCancel keeps the values but drops the
	// cancellation -- shutdown is the pool's concern, not the request's
	// (Pool.Submit/OnAbandon resolve every in-flight job at shutdown; see
	// runScan). Tracker.add/done, if a Tracker is set, is what lets
	// cmd/branchdam actually wait for that resolution to finish before
	// closing the database -- add() happens synchronously here, before the
	// goroutine starts, so a Wait() call racing immediately after RunScan
	// returns can never miss it.
	if deps.Tracker != nil {
		deps.Tracker.add()
	}
	go func() {
		if deps.Tracker != nil {
			defer deps.Tracker.done()
		}
		runScan(context.WithoutCancel(ctx), deps, location, job.ID, job.StartedAt)
	}()
	return job.ID, nil
}

// uncertainPaths is the pass's seen-but-uncertain set: every path the walk
// saw but that was not reliably committed (processFile error, submit refused,
// dropped result, batch Commit failure). It is concurrency-safe because the
// walk callback and the pool goroutines write to it while drainAndCommit and
// finalize read it. The MISSING sweep excludes these paths -- a file on disk
// with a stale last_seen_at is not proof it's gone, and the sweep is the only
// automated force that flips a node to MISSING.
type uncertainPaths struct {
	mu    sync.Mutex
	paths map[string]struct{}
}

func newUncertainPaths() *uncertainPaths {
	return &uncertainPaths{paths: make(map[string]struct{})}
}

func (u *uncertainPaths) add(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.paths[path] = struct{}{}
}

func (u *uncertainPaths) list() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.paths))
	for p := range u.paths {
		out = append(out, p)
	}
	return out
}

func runScan(ctx context.Context, deps ScanDeps, location storage.Location, jobID, startedAt int64) {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	results := make(chan Result, batchSize*2)
	var wg sync.WaitGroup
	var filesSeen, filesFailed atomic.Int32
	var abandonedCount atomic.Int32
	// interrupted is set (only) from OnAbandon, which fires exclusively from
	// Pool.closeOnDone after the pool's own ctx.Done() -- shutdown is its
	// only possible cause (see #99's terminalization predicate below). It is
	// NOT set from the "submit refused" branch: Pool.Submit's bare bool
	// conflates three distinct causes (duplicate key in flight, queue full,
	// pool closed) with no way to tell which one fired -- not a timing race
	// (deps.Shutdown and the pool's Run ctx observe the identical channel in
	// production, and a channel close is a single globally-visible event, so
	// there is nothing to race), but causal ambiguity: attributing every
	// refusal to shutdown would misclassify ordinary backpressure that has
	// nothing to do with it. Terminalization instead checks deps.Shutdown
	// itself, once, after the walk and drain are both joined -- a strictly
	// simpler and equally sufficient signal, since interrupted's own two
	// setters already cover every case where a specific file's outcome is
	// unambiguous.
	var interrupted atomic.Bool
	uncertain := newUncertainPaths()

	walkFn := deps.WalkFn
	if walkFn == nil {
		walkFn = indexer.Walk
	}

	// The walk runs on its own goroutine so drainAndCommit (below) can start
	// consuming results while the walk is still in progress, instead of
	// after it finishes. Without this, nobody reads `results` for the
	// entire walk: once the buffer (batchSize*2) fills, every pool worker
	// blocks on `results <- *result`, the pool's own queue then fills
	// behind them, and Submit starts refusing -- silently capping a scan at
	// roughly the first `cap(results) + queue depth + worker count` files
	// and reporting the rest as failed rather than failing the job.
	//
	// walkDone is joined explicitly (rather than relying on close(results)
	// having already happened-before drainAndCommit's return) so that
	// reading walkErr below is unambiguously safe -- today drainAndCommit
	// has only one return path, the `!ok` branch after close(results), so
	// this join is currently redundant with that happens-before edge, but
	// it documents the actual requirement instead of an implicit one.
	//
	// This join is safe even during server shutdown (#92): a pool worker
	// that's still blocked on `results <- *result` when the pool's Run(ctx)
	// is cancelled unblocks via its select's `<-jobCtx.Done()` case (jobCtx
	// is that same pool ctx), and a job still sitting in the pool's queue,
	// never dequeued at all, has its OnAbandon hook (set on the Submit call
	// below) release the same wg token Run() would have. Pool.Submit also
	// refuses any further submissions once the pool's own ctx is done
	// (independent of this scan's own ctx, which deliberately survives
	// shutdown -- see RunScan's context.WithoutCancel), so the walk
	// goroutine's wg.Wait() is guaranteed to see every token it handed out
	// eventually released, one way or another, and always terminates.
	var walkErr error
	walkDone := make(chan struct{})
	go func() {
		defer close(walkDone)
		walkErr = walkFn(ctx, location.RootPath, func(rec indexer.Record) error {
			if rec.IsSymlink {
				return nil // following a symlink is a storage.Guard-mediated decision elsewhere, not this pass's
			}
			filesSeen.Add(1)

			wg.Add(1)
			submitted := deps.Pool.Submit(ctx, workers.Job[string]{
				Key: rec.Path,
				Run: func(jobCtx context.Context) error {
					defer wg.Done()
					result, err := processFile(jobCtx, deps, location, rec)
					if err != nil {
						uncertain.add(rec.Path)
						filesFailed.Add(1)
						log.Warn("pipeline: index file failed", "path", rec.Path, "err", err)
						return err
					}
					select {
					case results <- *result:
					case <-jobCtx.Done():
						// Unlike the submit-refused branch below, this is
						// unambiguous: jobCtx is the pool's own Run context,
						// so its Done() firing can only mean the pool is
						// shutting down -- safe to record directly, no race
						// with deps.Shutdown to worry about.
						uncertain.add(rec.Path)
						filesFailed.Add(1)
						interrupted.Store(true)
					}
					return nil
				},
				// OnAbandon covers the case Run never even starts: the job
				// was accepted into the pool's queue but the pool shut down
				// before a worker dequeued it. Same bookkeeping as a refused
				// submit, so the walk goroutine's wg.Wait() below still sees
				// this token released. Abandons are counted, not logged
				// individually here -- a shutdown with a full queue
				// (queueDepth defaults to 1024) would otherwise emit one Warn
				// line per queued file; a single summary line is logged once
				// after wg.Wait() below instead.
				OnAbandon: func() {
					wg.Done()
					uncertain.add(rec.Path)
					filesFailed.Add(1)
					abandonedCount.Add(1)
					interrupted.Store(true)
				},
			})
			if !submitted {
				wg.Done()
				uncertain.add(rec.Path)
				filesFailed.Add(1)
				// This can be ordinary backpressure (duplicate in flight,
				// queue full) OR the pool refusing because it's shutting
				// down -- Pool.Submit's bare bool return doesn't distinguish
				// them (see interrupted's doc comment above for why that
				// ambiguity, not a timing race, is what keeps this branch
				// from setting interrupted). Not logged as a shutdown event;
				// terminalization decides that once, after the fact.
				log.Warn("pipeline: submit refused (duplicate in flight, queue full, or pool shutting down)", "path", rec.Path)
			}
			return nil
		})
		wg.Wait()
		if n := abandonedCount.Load(); n > 0 {
			log.Warn("pipeline: jobs abandoned (pool shut down before they ran)", "count", n)
		}
		close(results)
	}()

	total := drainAndCommit(ctx, deps, location.ID, jobID, results, &filesSeen, &filesFailed, uncertain, log)
	<-walkDone

	finalErr := walkErr
	if finalErr != nil {
		log.Error("pipeline: walk failed", "location", location.RootPath, "err", finalErr)
		// A walk that failed partway means the sweep cannot know which nodes
		// were genuinely unseen vs. just not reached -- sweep never runs. The
		// job is failed in its own transaction, so it reaches a terminal state
		// barring a failure of the terminalizing write itself -- which only
		// happens when the DB is failing, and there is nothing else the scan
		// can do about it.
		if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.FailScanJob(ctx, sqlcgen.FailScanJobParams{ID: jobID, LastError: sql.NullString{String: finalErr.Error(), Valid: true}})
		}); err != nil {
			log.Error("pipeline: fail scan job", "jobID", jobID, "err", err)
		}
		return
	}

	// The sweep runs on every successful walk, excluding this pass's
	// seen-but-uncertain paths. A file the walk saw but failed to commit never
	// had last_seen_at bumped -- sweeping it would flip a live file to MISSING
	// and can feed a spurious RebaseMissingNodePath steal -- so exclusion, not
	// a whole-location skip, is the fix: everything else this scan genuinely
	// did not see is still swept.
	keepActive := uncertain.list()
	if len(keepActive) == 0 {
		// sqlc substitutes NULL for an empty slice, and file_path NOT IN
		// (NULL) is unknown -- false -- in SQL's three-valued logic: an empty
		// list would silently disable the sweep on every clean pass. A real
		// file_path is always a non-empty absolute path, so this sentinel can
		// never match one and NOT IN stays true for every row.
		keepActive = []string{""}
	} else {
		log.Info("pipeline: excluding seen-but-uncertain paths from MISSING sweep", "jobID", jobID, "excluded", len(keepActive))
	}

	// A sweep failure is logged, never fatal -- the scan itself succeeded, and
	// the node is simply swept one pass later (delayed-not-wrong).
	var swept int64
	if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
		jsonKeepActive, _ := json.Marshal(keepActive)
		var err error
		swept, err = q.MarkUnseenNodesMissing(ctx, sqlcgen.MarkUnseenNodesMissingParams{
			StorageLocationID: location.ID,
			LastSeenAt:        startedAt,
			JsonEach:          string(jsonKeepActive),
		})
		return err
	}); err != nil {
		log.Error("pipeline: MISSING sweep failed (delayed-not-wrong, swept next pass)", "jobID", jobID, "err", err)
	} else if swept > 0 {
		log.Info("pipeline: marked unseen nodes MISSING", "jobID", jobID, "count", swept)
	}

	// #99: a scan that lost work to a mid-shutdown pool close is CANCELLED,
	// not COMPLETED -- mirroring watchLocation's own CANCELLED-by-default
	// convention (watcher.go) for a clean-shutdown termination, as opposed
	// to FAILED for a genuine error (the walkErr branch above, which already
	// returned before this point). Evaluated ONCE here, after both the walk
	// goroutine and drainAndCommit have been joined (walkDone above, and
	// drainAndCommit's own return already happened-before total was
	// assigned) -- not per-file during the walk; interrupted's own doc
	// comment explains why per-file attribution isn't trustworthy anyway.
	// isClosed(deps.Shutdown) is the fallback for jobs that were entirely
	// enumerated and refused via the ambiguous "submit refused" branch,
	// which never sets interrupted itself; filesFailed > 0 is what keeps a
	// scan that finished all its work just as SIGTERM arrived recording as
	// COMPLETED rather than CANCELLED (isClosed alone can't tell "shutdown
	// happened" from "shutdown happened after we were already done").
	//
	// Accepted imprecision: a genuine per-file error (e.g. a file vanishing
	// mid-scan, processFile failing) that merely coincides with an unrelated
	// shutdown signal also satisfies this predicate and reads CANCELLED, not
	// something that preserves the real-failure signal the way FAILED's
	// last_error would. Both CANCELLED and COMPLETED-with-failures already
	// mean "not a fully clean pass" -- the files_failed count itself, not
	// the state label, is where the detail lives; this predicate's job is
	// only to distinguish "shutdown was involved" from "it wasn't."
	//
	// The three-state contract this produces, now that #88 is also merged:
	//   CANCELLED  -- clean shutdown; the scan terminalized itself here.
	//   FAILED     -- a genuine walk error (above), OR the process died
	//                 before terminalizing at all and reconcileOrphanedScanJobs
	//                 (cmd/branchdam, #88) flipped the orphaned RUNNING row
	//                 at the next boot.
	//   COMPLETED  -- ran to completion, whether or not shutdown began after.
	interruptedByShutdown := (interrupted.Load() || isClosed(deps.Shutdown)) && filesFailed.Load() > 0

	// CompleteScanJob/CancelScanJob runs in its own transaction, after the
	// sweep: the two are deliberately not atomic. A crash between them
	// leaves the job RUNNING with already-swept nodes -- status-only rows,
	// the swept rows are already correctly MISSING, and the next scan
	// re-sweeps harmlessly. Gating the sweep and the terminal write behind
	// one transaction would instead widen the "RUNNING forever" failure
	// window on a mid-write crash.
	if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
		if interruptedByShutdown {
			return q.CancelScanJob(ctx, jobID)
		}
		return q.CompleteScanJob(ctx, jobID)
	}); err != nil {
		log.Error("pipeline: terminalize scan job", "jobID", jobID, "cancelled", interruptedByShutdown, "err", err)
	}
	log.Info("pipeline: scan finished", "jobID", jobID, "seen", filesSeen.Load(), "failed", filesFailed.Load(),
		"cancelled", interruptedByShutdown, "inserted", total.Inserted, "touched", total.Touched,
		"versionCollisions", total.VersionCollisions, "moved", total.Moved, "edgesCreated", total.EdgesCreated,
		"metadataWritten", total.MetadataWritten)
}

// isClosed reports whether ch has been closed, without blocking. A nil
// channel (ScanDeps.Shutdown unset) reads as never closed -- select on a nil
// channel case simply never fires, so this is nil-safe by construction.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// drainAndCommit reads results as they arrive and commits every batchSize
// items or batchInterval, whichever comes first, updating scan_jobs
// progress -- and nudging the SSE hub, if deps.Nudge is set -- after each
// flush that changed anything. A ticker firing over an empty buf still
// reports files_seen alone rather than doing nothing: with hashWorkers
// capped and a single video's probes able to run for tens of seconds
// (processFile's doc comment), a scan can go a long stretch between
// committed batches even while the walk itself keeps advancing, and the old
// "empty buf means nothing to do" early return left files_seen -- already
// incrementing in memory -- unreported for that entire stretch.
func drainAndCommit(ctx context.Context, deps ScanDeps, locationID, jobID int64, results <-chan Result, filesSeen, filesFailed *atomic.Int32, uncertain *uncertainPaths, log *slog.Logger) Stats {
	var total Stats
	buf := make([]Result, 0, batchSize)
	var lastReportedSeen int32 = -1 // forces the very first flush to always report

	flush := func() {
		committedBatch := len(buf) > 0
		if committedBatch {
			stats, err := Commit(ctx, deps.DB, locationID, buf, log)
			total.Inserted += stats.Inserted
			total.Touched += stats.Touched
			total.VersionCollisions += stats.VersionCollisions
			total.Moved += stats.Moved
			total.MetadataWritten += stats.MetadataWritten
			if err != nil {
				// The whole batch failed to land -- every path in it stays
				// seen-but-uncertain and must be excluded from the MISSING
				// sweep, or a live file with a stale last_seen_at gets flipped
				// to MISSING and can feed a spurious RebaseMissingNodePath steal.
				log.Error("pipeline: commit batch", "err", err)
				filesFailed.Add(int32(len(buf)))
				for _, r := range buf {
					uncertain.add(r.Path)
				}
			} else if deps.Engine != nil {
				total.EdgesCreated += resolveEdgesForBatch(ctx, deps, buf, log)
			}
			buf = buf[:0]
		}

		seen := filesSeen.Load()
		if !committedBatch && seen == lastReportedSeen {
			return // nothing changed since the last report: no write, no nudge
		}

		err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.UpdateScanJobProgress(ctx, sqlcgen.UpdateScanJobProgressParams{
				ID:           jobID,
				FilesSeen:    int64(seen),
				FilesHashed:  int64(total.Inserted + total.Touched + total.VersionCollisions + total.Moved),
				FilesFailed:  int64(filesFailed.Load()),
				EdgesCreated: int64(total.EdgesCreated),
			})
		})
		if err != nil {
			log.Error("pipeline: update scan job progress", "err", err)
			return // the write didn't land -- retry on the next tick, so don't advance lastReportedSeen
		}
		lastReportedSeen = seen
		if deps.Nudge != nil {
			deps.Nudge()
		}
	}

	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case r, ok := <-results:
			if !ok {
				flush()
				return total
			}
			buf = append(buf, r)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// resolveEdgesForBatch runs edge resolution for every node just committed
// in buf, returning how many of the resulting edges were newly created --
// as opposed to an existing edge whose confidence/evidence was merely
// refreshed -- which is what scan_jobs.edges_created reports. This is the
// join point between node ingestion (this package) and lineage resolution
// (internal/graph) -- Commit only ever writes media_nodes rows; nothing
// else in the scan flow would otherwise ever call graph.Engine. One extra
// read per node (re-fetching the live row by path) keeps this decoupled
// from Commit's internals rather than threading node IDs back out of it.
func resolveEdgesForBatch(ctx context.Context, deps ScanDeps, buf []Result, log *slog.Logger) int {
	var created int
	for _, r := range buf {
		node, err := deps.DB.Reader.GetLiveNodeByPath(ctx, r.Path)
		if err != nil {
			log.Warn("pipeline: resolve edges: re-fetch node", "path", r.Path, "err", err)
			continue
		}
		_, n, err := deps.Engine.ResolveAndCommit(ctx, toGraphNode(node))
		if err != nil {
			log.Warn("pipeline: resolve edges", "path", r.Path, "err", err)
			continue
		}
		created += n
	}
	return created
}

func toGraphNode(n sqlcgen.MediaNode) graph.Node {
	return graph.ToNode(n)
}

// needsFullHash decides whether Result.FullHash should be computed for this
// file, per docs/schema.md fix #8's policy. tierReadOnly is true when the
// file lives on a TIER3_MASTER_ARCHIVE location (the bit-for-bit
// verification promise); hasCollision is true when another LIVE node
// already shares this file's fast_hash at a different path (T1, spec 9.5 --
// duplicate detection must never be decided by 64 bits alone). An
// unrecognized policy string falls back to the same behavior as the
// default "tier3_and_collision", not to "never" -- silently skipping
// integrity verification because of a config typo would be the wrong
// failure mode.
func needsFullHash(policy string, tierReadOnly, hasCollision bool) bool {
	switch policy {
	case "always":
		return true
	case "never":
		return false
	default:
		return tierReadOnly || hasCollision
	}
}

// processFile does all the slow, off-transaction work for one file: opens
// it through storage.Guard, hashes it, optionally escalates to full_hash,
// and optionally runs exiftool. It also runs FFProbe for video extensions
// under the same probeTimeout budget; a video-heavy scan can pause progress
// for long stretches between batch commits. Runs entirely on a workers.Pool
// goroutine, never inside a database transaction.
func processFile(ctx context.Context, deps ScanDeps, location storage.Location, rec indexer.Record) (*Result, error) {
	f, err := deps.Guard.OpenRead(rec.Path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	fastHash, err := hashing.FastHash(f, rec.Size)
	if err != nil {
		return nil, fmt.Errorf("fast hash: %w", err)
	}

	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(rec.Path)), ".")
	result := &Result{
		Path:     rec.Path,
		FileName: filepath.Base(rec.Path),
		FileExt:  ext,
		Size:     rec.Size,
		ModTime:  rec.ModTime,
		FastHash: fastHash,
	}

	hasCollision := false
	if deps.DB != nil {
		if nodes, err := deps.DB.Reader.ListLiveNodesByFastHash(ctx, &fastHash); err == nil {
			for _, n := range nodes {
				if n.FilePath != rec.Path {
					hasCollision = true
					break
				}
			}
		}
	}

	if needsFullHash(deps.FullHashPolicy, location.ReadOnly, hasCollision) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek for full hash: %w", err)
		}
		if fh, err := hashing.FullHash(f); err != nil {
			deps.logOrDiscard().Warn("pipeline: full hash failed", "path", rec.Path, "err", err)
		} else {
			result.FullHash = fh
		}
	}

	if deps.Prober != nil && deps.Prober.HasExiftool() {
		// spec directive 9.4: fall back gracefully to fast_hash indexing if
		// metadata parsing fails -- an Exif error here is logged, never
		// returned, so one unreadable tag never fails the whole file.
		exifCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		exif, err := deps.Prober.Exif(exifCtx, rec.Path)
		cancel()
		if err != nil {
			deps.logOrDiscard().Debug("pipeline: exif probe failed, indexing without it", "path", rec.Path, "err", err)
		} else {
			result.OriginalDocumentID = exif.OriginalDocumentID
			result.DocumentID = exif.DocumentID
			result.DerivedFromID = exif.DerivedFromID
			result.CapturedAt = exif.CapturedAt
			result.CameraModel = exif.Model
			result.Make = exif.Make
			result.LensModel = exif.LensModel
			result.SerialNumber = exif.SerialNumber
			result.GPSLatitude = exif.GPSLatitude
			result.GPSLongitude = exif.GPSLongitude
			result.ExifRaw = selectExifRaw(exif.Raw)
		}
	}

	if deps.Prober != nil && isImageExt(ext) && !deps.DisablePerceptualHash {
		phCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		ph, err := deps.Prober.ExtractPHash(phCtx, rec.Path)
		cancel()
		if err != nil {
			deps.logOrDiscard().Debug("pipeline: phash extraction failed", "path", rec.Path, "err", err)
		} else if ph != nil {
			result.PHash = ph
		}
	}

	if deps.Prober != nil && deps.Prober.HasFFProbe() && isVideoExt(ext) {
		// spec directive 9.4: ffprobe failure is logged and the file still
		// indexes -- same graceful-degradation rule as the Exif call.
		ffCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		ff, err := deps.Prober.FFProbe(ffCtx, rec.Path)
		cancel()
		if err != nil {
			deps.logOrDiscard().Debug("pipeline: ffprobe probe failed, indexing without it", "path", rec.Path, "err", err)
		} else {
			result.FFProbe = ff
		}
	}

	return result, nil
}

func (d ScanDeps) logOrDiscard() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.DiscardHandler)
}
