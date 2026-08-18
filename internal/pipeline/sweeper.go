package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// defaultSweepInterval is #60's own default -- the spec's "polling mtime
// every 10 minutes" directive for SMB/NFS shares (docs/spec/original-spec.md).
const defaultSweepInterval = 10 * time.Minute

// SweptLocation pairs a storage.Location with its sweep interval.
// SweeperSupervisor.Start takes one per location -- unlike
// WatcherSupervisor's single shared debounce, #60's interval is configured
// per location (config.StorageLocation.SweepIntervalSecs). Interval <= 0
// means "use defaultSweepInterval".
type SweptLocation struct {
	Location storage.Location
	Interval time.Duration
}

// SweeperSupervisor runs one differential mtime sweep per opted-in storage
// location on a fixed interval, on the server's signal context -- the
// low-priority polling adjunct to fsnotify (WatcherSupervisor) for SMB/NFS
// shares where fsnotify does not fire reliably. Each pass gets a
// kind='INCREMENTAL' scan_jobs row and reuses runScan's own
// clean-completion-gated MISSING sweep and #89's node_metadata pruning
// (see runScan's differential parameter). Tier 3 locations are never swept
// -- main.go filters before Start, same as WatcherSupervisor does for
// watched locations.
//
// Each location's sweep runs on its own dedicated goroutine, one pass at a
// time: the ticker loop only re-enters its select once runOneSweep has
// returned, so a pass slower than its interval cannot overlap itself --
// Go's own time.Ticker drops any ticks that arrive while its single
// buffered slot is unread, so the next receive after a slow pass fires
// immediately rather than queuing a backlog.
type SweeperSupervisor struct {
	deps      ScanDeps
	log       *slog.Logger
	wg        sync.WaitGroup
	startOnce sync.Once
}

func NewSweeperSupervisor(deps ScanDeps) *SweeperSupervisor {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SweeperSupervisor{deps: deps, log: log}
}

// Start begins one sweep goroutine per location and returns immediately.
// Goroutines run until ctx is cancelled. A second call is a no-op and logs
// a warning, mirroring WatcherSupervisor.Start.
func (s *SweeperSupervisor) Start(ctx context.Context, locations []SweptLocation) {
	started := false
	s.startOnce.Do(func() {
		started = true
		for _, sl := range locations {
			interval := sl.Interval
			if interval <= 0 {
				interval = defaultSweepInterval
			}
			s.wg.Add(1)
			go func(loc storage.Location, iv time.Duration) {
				defer s.wg.Done()
				s.sweepLocation(ctx, loc, iv)
			}(sl.Location, interval)
		}
	})
	if !started {
		s.log.Warn("pipeline: SweeperSupervisor.Start called after the first start; ignoring", "locations", len(locations))
	}
}

// Wait blocks until every sweep goroutine has returned -- called during
// shutdown, after the sweeper's ctx is cancelled and before pool.Drain and
// db.Close (see main.go), mirroring WatcherSupervisor.Wait.
func (s *SweeperSupervisor) Wait() { s.wg.Wait() }

func (s *SweeperSupervisor) sweepLocation(ctx context.Context, loc storage.Location, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOneSweep(ctx, loc)
		}
	}
}

// runOneSweep creates the pass's scan_jobs row and runs it to completion
// before returning -- deliberately synchronous, not RunSweep's
// fire-and-forget async wrapper, since sweepLocation's own select loop
// (only re-entering select once this call returns) is what guarantees
// passes never overlap.
//
// context.WithoutCancel mirrors startScanAsync's own reasoning: the scan
// work must survive to its own terminal state even once shutdown begins.
// deps.Shutdown -- wired by the caller (main.go) to the same ctx.Done()
// this loop itself observes -- is the separate signal runScan's own
// CANCELLED-vs-FAILED terminalization logic uses to tell a deliberate
// shutdown apart from a genuine walk failure; without WithoutCancel here,
// a shutdown mid-walk would surface as a walk error (FAILED) instead of the
// CANCELLED state watchLocation's own convention (watcher.go) uses for a
// clean shutdown.
func (s *SweeperSupervisor) runOneSweep(ctx context.Context, loc storage.Location) {
	job, err := createScanJob(ctx, s.deps, loc, "INCREMENTAL")
	if err != nil {
		s.log.Error("pipeline: create sweep job", "location", loc.RootPath, "err", err)
		return
	}
	runScan(context.WithoutCancel(ctx), s.deps, loc, job.ID, job.StartedAt, true)
}
