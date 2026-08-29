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

// PurgeTrash permanently removes files from the .trash/ directory under locRootPath
// that were modified before cutoff (determined by retentionDays relative to now).
// If retentionDays <= 0, PurgeTrash is a no-op (automated purge disabled).
func PurgeTrash(ctx context.Context, locRootPath string, retentionDays int, now time.Time) (TrashPruneResult, error) {
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

		if info.ModTime().Before(cutoff) {
			size := info.Size()
			if err := os.Remove(path); err != nil {
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
	cleanEmptyDirs(trashRoot)

	return res, nil
}

func cleanEmptyDirs(dir string) {
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
		_ = os.Remove(dirs[i]) // os.Remove fails non-fatally if directory is non-empty
	}
}

// PurgeAllTrash runs PurgeTrash across all storage locations in the guard.
func PurgeAllTrash(ctx context.Context, guard *storage.Guard, retentionDays int, now time.Time) (TrashPruneResult, error) {
	if guard == nil || retentionDays <= 0 {
		return TrashPruneResult{}, nil
	}

	var totalRes TrashPruneResult
	for _, loc := range guard.Locations() {
		res, err := PurgeTrash(ctx, loc.RootPath, retentionDays, now)
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
	log              *slog.Logger

	wg        sync.WaitGroup
	startOnce sync.Once
}

// NewTrashWorker creates a new TrashWorker.
func NewTrashWorker(guard *storage.Guard, getRetentionDays func() int, log *slog.Logger) *TrashWorker {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if getRetentionDays == nil {
		getRetentionDays = func() int { return 30 }
	}
	return &TrashWorker{
		guard:            guard,
		getRetentionDays: getRetentionDays,
		log:              log,
	}
}

// PurgeOnce runs a single trash purge pass across all locations.
func (w *TrashWorker) PurgeOnce(ctx context.Context) (TrashPruneResult, error) {
	retentionDays := w.getRetentionDays()
	if retentionDays <= 0 {
		return TrashPruneResult{}, nil
	}
	return PurgeAllTrash(ctx, w.guard, retentionDays, time.Now().UTC())
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
