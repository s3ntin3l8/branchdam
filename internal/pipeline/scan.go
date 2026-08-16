package pipeline

import (
	"context"
	"database/sql"
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
	exifTimeout   = 30 * time.Second
)

// ScanDeps bundles what a scan needs. Pool is expected to already be
// running (Pool.Run called once at server startup, shared across scans) --
// RunScan only submits to it, never starts or stops it. Engine is optional:
// nil skips edge resolution entirely, which existing tests use to isolate
// node-commit behavior from internal/graph.
type ScanDeps struct {
	DB             *db.DB
	Guard          *storage.Guard
	Prober         *probe.Prober
	Pool           *workers.Pool[string]
	Engine         *graph.Engine
	FullHashPolicy string // "always" | "tier3_and_collision" (default) | "never"
	Log            *slog.Logger

	// WalkFn is the directory-walk function RunScan uses, defaulting to
	// indexer.Walk. Overridable in tests to force a mid-walk error -- the
	// data-loss failure mode the MISSING sweep must never fire on.
	WalkFn func(ctx context.Context, root string, onFile func(indexer.Record) error) error
}

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

	go runScan(ctx, deps, location, job.ID, job.StartedAt)
	return job.ID, nil
}

func runScan(ctx context.Context, deps ScanDeps, location storage.Location, jobID, startedAt int64) {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	results := make(chan Result, batchSize*2)
	var wg sync.WaitGroup
	var filesSeen, filesFailed atomic.Int32

	walkFn := deps.WalkFn
	if walkFn == nil {
		walkFn = indexer.Walk
	}
	walkErr := walkFn(ctx, location.RootPath, func(rec indexer.Record) error {
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
					filesFailed.Add(1)
					log.Warn("pipeline: index file failed", "path", rec.Path, "err", err)
					return err
				}
				select {
				case results <- *result:
				case <-jobCtx.Done():
				}
				return nil
			},
		})
		if !submitted {
			wg.Done()
			filesFailed.Add(1)
			log.Warn("pipeline: submit refused (duplicate in flight or queue full)", "path", rec.Path)
		}
		return nil
	})
	if walkErr != nil {
		log.Error("pipeline: walk failed", "location", location.RootPath, "err", walkErr)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	total := drainAndCommit(ctx, deps, location.ID, jobID, results, &filesSeen, &filesFailed, log)

	finalErr := walkErr
	if finalErr != nil {
		// A walk that failed partway means the sweep cannot know which nodes
		// were genuinely unseen vs. just not reached -- sweep never runs. The
		// job is failed in its own transaction so it always terminalizes.
		if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.FailScanJob(ctx, sqlcgen.FailScanJobParams{ID: jobID, LastError: sql.NullString{String: finalErr.Error(), Valid: true}})
		}); err != nil {
			log.Error("pipeline: fail scan job", "jobID", jobID, "err", err)
		}
		return
	} else if failed := filesFailed.Load(); failed > 0 {
		// Files the walk SAW but failed to commit (processFile error, submit
		// refused) never had last_seen_at bumped -- sweeping them would flip
		// live files to MISSING and can feed a spurious RebaseMissingNodePath
		// steal. A pass with any failed files skips the sweep entirely.
		log.Info("pipeline: skipping MISSING sweep, files failed this pass", "jobID", jobID, "failed", failed)
	} else {
		// Clean pass: everything walked was committed, so anything still
		// older than the scan's start was genuinely unseen. A sweep failure
		// is logged, never fatal -- the scan itself succeeded, and the node
		// is simply swept one pass later (delayed-not-wrong).
		var swept int64
		if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
			var err error
			swept, err = q.MarkUnseenNodesMissing(ctx, sqlcgen.MarkUnseenNodesMissingParams{
				StorageLocationID: location.ID,
				BeforeUnix:        startedAt,
			})
			return err
		}); err != nil {
			log.Error("pipeline: MISSING sweep failed (delayed-not-wrong, swept next pass)", "jobID", jobID, "err", err)
		} else if swept > 0 {
			log.Info("pipeline: marked unseen nodes MISSING", "jobID", jobID, "count", swept)
		}
	}
	if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.CompleteScanJob(ctx, jobID)
	}); err != nil {
		log.Error("pipeline: complete scan job", "jobID", jobID, "err", err)
	}
	log.Info("pipeline: scan complete", "jobID", jobID, "seen", filesSeen.Load(), "failed", filesFailed.Load(),
		"inserted", total.Inserted, "touched", total.Touched, "versionCollisions", total.VersionCollisions, "moved", total.Moved)
}

// drainAndCommit reads results as they arrive and commits every batchSize
// items or batchInterval, whichever comes first, updating scan_jobs
// progress after each flush.
func drainAndCommit(ctx context.Context, deps ScanDeps, locationID, jobID int64, results <-chan Result, filesSeen, filesFailed *atomic.Int32, log *slog.Logger) Stats {
	var total Stats
	buf := make([]Result, 0, batchSize)

	flush := func() {
		if len(buf) == 0 {
			return
		}
		stats, err := Commit(ctx, deps.DB, locationID, buf)
		total.Inserted += stats.Inserted
		total.Touched += stats.Touched
		total.VersionCollisions += stats.VersionCollisions
		total.Moved += stats.Moved
		if err != nil {
			log.Error("pipeline: commit batch", "err", err)
		} else if deps.Engine != nil {
			resolveEdgesForBatch(ctx, deps, buf, log)
		}
		buf = buf[:0]
		if err := deps.DB.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.UpdateScanJobProgress(ctx, sqlcgen.UpdateScanJobProgressParams{
				ID:          jobID,
				FilesSeen:   int64(filesSeen.Load()),
				FilesHashed: int64(total.Inserted + total.Touched + total.VersionCollisions + total.Moved),
				FilesFailed: int64(filesFailed.Load()),
			})
		}); err != nil {
			log.Error("pipeline: update scan job progress", "err", err)
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
// in buf. This is the join point between node ingestion (this package) and
// lineage resolution (internal/graph) -- Commit only ever writes
// media_nodes rows; nothing else in the scan flow would otherwise ever call
// graph.Engine. One extra read per node (re-fetching the live row by path)
// keeps this decoupled from Commit's internals rather than threading node
// IDs back out of it.
func resolveEdgesForBatch(ctx context.Context, deps ScanDeps, buf []Result, log *slog.Logger) {
	for _, r := range buf {
		node, err := deps.DB.Reader.GetLiveNodeByPath(ctx, r.Path)
		if err != nil {
			log.Warn("pipeline: resolve edges: re-fetch node", "path", r.Path, "err", err)
			continue
		}
		if _, err := deps.Engine.ResolveAndCommit(ctx, toGraphNode(node)); err != nil {
			log.Warn("pipeline: resolve edges", "path", r.Path, "err", err)
		}
	}
}

func toGraphNode(n sqlcgen.MediaNode) graph.Node {
	gn := graph.Node{
		ID:       n.ID,
		FilePath: n.FilePath,
		FileName: n.FileName,
		FileExt:  n.FileExt,
	}
	if n.OriginalDocumentID.Valid {
		gn.OriginalDocumentID = n.OriginalDocumentID.String
	}
	if n.DocumentID.Valid {
		gn.DocumentID = n.DocumentID.String
	}
	if n.CameraModel.Valid {
		gn.CameraModel = n.CameraModel.String
	}
	if n.FilenameStem.Valid {
		gn.FilenameStem = n.FilenameStem.String
	}
	if n.CapturedAtUnix.Valid {
		t := time.Unix(n.CapturedAtUnix.Int64, 0).UTC()
		gn.CapturedAt = &t
	}
	return gn
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
// and optionally runs exiftool. Runs entirely on a workers.Pool goroutine,
// never inside a database transaction.
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
		exifCtx, cancel := context.WithTimeout(ctx, exifTimeout)
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
