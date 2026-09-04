// Package prune implements #61's TTL cache pruning engine: purging Tier-1
// cache files whose Master Node has a verified full_hash on Tier 3.
// Execute is the second (and last) real production caller of
// storage.Guard.Remove.
package prune

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// ErrNoLongerEligible is returned for a candidate whose verified Tier-3
// ancestor no longer exists by the time Execute gets to it -- see
// Execute's own doc comment for why this re-check exists.
var ErrNoLongerEligible = errors.New("node is no longer eligible for pruning: verified Tier-3 ancestor lost since Plan")

// ErrAncestorUnreachable is returned for a candidate whose verified Tier-3
// ancestor file on disk cannot be reached or no longer matches its stored
// mtime/size at Execute time -- see Execute's own doc comment for why this
// check exists (#246, #352).
var ErrAncestorUnreachable = errors.New("verified Tier-3 ancestor file on disk is unreachable: refusing to delete candidate")

// ErrFileChangedSincePlan is returned for a candidate whose on-disk file no
// longer matches the (mtime, size) Plan recorded -- see Execute's own doc
// comment for why this re-check exists.
var ErrFileChangedSincePlan = errors.New("file on disk changed since it was planned for pruning: refusing to delete")

// Candidate is one node eligible for pruning: ACTIVE, past its location's
// TTL, with a live Tier-3 ancestor carrying a verified full_hash.
type Candidate struct {
	NodeID            int64
	NodeUUID          string
	FilePath          string
	FileName          string
	SizeBytes         int64
	MtimeUnix         int64
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
			NodeID: r.ID, NodeUUID: r.NodeUuid, FilePath: r.FilePath, FileName: r.FileName,
			SizeBytes: r.SizeBytes, MtimeUnix: r.MtimeUnix, StorageLocationID: r.StorageLocationID,
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

// Execute purges every candidate: re-verifies DB eligibility, re-verifies
// every verified Tier-3 ancestor file is reachable on disk and matches its
// stored (mtime, size) (os.Lstat, aborting with ErrAncestorUnreachable if
// unreachable or modified -- #246, #352), re-verifies the on-disk candidate file
// hasn't changed since Plan, then storage.Guard.Remove
// (so a read-only tier or a symlink escape is refused before any DB write
// -- resolved and defeated by Guard.CheckWrite's own canonicalize step,
// the same defense TestSymlinkEscapeRefused proves for every other Guard
// caller), then MarkNodeMissing -- the DB re-verify and the write it gates
// share one transaction, so they can never observe different states.
// Rows are never deleted, matching this repo's invariant -- a purged node
// lands in MISSING, the same state a vanished file gets; TouchMediaNode
// reactivates it if the cache is later regenerated at the same path, and
// RebaseMissingNodePath can adopt its fast_hash if identical content
// reappears elsewhere. Both are the desired semantics for a cache file.
//
// cutoffUnix is the same TTL cutoff the caller's Plan call used --
// Execute pre-runs Plan once per unique storage location against the Reader
// (see the pre-compute block below for why this is sufficient) and uses
// that snapshot as the per-candidate eligibility set inside the transaction
// loop. A candidate not in the snapshot is aborted with
// ErrNoLongerEligible. The in-transaction re-check still goes deeper than
// that snapshot for the critical case: it re-validates every verified
// Tier-3 ancestor's row in the same writer transaction, so a Tier-3 master
// going MISSING (or its full_hash getting invalidated) in the milliseconds
// between the pre-compute and the commit still aborts the purge -- the
// verified-hash gate the original Plan was re-running inside InTx exists
// to enforce. The mtime/TTL portion of Plan's own filter is redundant
// across this gap (a candidate's mtime cannot un-age, and ACTIVE -> MISSING
// on the candidate itself only happens via the InTx's own MarkNodeMissing
// step, not a concurrent change we'd race with).
//
// The DB re-check alone isn't sufficient, though: media_nodes.mtime_unix
// is itself only as fresh as the last scan/sweep that touched this node,
// so the row can be stale relative to the real file even when the DB-side
// eligibility check above passes cleanly -- e.g. an application
// regenerated the cache file with fresh content moments before Execute
// runs, and no scan has observed that yet. Deleting in that window would
// destroy live, freshly-written content the DB has no idea is new. A
// Lstat immediately before Guard.Remove -- deliberately NOT following a
// symlink, the same defense-in-depth direction Guard.CheckWrite's own
// canonicalize step takes -- catches this: an (mtime, size) mismatch
// against what Plan recorded aborts with ErrFileChangedSincePlan, and a
// file that's already gone (fs.ErrNotExist) is treated as nothing left to
// remove, so the node still lands in MISSING rather than erroring on a
// file that vanished on its own.
//
// A per-candidate failure never aborts the batch: one re-check failure,
// one Guard refusal, or one failed transaction is recorded in that
// candidate's Result and the rest still run. There is no rollback across
// candidates -- each purge is independently committed as it happens, so a
// crash partway through leaves some files gone and their nodes MISSING,
// and the remainder untouched; re-running Plan against the same location
// picks up exactly where it left off.
func Execute(ctx context.Context, database *db.DB, guard *storage.Guard, candidates []Candidate, cutoffUnix int64) []Result {
	// Pre-compute eligibility per unique storage location once, before the
	// transaction loop. This replaces the previous per-candidate Plan call
	// inside each InTx, which re-ran the same query for every candidate on
	// the same location.
	//
	// Why a Reader snapshot here is sufficient: the loop below runs every
	// InTx back-to-back in a single process, so the window between this
	// snapshot and each InTx is bounded to milliseconds. The only state
	// change that can invalidate a Plan row is the candidate's Tier-3
	// ancestor going MISSING or losing its full_hash, and the InTx body
	// re-validates exactly that via ListVerifiedTier3Ancestors before any
	// destructive call -- the TTL/ACTIVE filters Plan encodes cannot be
	// invalidated in the opposite direction (a node's mtime cannot un-age,
	// and ACTIVE -> MISSING on the candidate itself is the InTx's own
	// MarkNodeMissing step, not a concurrent state we'd race with). So
	// the only thing the InTx check is doing that Plan was also doing is
	// re-reading the ancestor's liveness, and it does so under the writer
	// transaction, which sees the up-to-date row.
	locations := make(map[int64][]Candidate)
	for _, c := range candidates {
		locations[c.StorageLocationID] = append(locations[c.StorageLocationID], c)
	}
	eligibleMap := make(map[int64]bool, len(candidates))
	// failedLocations records locations whose Plan call errored; the
	// candidates from those locations are appended to results with the
	// real error and skipped in the loop below, so the InTx path never
	// runs for them and doesn't overwrite the wrapped error.
	failedLocations := make(map[int64]struct{}, len(locations))
	// errByLocation is a pre-computed error per failed location. Built
	// in the pre-compute loop (order doesn't matter) and read in the
	// candidate loop to assemble results in input order.
	errByLocation := make(map[int64]error, len(locations))
	for locID := range locations {
		planned, err := Plan(ctx, database.Reader, locID, cutoffUnix)
		if err != nil {
			failedLocations[locID] = struct{}{}
			errByLocation[locID] = fmt.Errorf("plan eligibility for location %d: %w", locID, err)
			continue
		}
		for _, p := range planned {
			eligibleMap[p.NodeID] = true
		}
	}

	// Iterate candidates in input order so Result slice ordering
	// matches the request. Previously a per-location grouping loop
	// (map iteration over locations) could reorder Results vs.
	// input candidates, which the caller (handlePrune) reports
	// independently so nothing was misattributed, but the contract
	// was lossy for no reason.
	results := make([]Result, 0, len(candidates))
	for _, c := range candidates {
		if err, ok := errByLocation[c.StorageLocationID]; ok {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			if !eligibleMap[c.NodeID] {
				return ErrNoLongerEligible
			}

			// Pre-delete re-verification of the Tier-3 ancestor's file on disk (#246, #352):
			// stat every verified Tier-3 ancestor. If any ancestor is unreachable or has changed
			// (mtime/size mismatch against stored DB row), refuse to delete the Tier-1 candidate.
			// Re-verifying mtime and size ensures the on-disk master hasn't been rewritten or
			// replaced since its verified full_hash was recorded; if it was modified, the
			// stored full_hash cannot be trusted to represent the current file on disk, so we
			// treat it as an unverified/unreachable master and skip deletion.
			ancestors, err := q.ListVerifiedTier3Ancestors(ctx, c.NodeID)
			if err != nil {
				return err
			}
			if len(ancestors) == 0 {
				return ErrNoLongerEligible
			}
			for _, a := range ancestors {
				aInfo, statErr := os.Lstat(a.FilePath)
				if statErr != nil {
					return ErrAncestorUnreachable
				}
				if aInfo.ModTime().Unix() != a.MtimeUnix || aInfo.Size() != a.SizeBytes {
					// Second-granularity check matching mtime_unix's column type.
					// A modified ancestor means the DB's verified full_hash is stale
					// relative to the on-disk master file.
					return ErrAncestorUnreachable
				}
			}

			info, statErr := os.Lstat(c.FilePath)
			switch {
			case errors.Is(statErr, fs.ErrNotExist):
				// Already gone on its own -- nothing for Guard to remove,
				// but the node's record is stale either way.
			case statErr != nil:
				return statErr
			case info.ModTime().Unix() != c.MtimeUnix || info.Size() != c.SizeBytes:
				// Second-granularity, matching mtime_unix's own column type: a
				// same-size rewrite landing within the same Unix second as the
				// original would not be caught here. Inherent to the schema,
				// not a gap introduced by this check.
				return ErrFileChangedSincePlan
			default:
				if err := guard.Remove(c.FilePath); err != nil {
					return err
				}
			}
			return q.MarkNodeMissing(ctx, c.NodeID)
		})
		if err != nil {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}
		results = append(results, Result{Candidate: c, Purged: true})
	}
	return results
}
