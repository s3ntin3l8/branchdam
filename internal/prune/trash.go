package prune

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// DefaultTrashPurgeInterval is how often the trash worker runs by default (1 hour).
const DefaultTrashPurgeInterval = 1 * time.Hour

// TrashPruneResult reports the result of a trash pruning pass.
type TrashPruneResult struct {
	FilesPurged int
	BytesFreed  int64
	Errors      []error
}

// TrashTimeFunc reports when the .trash/ copy at path (inside the storage
// location rooted at locRoot) was trashed, as recorded in the database --
// pipeline.TrashTimeLookup is the production implementation (it logs each
// known=false at DEBUG with the path and parsed node_uuid). known=false
// means no TRASHED node owns the file, and the purge falls back to the
// file's mtime. A non-nil error skips the file for this pass: a DB hiccup
// must never turn into an early unlink.
type TrashTimeFunc func(ctx context.Context, locRoot, path string) (trashedAt time.Time, known bool, err error)

// PurgeTrash permanently removes files from the .trash/ directory under locRootPath
// that were trashed before cutoff (determined by retentionDays relative to now).
// If retentionDays <= 0, PurgeTrash is a no-op (automated purge disabled).
//
// A file's age is its owning node's recorded trash time (trashTimes) when
// one exists, else its mtime. The mtime alone was not safe: when
// pipeline.moveToTrash's Chtimes failed, a just-trashed file kept its
// original, typically old, mtime and was unlinked on the next pass. A nil
// trashTimes ages every file by mtime.
//
// Every unlink goes through guard (AGENTS.md invariant 3): a location marked
// read-only is never purged, even if its .trash/ is physically writable.
func PurgeTrash(ctx context.Context, guard *storage.Guard, locRootPath string, retentionDays int, now time.Time, trashTimes TrashTimeFunc) (TrashPruneResult, error) {
	if retentionDays <= 0 {
		return TrashPruneResult{}, nil
	}

	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	trashRoot := filepath.Join(locRootPath, ".trash")

	if _, err := os.Stat(trashRoot); os.IsNotExist(err) {
		return TrashPruneResult{}, nil
	}

	var res TrashPruneResult

	err := filepath.WalkDir(trashRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		trashedAt := info.ModTime()
		if trashTimes != nil {
			at, known, lookupErr := trashTimes(ctx, locRootPath, path)
			if lookupErr != nil {
				res.Errors = append(res.Errors, lookupErr)
				return nil
			}
			if known {
				trashedAt = at
			}
		}

		if trashedAt.Before(cutoff) {
			size := info.Size()
			if err := guard.Remove(path); err != nil {
				res.Errors = append(res.Errors, err)
			} else {
				res.FilesPurged++
				res.BytesFreed += size
			}
		}
		return nil
	})

	if err != nil {
		return res, err
	}

	// Clean up empty directories under .trash/
	cleanEmptyDirs(guard, trashRoot)

	return res, nil
}

func cleanEmptyDirs(guard *storage.Guard, dir string) {
	// Post-order walk from deepest directories up
	var dirs []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == dir || !d.IsDir() {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})

	// Iterate in reverse to remove innermost empty subdirectories first
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = guard.Remove(dirs[i]) // fails non-fatally if the directory is non-empty
	}
}

// PurgeAllTrash runs PurgeTrash across all storage locations in the guard.
func PurgeAllTrash(ctx context.Context, guard *storage.Guard, retentionDays int, now time.Time, trashTimes TrashTimeFunc) (TrashPruneResult, error) {
	if guard == nil || retentionDays <= 0 {
		return TrashPruneResult{}, nil
	}

	var totalRes TrashPruneResult
	for _, loc := range guard.Locations() {
		// Read-only and virtual locations are never written to; skip them
		// up front rather than counting a refusal per file.
		if loc.ReadOnly || loc.IsVirtual {
			continue
		}
		res, err := PurgeTrash(ctx, guard, loc.RootPath, retentionDays, now, trashTimes)
		totalRes.FilesPurged += res.FilesPurged
		totalRes.BytesFreed += res.BytesFreed
		totalRes.Errors = append(totalRes.Errors, res.Errors...)
		if err != nil && !errors.Is(err, context.Canceled) {
			return totalRes, err
		}
	}
	return totalRes, nil
}

// TrashWorker runs background trash purge on a schedule.
type TrashWorker struct {
	guard            *storage.Guard
	getRetentionDays func() int
	trashTimes       TrashTimeFunc
	log              *slog.Logger

	wg        sync.WaitGroup
	startOnce sync.Once
}

// TrashWorkerOption configures a TrashWorker.
type TrashWorkerOption func(*TrashWorker)

// WithTrashTimes makes the worker age .trash/ copies by their recorded
// trash time (see TrashTimeFunc) instead of by mtime alone.
func WithTrashTimes(fn TrashTimeFunc) TrashWorkerOption {
	return func(w *TrashWorker) { w.trashTimes = fn }
}

// NewTrashWorker creates a new TrashWorker.
func NewTrashWorker(guard *storage.Guard, getRetentionDays func() int, log *slog.Logger, opts ...TrashWorkerOption) *TrashWorker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if getRetentionDays == nil {
		getRetentionDays = func() int { return 30 }
	}
	w := &TrashWorker{
		guard:            guard,
		getRetentionDays: getRetentionDays,
		log:              log,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// PurgeOnce runs a single trash purge pass across all locations.
func (w *TrashWorker) PurgeOnce(ctx context.Context) (TrashPruneResult, error) {
	retentionDays := w.getRetentionDays()
	if retentionDays <= 0 {
		return TrashPruneResult{}, nil
	}
	return PurgeAllTrash(ctx, w.guard, retentionDays, time.Now().UTC(), w.trashTimes)
}

// Start launches the background ticker for periodic trash purge.
func (w *TrashWorker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultTrashPurgeInterval
	}
	w.startOnce.Do(func() {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			// Run initial pass on startup
			if res, err := w.PurgeOnce(ctx); err != nil {
				w.log.Warn("trash: purge pass failed", "err", err)
			} else {
				if len(res.Errors) > 0 {
					w.log.Warn("trash: errors encountered during purge", "errCount", len(res.Errors))
				}
				if res.FilesPurged > 0 {
					w.log.Info("trash: pruned expired files", "purged", res.FilesPurged, "freedBytes", res.BytesFreed)
				}
			}

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if res, err := w.PurgeOnce(ctx); err != nil {
						w.log.Warn("trash: purge pass failed", "err", err)
					} else {
						if len(res.Errors) > 0 {
							w.log.Warn("trash: errors encountered during purge", "errCount", len(res.Errors))
						}
						if res.FilesPurged > 0 {
							w.log.Info("trash: pruned expired files", "purged", res.FilesPurged, "freedBytes", res.BytesFreed)
						}
					}
				}
			}
		}()
	})
}
