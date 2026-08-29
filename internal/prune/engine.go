// Package prune implements #61's TTL cache pruning engine: purging Tier-1
// cache files whose Master Node has a verified full_hash on Tier 3.
// Execute is the second (and last) real production caller of
// storage.Guard.Remove.
package prune

import (
	"context"
	"errors"
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
// ancestor file on disk cannot be reached at Execute time -- see Execute's
// own doc comment for why this check exists (#246).
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
// every verified Tier-3 ancestor file is reachable on disk (os.Lstat,
// aborting with ErrAncestorUnreachable if unreachable -- #246), re-verifies
// the on-disk file hasn't changed since Plan, then storage.Guard.Remove
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
// Execute re-runs Plan itself, scoped to each candidate's own location,
// inside the transaction, and aborts that candidate with
// ErrNoLongerEligible if it no longer appears. This closes the DB-side
// TOCTOU between a dry-run response and a later Execute call: without it,
// a Tier-3 master going MISSING (or its full_hash getting invalidated)
// between Plan and Execute would let Execute delete the one remaining
// verified copy of a file -- exactly what the verified-hash gate exists
// to prevent. The mtime/TTL portion of Plan's own filter is redundant
// here (a candidate's mtime cannot un-age), but re-running the whole
// query is simpler and safer than duplicating only the ancestor-liveness
// half of it.
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
	locations := make(map[int64][]Candidate)
	for _, c := range candidates {
		locations[c.StorageLocationID] = append(locations[c.StorageLocationID], c)
	}
	eligibleMap := make(map[int64]bool, len(candidates))
	for locID := range locations {
		planned, err := Plan(ctx, database.Reader, locID, cutoffUnix)
		if err != nil {
			continue
		}
		for _, p := range planned {
			eligibleMap[p.NodeID] = true
		}
	}

	results := make([]Result, 0, len(candidates))
	for _, c := range candidates {
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			if !eligibleMap[c.NodeID] {
				return ErrNoLongerEligible
			}

			// Pre-delete re-verification of the Tier-3 ancestor's file on disk (#246):
			// stat every verified Tier-3 ancestor. If any ancestor is unreachable,
			// refuse to delete the Tier-1 candidate.
			ancestors, err := q.ListVerifiedTier3Ancestors(ctx, c.NodeID)
			if err != nil {
				return err
			}
			if len(ancestors) == 0 {
				return ErrNoLongerEligible
			}
			for _, a := range ancestors {
				if _, statErr := os.Lstat(a.FilePath); statErr != nil {
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
