package pipeline

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/indexer"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// TestSweeperSupervisorNoOverlap proves a sweep pass slower than its
// configured interval never runs concurrently with the next pass:
// sweepLocation's own select loop only re-enters select once runOneSweep
// has returned, so overlap is structurally impossible rather than merely
// unlikely. Forces a pass slower than the interval via WalkFn and tracks
// concurrent WalkFn invocations directly, rather than relying on
// scan_jobs timestamps -- unixepoch()'s 1s granularity can't distinguish
// sub-second overlap.
func TestSweeperSupervisorNoOverlap(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "alpha content")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)

	var inFlight, maxInFlight, passes atomic.Int32
	deps.WalkFn = func(ctx context.Context, root string, onFile func(indexer.Record) error) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			cur := maxInFlight.Load()
			if n <= cur || maxInFlight.CompareAndSwap(cur, n) {
				break
			}
		}
		passes.Add(1)
		time.Sleep(80 * time.Millisecond) // deliberately longer than the 20ms interval below
		return nil
	}

	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}
	sweeper := NewSweeperSupervisor(deps)
	ctx, cancel := context.WithCancel(context.Background())
	sweeper.Start(ctx, []SweptLocation{{Location: loc, Interval: 20 * time.Millisecond}})

	time.Sleep(300 * time.Millisecond)
	cancel()
	sweeper.Wait()

	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("max concurrent sweep passes for one location = %d, want at most 1", got)
	}
	if got := passes.Load(); got < 2 {
		t.Errorf("passes run = %d, want at least 2 (300ms runtime vs a 20ms interval + 80ms pass duration)", got)
	}
}

// TestSweeperSupervisorStartTwiceWarnsAndNoops mirrors
// WatcherSupervisor's own second-Start-is-a-noop contract.
func TestSweeperSupervisorStartTwiceWarnsAndNoops(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	locationID := seedPipelineLocation(t, database, resolvedRoot)
	deps := scanTestDeps(t, database, resolvedRoot, locationID)
	loc := storage.Location{ID: locationID, Name: "test-export", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false}

	sweeper := NewSweeperSupervisor(deps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweeper.Start(ctx, []SweptLocation{{Location: loc, Interval: time.Hour}})
	sweeper.Start(ctx, []SweptLocation{{Location: loc, Interval: time.Hour}}) // no-op, must not panic or double-run

	cancel()
	sweeper.Wait()
}
