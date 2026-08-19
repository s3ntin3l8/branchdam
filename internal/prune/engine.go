// Package prune implements #61's TTL cache pruning engine: purging Tier-1
// cache files whose Master Node has a verified full_hash on Tier 3.
// Execute is the second (and last) real production caller of
// storage.Guard.Remove.
package prune

import (
	"context"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// Candidate is one node eligible for pruning: ACTIVE, past its location's
// TTL, with a live Tier-3 ancestor carrying a verified full_hash.
type Candidate struct {
	NodeID            int64
	NodeUUID          string
	FilePath          string
	FileName          string
	SizeBytes         int64
	StorageLocationID int64
}

// Plan returns every pruning candidate on locationID whose mtime_unix is
// older than cutoffUnix. It never mutates anything -- Plan is what backs a
// dry-run response, and Execute (below) is a separate, explicit step so a
// caller can inspect the plan before ever touching disk.
func Plan(ctx context.Context, reader *sqlcgen.Queries, locationID, cutoffUnix int64) ([]Candidate, error) {
	rows, err := reader.ListPrunableNodes(ctx, sqlcgen.ListPrunableNodesParams{
		StorageLocationID: locationID,
		MtimeUnix:         cutoffUnix,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, len(rows))
	for i, r := range rows {
		out[i] = Candidate{
			NodeID: r.ID, NodeUUID: r.NodeUuid, FilePath: r.FilePath,
			FileName: r.FileName, SizeBytes: r.SizeBytes, StorageLocationID: r.StorageLocationID,
		}
	}
	return out, nil
}

// Result reports what happened to one candidate during Execute.
type Result struct {
	Candidate
	Purged bool
	Err    error // nil when Purged; the reason otherwise (Guard refusal or a DB error)
}

// Execute purges every candidate: storage.Guard.Remove first (so a
// read-only tier or a symlink escape is refused before any DB write --
// resolved and defeated by Guard.CheckWrite's own canonicalize step, the
// same defense TestSymlinkEscapeRefused proves for every other Guard
// caller), then MarkNodeMissing in its own transaction. Rows are never
// deleted, matching this repo's invariant -- a purged node lands in
// MISSING, the same state a vanished file gets; TouchMediaNode reactivates
// it if the cache is later regenerated at the same path, and
// RebaseMissingNodePath can adopt its fast_hash if identical content
// reappears elsewhere. Both are the desired semantics for a cache file.
//
// A per-candidate failure never aborts the batch: one Guard refusal or one
// failed transaction is recorded in that candidate's Result and the rest
// still run. There is no rollback across candidates -- each purge is
// independently committed as it happens, so a crash partway through leaves
// some files gone and their nodes MISSING, and the remainder untouched;
// re-running Plan against the same location picks up exactly where it left
// off.
func Execute(ctx context.Context, database *db.DB, guard *storage.Guard, candidates []Candidate) []Result {
	results := make([]Result, 0, len(candidates))
	for _, c := range candidates {
		if err := guard.Remove(c.FilePath); err != nil {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}
		if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.MarkNodeMissing(ctx, c.NodeID)
		}); err != nil {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}
		results = append(results, Result{Candidate: c, Purged: true})
	}
	return results
}
