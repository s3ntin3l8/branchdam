package pipeline

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/indexer"
)

// sweepUnchanged reports whether rec matches a live, ACTIVE node's stored
// (mtime_unix, size_bytes) exactly -- the differential sweep's entire
// premise (#60): an unchanged file is TouchMediaNode'd directly by the
// caller, never opened, hashed, or routed through Commit. A Result with an
// empty FastHash routed through Commit would fail commitOne's fast_hash
// equality check and fall into commitVersionCollision, archiving the live
// node and inserting a hash-less successor -- so the unchanged path must
// never reach Commit at all.
//
// GetLiveNodeByPath excludes only ARCHIVED rows, so it can also return a
// MISSING node -- deliberately excluded here rather than trusted on a
// matching mtime/size alone. A full scan's ordinary touched branch only
// reactivates a MISSING node after actually computing and comparing its
// fast_hash; skipping that verification for a MISSING node purely because
// mtime+size happen to match would let a coincidentally-sized different
// file reattach a stale node's identity (full_hash, phash, thumbnail
// references) to itself. A MISSING node therefore always falls through to
// the ordinary hash+Commit path, which performs that verification -- only
// an already-ACTIVE node's differential comparison is trusted.
//
// A lookup error -- including sql.ErrNoRows for a brand new file --
// reports unchanged=false, routing the file through the ordinary
// hash+Commit path. Fail-safe: the worst case is an unnecessary re-hash,
// never a skipped change.
func sweepUnchanged(ctx context.Context, deps ScanDeps, rec indexer.Record) (sqlcgen.MediaNode, bool) {
	node, err := deps.DB.Reader.GetLiveNodeByPath(ctx, rec.Path)
	if err != nil {
		return sqlcgen.MediaNode{}, false
	}
	if node.LifecycleState == "ACTIVE" && node.MtimeUnix == rec.ModTime.Unix() && node.SizeBytes == rec.Size {
		return node, true
	}
	return sqlcgen.MediaNode{}, false
}

// touchBatcher batches TouchMediaNode calls made by a differential sweep's
// unchanged-file fast path into batchSize-sized transactions -- the writer
// pool is SetMaxOpenConns(1), so one InTx per unchanged file would
// serialize an SMB-scale sweep behind per-file round-trip latency, the same
// reasoning drainAndCommit's own batching already applies to hashed
// results.
//
// Only ever touched from the walk goroutine: indexer.Walk calls onFile
// serially, so add/flush need no locking of their own.
type touchBatcher struct {
	db          *db.DB
	uncertain   *uncertainPaths
	filesFailed *atomic.Int32
	log         *slog.Logger
	entries     []touchEntry
	total       int // running count of successfully flushed touches, for logging
}

type touchEntry struct {
	id        int64
	path      string
	mtimeUnix int64
}

func newTouchBatcher(database *db.DB, uncertain *uncertainPaths, filesFailed *atomic.Int32, log *slog.Logger) *touchBatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &touchBatcher{db: database, uncertain: uncertain, filesFailed: filesFailed, log: log, entries: make([]touchEntry, 0, batchSize)}
}

func (b *touchBatcher) add(ctx context.Context, id int64, path string, mtimeUnix int64) {
	b.entries = append(b.entries, touchEntry{id: id, path: path, mtimeUnix: mtimeUnix})
	if len(b.entries) >= batchSize {
		b.flush(ctx)
	}
}

// flush commits every pending touch in one transaction. A failure leaves
// every entry's path in uncertain -- matching drainAndCommit's own
// failed-batch handling -- so the MISSING sweep excludes them rather than
// flipping a live, merely-unwritten file to MISSING. filesFailed is bumped
// too, for the same reason drainAndCommit's own failed-Commit branch bumps
// it: scan_jobs.files_failed must reflect every failure class this pass can
// hit, not just processFile/Commit errors, or an operator watching that
// column undercounts a touch-batch-specific outage.
func (b *touchBatcher) flush(ctx context.Context) {
	if len(b.entries) == 0 {
		return
	}
	entries := b.entries
	b.entries = b.entries[:0]
	err := b.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, e := range entries {
			if err := q.TouchMediaNode(ctx, sqlcgen.TouchMediaNodeParams{ID: e.id, MtimeUnix: e.mtimeUnix}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.log.Error("pipeline: sweep touch batch failed (delayed-not-wrong, retried next pass)", "err", err, "count", len(entries))
		if b.filesFailed != nil {
			b.filesFailed.Add(int32(len(entries)))
		}
		for _, e := range entries {
			b.uncertain.add(e.path)
		}
		return
	}
	b.total += len(entries)
}
