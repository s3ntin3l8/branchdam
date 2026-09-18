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
	ErrAssetAlreadyExists    = errors.New("pipeline: original file and trash copy both exist; ambiguous restore")
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
			if err := moveToTrash(guard, loc.RootPath, loaded.FilePath, loaded.NodeUuid, log); err != nil {
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
		if err := restoreFromTrash(guard, loaded.FilePath, loaded.NodeUuid, log); err != nil {
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

// moveToTrash moves filePath to <rootPath>/.trash/<rel_path>.<nodeUUID>.<ext>,
// creating the .trash/ directory tree as needed. The nodeUUID is embedded in
// the filename so each trashed node's bytes are uniquely identifiable on disk
// without consulting the DB -- critical for the collision case where the same
// rel_path has been trashed twice for two different node_uuids (Hermes
// review, trash.go:333 in the original implementation).
//
// If the source file is missing on disk, this logs a warning and returns nil --
// the DB row's TRASHED state is still the durable record of intent; the user
// (or an upstream sync) may have already moved the file. Refusing the trash
// in that case would leave the row in an ACTIVE/MISSING limbo forever.
func moveToTrash(guard *storage.Guard, rootPath, filePath, nodeUUID string, log *slog.Logger) error {
	relPath, err := filepath.Rel(rootPath, filePath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return fmt.Errorf("compute rel path for %s under %s: %w", filePath, rootPath, err)
	}
	if nodeUUID == "" {
		return fmt.Errorf("moveToTrash requires a non-empty nodeUUID for filename-uniqueness")
	}
	// Embed the node_uuid in the trash filename so the same rel_path can
	// be trashed by N nodes with N distinct, recoverable copies.
	ext := filepath.Ext(relPath)
	stem := strings.TrimSuffix(relPath, ext)
	uuidShort := nodeUUID
	if len(uuidShort) > 8 {
		uuidShort = uuidShort[:8]
	}
	trashRel := stem + "." + uuidShort + ext
	trashPath := filepath.Join(rootPath, ".trash", trashRel)
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
	if err := os.Rename(filePath, trashPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", filePath, trashPath, err)
	}
	now := time.Now().UTC()
	if chErr := os.Chtimes(trashPath, now, now); chErr != nil {
		log.Warn("stamp trash mtime", "path", trashPath, "err", chErr)
	}
	return nil
}

// restoreFromTrash moves a trashed file back to its original path. The trash
// filename is deterministic given (rel_path, node_uuid) (see moveToTrash), so
// we can compute the exact .trash path without scanning. Returns
// ErrAssetTrashFileMissing if no .trash copy can be found (TTL expired and
// the pruner already removed it).
//
// Three terminal cases:
//   - original path missing, trash copy present: rename trash -> original.
//     This is the normal restore.
//   - original path present, no trash copy: the trashed node was logical-only
//     (Tier 3 master archive or read-only tier -- bytes never moved). The
//     row's TRASHED state was the user-visible signal; nothing to do on
//     disk. Silent no-op, log info.
//   - original path present AND trash copy present: ambiguous -- a re-ingest
//     or manual copy may have placed new bytes at the original path while a
//     trash copy still exists. Returning ErrAssetAlreadyExists forces the
//     caller (HTTP layer / drainer) to surface the conflict to the user
//     instead of silently overwriting one copy with the other. Same shape
//     as the live-path collision check earlier in the restore pipeline.
func restoreFromTrash(guard *storage.Guard, filePath, nodeUUID string, log *slog.Logger) error {
	loc, err := guard.Resolve(filePath)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", filePath, err)
	}
	if loc.IsVirtual {
		return nil
	}
	if nodeUUID == "" {
		return fmt.Errorf("restoreFromTrash requires a non-empty nodeUUID")
	}
	relPath, err := filepath.Rel(loc.RootPath, filePath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return fmt.Errorf("compute rel path for %s under %s: %w", filePath, loc.RootPath, err)
	}

	ext := filepath.Ext(relPath)
	stem := strings.TrimSuffix(relPath, ext)
	uuidShort := nodeUUID
	if len(uuidShort) > 8 {
		uuidShort = uuidShort[:8]
	}
	trashPath := filepath.Join(loc.RootPath, ".trash", stem+"."+uuidShort+ext)

	originalExists := false
	if _, statErr := os.Stat(filePath); statErr == nil {
		originalExists = true
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat original %s: %w", filePath, statErr)
	}

	trashExists := false
	if _, err := os.Stat(trashPath); err == nil {
		trashExists = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", trashPath, err)
	}

	switch {
	case !originalExists && trashExists:
		// Normal restore: rename .trash/<rel>.<uuid>.<ext> -> <rel>.<ext>.
		if err := guard.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return fmt.Errorf("mkdir restore parent: %w", err)
		}
		if err := os.Rename(trashPath, filePath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", trashPath, filePath, err)
		}
		log.Info("restored from trash", "from", trashPath, "to", filePath)
		return nil

	case originalExists && !trashExists:
		// Logical-only trash (Tier 3 / read-only / virtual tier): the bytes
		// never moved because they were protected. The DB row was marked
		// TRASHED as a user-visible signal; nothing to do on disk.
		log.Info("restore: file already at original path (logical-only trash); no disk move needed",
			"path", filePath)
		return nil

	case !originalExists && !trashExists:
		return fmt.Errorf("%w: %s", ErrAssetTrashFileMissing, trashPath)

	default:
		// originalExists && trashExists: ambiguous. Refuse to silently
		// clobber either copy. Caller decides what to do.
		return fmt.Errorf("%w: original path %s and trash copy %s both exist; refusing to silently clobber", ErrAssetAlreadyExists, filePath, trashPath)
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
		// Exports share the .trash/ buffer with the master so a restore
		// of the master also restores its exports atomically (the master's
		// restoreLinkedExports pass looks up .trash/<export rel> just like
		// the master's own move). For read-only or virtual tiers there is
		// no bytes to move -- mirror the master's read-only/tier3
		// logical-only trash path and leave the export ACTIVE on disk.
		if guard != nil && exp.FilePath != "" {
			expLoc, resolveErr := guard.Resolve(exp.FilePath)
			if resolveErr != nil {
				log.Warn("resolve export location", "path", exp.FilePath, "err", resolveErr)
				continue
			}
			if expLoc.ReadOnly || expLoc.IsVirtual {
				log.Warn("trash: skipping read-only/virtual export; leaving ACTIVE on disk",
					"exportID", exp.ID, "tier", expLoc.Tier, "path", exp.FilePath)
				continue
			}
			if err := moveToTrash(guard, expLoc.RootPath, exp.FilePath, exp.NodeUuid, log); err != nil {
				return fmt.Errorf("move export to trash: %w", err)
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
//
// Tolerates missing .trash/ copies (TTL pruner may have already purged them,
// or the export was on a read-only/virtual tier at trash time and was never
// moved into .trash/ in the first place): in those cases the export's
// lifecycle_state still flips back to ACTIVE so the master's restore succeeds
// as a whole, and a warning is logged. The user can re-export if needed.
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
			if err := restoreFromTrash(guard, exp.FilePath, exp.NodeUuid, log); err != nil {
				if errors.Is(err, ErrAssetTrashFileMissing) {
					log.Warn("trash copy for export is missing (TTL expired or never moved); export restored logically only -- re-export may be needed",
						"exportID", exp.ID, "filePath", exp.FilePath)
				} else {
					return fmt.Errorf("restore export %s: %w", exp.FilePath, err)
				}
			}
		}
		if err := q.UntrashNode(ctx, exp.ID); err != nil {
			return fmt.Errorf("untrash export: %w", err)
		}
	}
	return nil
}
