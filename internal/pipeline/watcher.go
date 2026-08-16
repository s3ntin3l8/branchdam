package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
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
// the server's signal context, feeding events through the same processFile /
// Commit / resolveEdgesForBatch machinery a full scan uses. Each watched
// location gets a long-lived scan_jobs row with kind='WATCH' that stays
// RUNNING until shutdown (-> CANCELLED) or the watcher dies (-> FAILED).
// Tier 3 locations are never watched -- main.go filters before Start.
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
// dies (-> FAILED).
func (w *WatcherSupervisor) Start(ctx context.Context, locations []storage.Location, debounce time.Duration) {
	if debounce <= 0 {
		debounce = watchDebounce
	}
	for _, loc := range locations {
		w.wg.Add(1)
		go func(l storage.Location) {
			defer w.wg.Done()
			w.watchLocation(ctx, l, debounce)
		}(loc)
	}
}

// Wait blocks until every watch goroutine has returned. Called during
// shutdown, after the fsnotify context is cancelled and before pool.Drain
// and db.Close (see main.go).
func (w *WatcherSupervisor) Wait() { w.wg.Wait() }

// watchLocation owns one location's watch lifecycle: creates the WATCH
// scan_jobs row, feeds events to the pipeline, and finalizes the job on exit.
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

	var seen, hashed, failed atomic.Int32
	watchErr := indexer.Watch(ctx, loc.RootPath, debounce, w.log,
		func(rec indexer.Record) error {
			if rec.IsSymlink {
				return nil
			}
			seen.Add(1)
			result, err := processFile(ctx, w.deps, loc, rec)
			if err != nil {
				failed.Add(1)
				return err
			}
			stats, err := Commit(ctx, w.deps.DB, loc.ID, []Result{*result})
			if err != nil {
				failed.Add(1)
				return err
			}
			hashed.Add(int32(stats.Inserted + stats.Touched + stats.VersionCollisions + stats.Moved))
			if w.deps.Engine != nil {
				resolveEdgesForBatch(ctx, w.deps, []Result{*result}, w.log)
			}
			w.updateJob(job.ID, &seen, &hashed, &failed)
			w.bump()
			return nil
		},
		func(path string) error {
			seen.Add(1)
			node, err := w.deps.DB.Reader.GetLiveNodeByPath(ctx, path)
			if errors.Is(err, sql.ErrNoRows) {
				w.bump() // never indexed, or already rebound
				return nil
			}
			if err != nil {
				return err
			}
			if node.LifecycleState == "MISSING" {
				w.bump()
				return nil
			}
			if err := w.deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
				return q.MarkNodeMissing(ctx, node.ID)
			}); err != nil {
				return err
			}
			w.updateJob(job.ID, &seen, &hashed, &failed)
			w.bump()
			return nil
		},
	)

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

func (w *WatcherSupervisor) updateJob(jobID int64, seen, hashed, failed *atomic.Int32) {
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
