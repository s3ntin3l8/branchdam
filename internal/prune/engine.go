// Package prune implements #61's TTL cache pruning engine: purging Tier-1
// cache files whose Master Node has a verified full_hash on Tier 3 --
// server-computed per full_hash_provenance, never one a paired device only
// asserted (#505; see internal/rehash).
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
// caller), then MarkNodeMissing.
//
// The DB re-verify and the write it gates no longer share one transaction.
// The filesystem calls sit between them, and they have to: the writer
// connection is a process-wide resource, and a stat or an unlink on an NFS
// or SMB mount can block for minutes on a stale handle, so holding the
// writer across them turns one slow mount into a server-wide write stall.
// What protects the purge instead is that the verified Tier-3 ancestor set
// is read on the writer connection both BEFORE the filesystem work and
// again AFTER it, and the purge proceeds only when the two reads describe
// the same ancestors -- see the phase comments below.
//
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
// ErrNoLongerEligible. The authoritative re-check still goes deeper than
// that snapshot for the critical case: it re-reads every verified Tier-3
// ancestor's row on the writer connection, so a Tier-3 master that has
// gone MISSING (or lost its full_hash) by then aborts the purge -- the
// verified-hash gate the original Plan was re-running inside InTx exists
// to enforce. The same read is repeated once more after the on-disk
// verification, so a master archived DURING the filesystem work aborts the
// purge as well. The mtime/TTL portion of Plan's own filter is redundant
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
	return execute(ctx, database, guard, candidates, cutoffUnix, nil)
}

// execute is Execute with a test seam.
//
// afterVerify, when set, runs between the on-disk verification and the
// authoritative re-read -- the exact window the phase split opened, and
// the one a concurrent scan can land its version-collision archive in. A
// test uses it to archive the Tier-3 master there, which is the write
// that must abort the purge. It is nil in production.
func execute(ctx context.Context, database *db.DB, guard *storage.Guard, candidates []Candidate, cutoffUnix int64, afterVerify func()) []Result {
	// Pre-compute eligibility per unique storage location once, before the
	// transaction loop. This replaces the previous per-candidate Plan call
	// inside each InTx, which re-ran the same query for every candidate on
	// the same location.
	//
	// Why a Reader snapshot here is sufficient: the loop below runs every
	// InTx back-to-back in a single process, so the window between this
	// snapshot and each InTx is bounded to milliseconds. The only state
	// change that can invalidate a Plan row is the candidate's Tier-3
	// ancestor going MISSING or losing its full_hash, and the first phase
	// below re-validates exactly that via ListVerifiedTier3Ancestors before
	// any destructive call -- and the second phase re-reads the same set
	// again after the filesystem work, so a master archived in between
	// aborts the purge too. The TTL/ACTIVE filters Plan encodes cannot be
	// invalidated in the opposite direction (a node's mtime cannot un-age,
	// and ACTIVE -> MISSING on the candidate itself is the MarkNodeMissing
	// step, not a concurrent state we'd race with). So the only thing the
	// re-validation is doing that Plan was also doing is re-reading the
	// ancestor's liveness, and it does so under the writer transaction,
	// which sees the up-to-date row.
	//
	// The destructive calls are deliberately OUTSIDE that transaction. The
	// single writer connection is a process-wide resource, and the
	// filesystem calls in the path (an Lstat per ancestor, the unlink) can
	// block for a long time on an NFS or SMB mount -- a stale NFS handle
	// can block for minutes. Holding the writer across them turns a slow
	// mount into a server-wide write stall.
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
		// Cheap reject BEFORE taking the single writer connection: the
		// ancestor + candidate Lstats can be slow on NFS/SMB mounts, and a
		// candidate that fails them never needs the write lock. This is only
		// an optimization -- the authoritative re-verification (invariant 7)
		// still runs in phase 2, after the transaction is closed and
		// immediately before Guard.Remove.
		if !eligibleMap[c.NodeID] {
			results = append(results, Result{Candidate: c, Purged: false, Err: ErrNoLongerEligible})
			continue
		}
		if ancestors, aerr := database.Reader.ListVerifiedTier3Ancestors(ctx, c.NodeID); aerr == nil {
			if _, verr := verifyOnDisk(c, ancestors); verr != nil {
				results = append(results, Result{Candidate: c, Purged: false, Err: verr})
				continue
			}
		}
		// (A reader error just falls through to the authoritative in-tx check.)
		// The reject reads a reader snapshot, so a row changed since can make it
		// a false negative (skipping a candidate the in-tx check would pass):
		// a harmless pessimization, never a correctness issue -- do not infer
		// perfect parity with the in-tx check.
		//
		// Phase 1 -- the authoritative re-verification, DATABASE ONLY. The
		// single writer connection is a process-wide resource, so nothing that
		// touches the filesystem may happen while it is held: an Lstat or an
		// unlink on an NFS or SMB mount can block for seconds, and a stale
		// NFS handle can block for minutes, stalling every write the server
		// does for the duration. So the ancestors are read here, on the
		// writer connection (so the read sees the up-to-date row rather than
		// a reader snapshot), and the transaction is closed before any
		// filesystem call.
		var ancestors []sqlcgen.ListVerifiedTier3AncestorsRow
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			if !eligibleMap[c.NodeID] {
				return ErrNoLongerEligible
			}
			a, err := q.ListVerifiedTier3Ancestors(ctx, c.NodeID)
			if err != nil {
				return err
			}
			ancestors = a
			return nil
		})
		if err != nil {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}

		// Phase 2 -- the filesystem work, with NO writer connection held.
		// This is where invariant 7 is actually enforced: every verified
		// Tier-3 ancestor must still be on disk with the size and mtime it
		// had when its full_hash was verified, and only then may the
		// candidate be unlinked. Holding the writer across these calls was
		// what made a slow mount a server-wide stall.
		gone, err := verifyOnDisk(c, ancestors)
		if err != nil {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}
		if afterVerify != nil {
			afterVerify()
		}

		// Phase 2.5 -- the authoritative re-read, once more, immediately
		// before the unlink.
		//
		// Phase 1 read the ancestors BEFORE the Lstat above, and that
		// Lstat can block for seconds on a Tier-3 master on an NFS or SMB
		// mount. Long enough for a concurrent scan to archive that master
		// and insert its successor -- the version-collision path
		// internal/pipeline.Commit takes when the master's bytes changed
		// under it. That archive is exactly what makes the prune gate
		// fail closed: the archived row leaves ListVerifiedTier3Ancestors,
		// so the candidate's edge chain no longer reaches a live, verified
		// Tier-3 master, and the purge must stop -- the Tier-1 cache may
		// be the last copy of the bytes the successor no longer holds.
		//
		// So the ancestors are read AGAIN here, on the writer connection,
		// after the filesystem work and immediately before the unlink, and
		// the purge proceeds only if the set is the SAME one that was
		// verified on disk. A master that was archived, replaced or
		// re-verified in the meantime changes the set, and the purge is
		// abandoned.
		//
		// What this cannot close: an archive that commits in the sliver
		// between this transaction's commit and the unlink syscall. It
		// needs a whole writer transaction to land there, and the window
		// is the handful of instructions Guard.Remove spends on its own
		// path checks -- not the seconds the Lstat above can take. The
		// alternative (holding the writer across the unlink) is the
		// server-wide write stall this phase split exists to avoid.
		var fresh []sqlcgen.ListVerifiedTier3AncestorsRow
		err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
			a, qerr := q.ListVerifiedTier3Ancestors(ctx, c.NodeID)
			if qerr != nil {
				return qerr
			}
			fresh = a
			return nil
		})
		if err != nil {
			results = append(results, Result{Candidate: c, Purged: false, Err: err})
			continue
		}
		if !sameAncestors(ancestors, fresh) {
			results = append(results, Result{Candidate: c, Purged: false, Err: ErrNoLongerEligible})
			continue
		}
		if !gone {
			if err := guard.Remove(c.FilePath); err != nil {
				results = append(results, Result{Candidate: c, Purged: false, Err: err})
				continue
			}
		}

		// Phase 3 -- record the outcome.
		//
		// The unlink is not transactional: the file is already gone before
		// this write lands. If the write fails, the node still claims a
		// file that no longer exists. That is not left dangling: the next
		// plan pass re-verifies the candidate, verifyOnDisk reports it
		// gone, and the node is marked MISSING then without another
		// unlink. The candidate is reported as an error rather than as a
		// successful purge, so the caller does not count it as reclaimed.
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

// verifyOnDisk is the pre-delete re-verification (#246, #352): every verified
// Tier-3 ancestor must still exist on disk with the mtime/size recorded when
// its full_hash was verified (a rewritten master's stored hash cannot be
// trusted), and the candidate itself must be unchanged since Plan. gone
// reports that the candidate already vanished on its own, so there is nothing
// for Guard to remove but the node's record is stale either way.
//
// Second-granularity mtime, matching mtime_unix's column type: a same-size
// rewrite within the same Unix second is not caught. Inherent to the schema.
func verifyOnDisk(c Candidate, ancestors []sqlcgen.ListVerifiedTier3AncestorsRow) (gone bool, err error) {
	if len(ancestors) == 0 {
		return false, ErrNoLongerEligible
	}
	for _, a := range ancestors {
		aInfo, statErr := os.Lstat(a.FilePath)
		if statErr != nil {
			return false, ErrAncestorUnreachable
		}
		if aInfo.ModTime().Unix() != a.MtimeUnix || aInfo.Size() != a.SizeBytes {
			return false, ErrAncestorUnreachable
		}
	}
	info, statErr := os.Lstat(c.FilePath)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return true, nil
	case statErr != nil:
		return false, statErr
	case info.ModTime().Unix() != c.MtimeUnix || info.Size() != c.SizeBytes:
		return false, ErrFileChangedSincePlan
	}
	return false, nil
}

// sameAncestors reports whether two authoritative reads of a candidate's
// verified Tier-3 ancestors describe the same set.
//
// The comparison is by node id and over the whole row the query returns --
// the path, the size, the mtime and the location -- not by position:
// ListVerifiedTier3Ancestors has no ORDER BY, so two reads of an unchanged
// lineage can return the same rows in a different order.
//
// A difference in either the membership or the recorded values means the
// lineage changed under the purge -- the master was archived and replaced,
// or its row was re-verified -- and the purge must fail closed.
func sameAncestors(verified, fresh []sqlcgen.ListVerifiedTier3AncestorsRow) bool {
	if len(verified) != len(fresh) {
		return false
	}
	byID := make(map[int64]sqlcgen.ListVerifiedTier3AncestorsRow, len(verified))
	for _, row := range verified {
		byID[row.ID] = row
	}
	for _, row := range fresh {
		other, ok := byID[row.ID]
		if !ok {
			return false
		}
		// Compare the whole row, not a hand-picked subset of the columns:
		// the on-disk verification above was done against the values the
		// first read returned, so ANY change to what the query returns --
		// the path, the size, the mtime, the location -- invalidates it.
		// A concurrent scan's TouchMediaNode refreshes mtime_unix while
		// leaving the id and the SERVER provenance in place, so the value
		// branch is reachable, not theoretical.
		if other != row {
			return false
		}
	}
	return true
}
