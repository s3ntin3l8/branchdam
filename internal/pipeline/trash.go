// Package pipeline — trash.go implements the shared TrashAsset /
// RestoreTrashedAsset operations.
//
// The HTTP DELETE /api/v1/assets/{id} endpoint, the POST /api/v1/assets/{id}/trash
// endpoint, and the agent drainer's EVENT_NODE_DELETED handler all delegate
// here. The lifecycle_state is the durable record of "the bytes are no longer
// at this path"; the .trash/<rel> move is the on-disk manifestation.
//
// TRASHED vs MISSING:
//
//   - MISSING is set by the scan watcher when a file that was at this path
//     last_seen_at is not seen this scan. The bytes are still presumed to
//     exist somewhere; we just lost track of them.
//
//   - TRASHED is set by TrashAsset. The bytes have been moved into
//     <storage_root>/.trash/<rel_path> (with a collision suffix if a prior
//     trashed copy of the same relative path already lives there) and the
//     row's lifecycle_state is recorded as TRASHED. A 30-day TTL pruner is
//     responsible for the final unlink; until then, the file is restorable.
//
// TRASHED vs ARCHIVED:
//
//   - ARCHIVED is the soft-delete state used by version-collision supersedes
//     (spec Pillar 3). The bytes stay at the original path until a TTL
//     pruner or the user explicitly clears them. ARCHIVED rows are reachable
//     from their successor's lineage view.
//
//   - TRASHED is the user-initiated destructive delete. The bytes have been
//     moved off the original path. TRASHED rows are NOT reachable from any
//     other node's lineage (parent_alive in v_media_edges_resolved excludes
//     TRASHED alongside MISSING/ARCHIVED) and they have no successor.
package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// Sentinel errors callers translate into HTTP responses.
//
//	asset already in the target state (idempotent re-trash / re-restore)
//	HTTP 200 with {ok:true} is preferred over 4xx for these
//
//	ErrAssetNotTrashed  / ErrAssetNotArchived : node was not in a trashable/restorable state
//	ErrAssetTrashFileMissing : node is TRASHED but the .trash/ copy is gone (or was never moved -- e.g. virtual location)
//	ErrAssetHasSuperseded : node has a superseded_by row, cannot restore
//	ErrAssetPathCollision : another live node already occupies the restore target path
var (
	ErrAssetNotTrashed       = errors.New("pipeline: asset is not in TRASHED state")
	ErrAssetTrashFileMissing = errors.New("pipeline: asset is TRASHED but its file is missing from .trash/")
	ErrAssetHasSuperseded    = errors.New("pipeline: asset has been superseded; restore forbidden")
	ErrAssetPathCollision    = errors.New("pipeline: another live asset already occupies the restore target path")
)

// TrashAsset moves the master file to <root>/.trash/<rel_path>, marks the node
// TRASHED, and (when keepExports is false) deletes any linked Tier-2 export
// files and marks their nodes TRASHED too. When keepExports is true, only the
// master moves -- Tier-2 exports stay ACTIVE on disk so the cloud gallery is
// not disturbed.
//
// Returns the post-trash MediaNode (with lifecycle_state = 'TRASHED'). Idempotent
// for already-TRASHED nodes (returns the existing row without touching disk).
//
// Caller owns the audit write -- pipeline.TrashAsset does not write to the
// actor_audit log; the HTTP layer (and the agent drainer) own that so each
// caller can attach its own caller-specific context (principal, agent id).
func TrashAsset(
	ctx context.Context,
	database *db.DB,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
	keepExports bool,
) (sqlcgen.MediaNode, error) {
	var node sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var innerErr error
		node, innerErr = TrashAssetTx(ctx, q, guard, log, nodeID, keepExports)
		return innerErr
	})
	return node, err
}

// TrashAssetTx is the inner transactional body of TrashAsset, extracted so
// callers already inside a transaction (the agent drainer's applyNodeDeleted
// runs inside ProcessPending's InTx) can avoid the nested-tx deadlock that
// would otherwise happen against db.DB's single-connection writer pool
// (Invariant 2).
func TrashAssetTx(
	ctx context.Context,
	q *sqlcgen.Queries,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
	keepExports bool,
) (sqlcgen.MediaNode, error) {
	loaded, err := q.GetMediaNodeByID(ctx, nodeID)
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("load node %d: %w", nodeID, err)
	}
	if loaded.LifecycleState == "TRASHED" {
		return loaded, nil
	}
	if loaded.LifecycleState == "ARCHIVED" {
		return sqlcgen.MediaNode{}, fmt.Errorf("%w: cannot trash an ARCHIVED asset (id=%d); restore first", ErrAssetNotTrashed, nodeID)
	}

	if guard != nil && loaded.FilePath != "" {
		loc, resolveErr := guard.Resolve(loaded.FilePath)
		if resolveErr != nil {
			return sqlcgen.MediaNode{}, fmt.Errorf("resolve storage location for %s: %w", loaded.FilePath, resolveErr)
		}
		switch {
		case loc.IsVirtual:
			// Virtual locations have no bytes on disk to move. The TRASHED
			// state still records intent; the file path is just a
			// bookkeeping row.
		case loc.ReadOnly || loc.Tier == "TIER3_MASTER_ARCHIVE":
			// Tier 3 master archives and read-only tiers must NEVER be
			// written to (AGENTS.md invariant #3: Tier 3 is read-only by
			// design; a user-initiated "trash" against a Tier 3 master is
			// a logical-only operation -- the row is marked TRASHED so it
			// disappears from listings, but the bytes stay on disk
			// untouched. The original PR #315 behavior was to mark the
			// row MISSING; the TRASHED state is its explicit-successor
			// per the EVENT_NODE_DELETED → TRASHED mapping.
			log.Warn("trash: skipping physical move for read-only/tier3 master; row marked TRASHED logically only",
				"nodeID", loaded.ID, "tier", loc.Tier, "path", loaded.FilePath)
		default:
			if err := moveToTrash(guard, loc.RootPath, loaded.FilePath, log); err != nil {
				return sqlcgen.MediaNode{}, fmt.Errorf("move to trash: %w", err)
			}
		}
	}

	if !keepExports {
		if err := purgeLinkedExports(ctx, q, guard, log, loaded.ID); err != nil {
			return sqlcgen.MediaNode{}, fmt.Errorf("purge linked exports: %w", err)
		}
	}

	if err := q.MarkNodeTrashed(ctx, loaded.ID); err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("mark node trashed: %w", err)
	}

	if err := q.DeleteRemoteSyncStateForNode(ctx, loaded.ID); err != nil {
		log.Warn("delete remote sync state on trash", "nodeID", loaded.ID, "err", err)
	}

	if !keepExports {
		if err := purgeRemoteSyncStateForLinkedExports(ctx, q, loaded.ID); err != nil {
			log.Warn("delete remote sync state for linked exports", "nodeID", loaded.ID, "err", err)
		}
	}

	updated, err := q.GetMediaNodeByID(ctx, loaded.ID)
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("reload node %d: %w", loaded.ID, err)
	}
	return updated, nil
}

// RestoreTrashedAsset moves the file back from .trash/<rel> to the original
// path, sets lifecycle_state back to ACTIVE, and (when linked exports were
// also trashed by a keepExports=false TrashAsset call) restores them too.
//
// Idempotent for already-ACTIVE nodes (returns the existing row). Returns
// ErrAssetNotTrashed / ErrAssetTrashFileMissing / ErrAssetHasSuperseded /
// ErrAssetPathCollision for the respective failure modes.
func RestoreTrashedAsset(
	ctx context.Context,
	database *db.DB,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
) (sqlcgen.MediaNode, error) {
	var node sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var innerErr error
		node, innerErr = RestoreTrashedAssetTx(ctx, q, guard, log, nodeID)
		return innerErr
	})
	return node, err
}

// RestoreTrashedAssetTx is the inner transactional body of RestoreTrashedAsset.
// See TrashAssetTx's doc comment for why this exists.
func RestoreTrashedAssetTx(
	ctx context.Context,
	q *sqlcgen.Queries,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
) (sqlcgen.MediaNode, error) {
	loaded, err := q.GetMediaNodeByID(ctx, nodeID)
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("load node %d: %w", nodeID, err)
	}
	if loaded.LifecycleState != "TRASHED" {
		return sqlcgen.MediaNode{}, fmt.Errorf("%w: node %d lifecycle_state=%s", ErrAssetNotTrashed, nodeID, loaded.LifecycleState)
	}

	if loaded.SupersededBy.Valid && loaded.SupersededBy.Int64 != 0 {
		return sqlcgen.MediaNode{}, fmt.Errorf("%w: node %d superseded by %d", ErrAssetHasSuperseded, nodeID, loaded.SupersededBy.Int64)
	}

	live, err := q.GetLiveNodeByPath(ctx, loaded.FilePath)
	if err == nil && live.ID != nodeID {
		return sqlcgen.MediaNode{}, fmt.Errorf("%w: node %d already occupies %s", ErrAssetPathCollision, live.ID, loaded.FilePath)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sqlcgen.MediaNode{}, fmt.Errorf("check live node by path: %w", err)
	}

	if guard != nil && loaded.FilePath != "" {
		if err := restoreFromTrash(guard, loaded.FilePath, log); err != nil {
			return sqlcgen.MediaNode{}, err
		}
	}

	if err := q.UntrashNode(ctx, loaded.ID); err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("untrash node: %w", err)
	}

	if err := restoreLinkedExports(ctx, q, guard, log, loaded.ID); err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("restore linked exports: %w", err)
	}

	updated, err := q.GetMediaNodeByID(ctx, loaded.ID)
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("reload node %d: %w", loaded.ID, err)
	}
	return updated, nil
}

// moveToTrash moves filePath to <rootPath>/.trash/<rel_path>, creating the
// .trash/ directory tree as needed. If a prior trashed copy of the same rel
// path already lives in .trash/, a numeric suffix (_1, _2, ...) is appended
// to avoid clobbering.
//
// If the source file is missing on disk, this logs a warning and returns nil --
// the DB row's TRASHED state is still the durable record of intent; the user
// (or an upstream sync) may have already moved the file. Refusing the trash
// in that case would leave the row in an ACTIVE/MISSING limbo forever.
func moveToTrash(guard *storage.Guard, rootPath, filePath string, log *slog.Logger) error {
	relPath, err := filepath.Rel(rootPath, filePath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return fmt.Errorf("compute rel path for %s under %s: %w", filePath, rootPath, err)
	}
	trashPath := filepath.Join(rootPath, ".trash", relPath)
	if err := guard.MkdirAll(filepath.Dir(trashPath), 0o755); err != nil {
		return fmt.Errorf("mkdir .trash parent: %w", err)
	}
	if _, statErr := os.Stat(filePath); statErr != nil {
		if os.IsNotExist(statErr) {
			log.Warn("trash: source file already missing on disk, marking TRASHED anyway", "path", filePath)
			return nil
		}
		return fmt.Errorf("stat source %s: %w", filePath, statErr)
	}
	trashPath = uniqueTrashPath(trashPath)
	if err := os.Rename(filePath, trashPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", filePath, trashPath, err)
	}
	now := time.Now().UTC()
	if chErr := os.Chtimes(trashPath, now, now); chErr != nil {
		log.Warn("stamp trash mtime", "path", trashPath, "err", chErr)
	}
	return nil
}

// restoreFromTrash moves a trashed file back to its original path. Computes
// the .trash/<rel_path> location, finds the unique-suffixed file if one
// exists, and renames it back. Returns ErrAssetTrashFileMissing if no .trash
// copy can be found (TTL expired and the pruner already removed it).
func restoreFromTrash(guard *storage.Guard, filePath string, log *slog.Logger) error {
	loc, err := guard.Resolve(filePath)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", filePath, err)
	}
	if loc.IsVirtual {
		return nil
	}
	relPath, err := filepath.Rel(loc.RootPath, filePath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return fmt.Errorf("compute rel path for %s under %s: %w", filePath, loc.RootPath, err)
	}
	canonicalTrash := filepath.Join(loc.RootPath, ".trash", relPath)

	if _, statErr := os.Stat(filePath); statErr == nil {
		return nil
	}

	trashPath, err := findTrashPath(canonicalTrash)
	if err != nil {
		return err
	}
	if err := guard.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return fmt.Errorf("mkdir restore parent: %w", err)
	}
	if err := os.Rename(trashPath, filePath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", trashPath, filePath, err)
	}
	log.Info("restored from trash", "from", trashPath, "to", filePath)
	return nil
}

func uniqueTrashPath(canonical string) string {
	if _, err := os.Stat(canonical); os.IsNotExist(err) {
		return canonical
	}
	ext := filepath.Ext(canonical)
	stem := strings.TrimSuffix(canonical, ext)
	for i := 1; ; i++ {
		candidate := stem + "_" + strconv.Itoa(i) + ext
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func findTrashPath(canonical string) (string, error) {
	if _, err := os.Stat(canonical); err == nil {
		return canonical, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat %s: %w", canonical, err)
	}
	ext := filepath.Ext(canonical)
	stem := strings.TrimSuffix(canonical, ext)
	for i := 1; ; i++ {
		candidate := stem + "_" + strconv.Itoa(i) + ext
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
		if i > 9999 {
			return "", fmt.Errorf("%w: too many suffixed candidates for %s", ErrAssetTrashFileMissing, canonical)
		}
	}
}

// purgeLinkedExports deletes the files for every Tier-2 export node linked to
// the master via a FINAL_EXPORT or immich_export edge, and marks those nodes
// MISSING -- they will be re-trashed by the outer MarkNodeTrashed call on the
// master? No: we mark them TRASHED here. The caller runs MarkNodeTrashed on
// the master AFTER this returns.
//
// Note: this runs inside the same transaction as the master's MarkNodeTrashed
// call. If a linked export's file is in a read-only tier (Immich's exports
// can be on TIER2_EXPORTS which is read-write per the storage policy), the
// guard refuses the Remove and the transaction rolls back.
func purgeLinkedExports(ctx context.Context, q *sqlcgen.Queries, guard *storage.Guard, log *slog.Logger, parentID int64) error {
	edges, err := q.ListEdgesBySource(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list edges by source: %w", err)
	}
	for _, edge := range edges {
		if edge.RelationshipType != "FINAL_EXPORT" && edge.Resolver != "immich_export" {
			continue
		}
		exp, err := q.GetMediaNodeByID(ctx, edge.TargetNodeID)
		if err != nil {
			log.Warn("load export node", "nodeID", edge.TargetNodeID, "err", err)
			continue
		}
		if exp.LifecycleState == "TRASHED" || exp.LifecycleState == "MISSING" {
			continue
		}
		if guard != nil && exp.FilePath != "" {
			expLoc, resolveErr := guard.Resolve(exp.FilePath)
			if resolveErr != nil {
				log.Warn("resolve export location", "path", exp.FilePath, "err", resolveErr)
				continue
			}
			if expLoc.ReadOnly || expLoc.IsVirtual {
				// Cannot physically remove from a read-only or virtual tier.
				// Leave the export node ACTIVE on disk; the master's
				// TRASHED state is the user-visible "this asset is gone"
				// signal -- the read-only export simply stays as a stale
				// mirror. Matches the original PR #315 behavior: read-only
				// exports were never touched by EVENT_NODE_DELETED.
				log.Warn("trash: skipping read-only/virtual export; leaving ACTIVE on disk",
					"exportID", exp.ID, "tier", expLoc.Tier, "path", exp.FilePath)
				continue
			}
			if rmErr := guard.Remove(exp.FilePath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("remove export %s: %w", exp.FilePath, rmErr)
			}
		} else if exp.FilePath != "" {
			if rmErr := os.Remove(exp.FilePath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("remove export %s: %w", exp.FilePath, rmErr)
			}
		}
		if err := q.MarkNodeTrashed(ctx, exp.ID); err != nil {
			return fmt.Errorf("mark export trashed: %w", err)
		}
		if err := q.DeleteRemoteSyncStateForNode(ctx, exp.ID); err != nil {
			log.Warn("delete remote sync state for export", "nodeID", exp.ID, "err", err)
		}
	}
	return nil
}

func purgeRemoteSyncStateForLinkedExports(ctx context.Context, q *sqlcgen.Queries, parentID int64) error {
	edges, err := q.ListEdgesBySource(ctx, parentID)
	if err != nil {
		return err
	}
	for _, edge := range edges {
		if edge.RelationshipType != "FINAL_EXPORT" && edge.Resolver != "immich_export" {
			continue
		}
		if err := q.DeleteRemoteSyncStateForNode(ctx, edge.TargetNodeID); err != nil {
			return err
		}
	}
	return nil
}

// restoreLinkedExports restores any TRASHED export nodes linked to this master,
// moving their files back from .trash/<rel> to the export's original path.
// No-op if no linked exports are TRASHED (the keepExports=true case).
func restoreLinkedExports(ctx context.Context, q *sqlcgen.Queries, guard *storage.Guard, log *slog.Logger, parentID int64) error {
	edges, err := q.ListEdgesBySource(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list edges by source: %w", err)
	}
	for _, edge := range edges {
		if edge.RelationshipType != "FINAL_EXPORT" && edge.Resolver != "immich_export" {
			continue
		}
		exp, err := q.GetMediaNodeByID(ctx, edge.TargetNodeID)
		if err != nil {
			log.Warn("load export node", "nodeID", edge.TargetNodeID, "err", err)
			continue
		}
		if exp.LifecycleState != "TRASHED" {
			continue
		}
		if guard != nil && exp.FilePath != "" {
			if err := restoreFromTrash(guard, exp.FilePath, log); err != nil {
				return fmt.Errorf("restore export %s: %w", exp.FilePath, err)
			}
		}
		if err := q.UntrashNode(ctx, exp.ID); err != nil {
			return fmt.Errorf("untrash export: %w", err)
		}
	}
	return nil
}
