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
//     row's lifecycle_state is recorded as TRASHED, with the trash time in
//     node_trash_times. A 30-day TTL pruner (internal/prune's TrashWorker,
//     aging copies by that recorded time) is responsible for the final
//     unlink; until then, the file is restorable.
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

	"github.com/google/uuid"

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
// TRASHED, and (when keepExports is false) moves the files of linked Tier-2
// exports -- reviewed (AUTO_ACCEPTED or CONFIRMED) FINAL_EXPORT or
// immich_export edges only -- into .trash/ and marks their nodes TRASHED too. When keepExports is true, only the
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
	ml := &moveLog{}
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var innerErr error
		node, innerErr = trashAssetTx(ctx, q, guard, log, nodeID, keepExports, ml)
		return innerErr
	})
	if err != nil {
		// The tx rolled back (a later step failed, or the commit itself
		// did): put every moved file back so DB and disk agree again.
		ml.undo(guard, log)
	}
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
	// Without a caller-supplied MoveLog (WithMoveLog) this owns its own and
	// undoes it when ITS steps fail. With one, the caller undoes it when its
	// enclosing transaction fails -- including AFTER this returned nil (a
	// later step or the commit failing), which this cannot see.
	// TrashAsset does NOT go through here -- it calls trashAssetTx directly.
	ml, owned := moveLogFrom(ctx)
	node, err := trashAssetTx(ctx, q, guard, log, nodeID, keepExports, ml)
	if err != nil && owned {
		ml.undo(guard, log)
	}
	return node, err
}

func trashAssetTx(
	ctx context.Context,
	q *sqlcgen.Queries,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
	keepExports bool,
	ml *moveLog,
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
			if err := moveToTrashLogged(guard, loc.RootPath, loaded.FilePath, loaded.NodeUuid, log, ml); err != nil {
				return sqlcgen.MediaNode{}, fmt.Errorf("move to trash: %w", err)
			}
		}
	}

	if !keepExports {
		if err := purgeLinkedExports(ctx, q, guard, log, loaded.ID, ml); err != nil {
			return sqlcgen.MediaNode{}, fmt.Errorf("purge linked exports: %w", err)
		}
	}

	if err := markTrashed(ctx, q, loaded.ID); err != nil {
		return sqlcgen.MediaNode{}, err
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
// path, sets lifecycle_state back to ACTIVE, and restores the linked exports
// that this node's own keepExports=false TrashAsset call cascaded into the
// trash (recorded in trash_cascades). Exports trashed independently stay
// trashed.
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
	// A rolled-back tx (a later DB step, or the commit) must also put the
	// restored file back into .trash/, or the row stays TRASHED with its
	// bytes at the original path.
	ml := &moveLog{}
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var innerErr error
		node, innerErr = restoreTrashedAssetTx(ctx, q, guard, log, nodeID, ml)
		return innerErr
	})
	if err != nil {
		ml.undo(guard, log)
	}
	return node, err
}

// RestoreTrashedAssetTx is the inner transactional body of RestoreTrashedAsset.
// See TrashAssetTx's doc comment for why this exists.
//
// A caller that attaches a MoveLog via WithMoveLog owns undoing it when ITS
// enclosing transaction fails; otherwise this undoes its own moves on error.
func RestoreTrashedAssetTx(
	ctx context.Context,
	q *sqlcgen.Queries,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
) (sqlcgen.MediaNode, error) {
	ml, owned := moveLogFrom(ctx)
	node, err := restoreTrashedAssetTx(ctx, q, guard, log, nodeID, ml)
	if err != nil && owned {
		ml.undo(guard, log)
	}
	return node, err
}

func restoreTrashedAssetTx(
	ctx context.Context,
	q *sqlcgen.Queries,
	guard *storage.Guard,
	log *slog.Logger,
	nodeID int64,
	ml *moveLog,
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
		if err := restoreFromTrash(guard, loaded.FilePath, loaded.NodeUuid, log, ml); err != nil {
			return sqlcgen.MediaNode{}, err
		}
	}

	if err := markUntrashed(ctx, q, loaded.ID); err != nil {
		return sqlcgen.MediaNode{}, err
	}
	// If this node was itself cascade-trashed as some master's export, it
	// is back now: close that cascade so the master's later restore cannot
	// resurrect a subsequent, independent trash of this node.
	if err := q.CloseTrashCascadesForExport(ctx, loaded.ID); err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("close trash cascade: %w", err)
	}

	if err := restoreLinkedExports(ctx, q, guard, log, loaded.ID, ml); err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("restore linked exports: %w", err)
	}

	updated, err := q.GetMediaNodeByID(ctx, loaded.ID)
	if err != nil {
		return sqlcgen.MediaNode{}, fmt.Errorf("reload node %d: %w", loaded.ID, err)
	}
	return updated, nil
}

// markTrashed flips nodeID to TRASHED and records when, in the caller's
// transaction. node_trash_times.trashed_at -- not the .trash/ copy's mtime
// -- is what the purge worker ages the copy by (see TrashTimeLookup).
func markTrashed(ctx context.Context, q *sqlcgen.Queries, nodeID int64) error {
	if err := q.MarkNodeTrashed(ctx, nodeID); err != nil {
		return fmt.Errorf("mark node trashed: %w", err)
	}
	if err := q.RecordNodeTrashed(ctx, nodeID); err != nil {
		return fmt.Errorf("record trash time: %w", err)
	}
	return nil
}

// markUntrashed is markTrashed's inverse: ACTIVE again, trash time cleared.
func markUntrashed(ctx context.Context, q *sqlcgen.Queries, nodeID int64) error {
	if err := q.UntrashNode(ctx, nodeID); err != nil {
		return fmt.Errorf("untrash node: %w", err)
	}
	if err := q.ClearNodeTrashed(ctx, nodeID); err != nil {
		return fmt.Errorf("clear trash time: %w", err)
	}
	return nil
}

// TrashTimeLookup returns the purge worker's view of the DB
// (prune.TrashTimeFunc): when the .trash/ copy at path, inside the
// storage location rooted at locRoot, was trashed. The owning node is
// found by the full node_uuid moveToTrash embeds in the copy's name and
// must be TRASHED now, have a recorded trash time, and map back to exactly
// this path via trashPathFor. Anything else -- legacy 8-char names, a
// restored or rebased node, a file nobody owns -- reports known=false and
// the worker falls back to the file's mtime. Each such fallback is logged
// at DEBUG (path, the node_uuid parsed from the name if any, and why), so a
// copy purged by mtime instead of by its recorded trash time can be traced.
// A nil log discards.
func TrashTimeLookup(database *db.DB, log *slog.Logger) func(ctx context.Context, locRoot, path string) (time.Time, bool, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, locRoot, path string) (time.Time, bool, error) {
		unknown := func(nodeUUID, reason string) (time.Time, bool, error) {
			log.DebugContext(ctx, "trash purge: no recorded trash time for .trash/ copy, aging it by mtime",
				"path", path, "nodeUuid", nodeUUID, "reason", reason)
			return time.Time{}, false, nil
		}
		nodeUUID, ok := nodeUUIDFromTrashName(path)
		if !ok {
			return unknown("", "no node_uuid in file name")
		}
		row, err := database.Reader.GetTrashedNodeTrashTime(ctx, nodeUUID)
		if errors.Is(err, sql.ErrNoRows) {
			return unknown(nodeUUID, "no TRASHED node with a recorded trash time")
		}
		if err != nil {
			return time.Time{}, false, fmt.Errorf("look up trash time for %s: %w", nodeUUID, err)
		}
		rel, err := filepath.Rel(locRoot, row.FilePath)
		if err != nil || !filepath.IsLocal(rel) || trashPathFor(locRoot, rel, nodeUUID) != filepath.Clean(path) {
			return unknown(nodeUUID, "node's trash path is not this file")
		}
		return time.Unix(row.TrashedAt.Int64, 0).UTC(), true, nil
	}
}

// nodeUUIDFromTrashName extracts the node_uuid trashPathFor embeds in a
// .trash/ file name: <stem>.<uuid><ext>, or <name>.<uuid> for a file with
// no extension (whose uuid then reads as the extension).
func nodeUUIDFromTrashName(path string) (string, bool) {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	if id := strings.TrimPrefix(ext, "."); isUUID(id) {
		return id, true
	}
	stem := strings.TrimSuffix(base, ext)
	const n = 36 // canonical UUID length
	if len(stem) <= n || stem[len(stem)-n-1] != '.' {
		return "", false
	}
	if id := stem[len(stem)-n:]; isUUID(id) {
		return id, true
	}
	return "", false
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// trashPathFor returns the .trash/ location for a node's copy of relPath:
// <root>/.trash/<stem>.<nodeUUID><ext>. The FULL uuid is embedded: the first
// 8 hex chars of a UUIDv7 are the top of its millisecond timestamp (a ~65s
// bucket), so two nodes trashing the same rel_path within that window used
// to get the same name and the second os.Rename silently clobbered the first.
func trashPathFor(rootPath, relPath, nodeUUID string) string {
	ext := filepath.Ext(relPath)
	return filepath.Join(rootPath, ".trash", strings.TrimSuffix(relPath, ext)+"."+nodeUUID+ext)
}

// legacyTrashPathFor is the pre-fix 8-char-suffix name. Files already in
// .trash/ were written with it, so restore still honours it as a fallback.
func legacyTrashPathFor(rootPath, relPath, nodeUUID string) string {
	short := nodeUUID
	if len(short) > 8 {
		short = short[:8]
	}
	ext := filepath.Ext(relPath)
	return filepath.Join(rootPath, ".trash", strings.TrimSuffix(relPath, ext)+"."+short+ext)
}

// MoveLog is the exported handle for a caller that runs TrashAssetTx /
// RestoreTrashedAssetTx inside its own transaction and must undo the file
// moves if that transaction rolls back.
type MoveLog = moveLog

type moveLogKey struct{}

// WithMoveLog returns ctx carrying a fresh MoveLog. Pass it to TrashAssetTx /
// RestoreTrashedAssetTx, and call Undo on the returned log if the enclosing
// transaction fails.
func WithMoveLog(ctx context.Context) (context.Context, *MoveLog) {
	ml := &moveLog{}
	return context.WithValue(ctx, moveLogKey{}, ml), ml
}

// Undo renames every recorded move back (newest first, best effort).
func (m *moveLog) Undo(guard *storage.Guard, log *slog.Logger) { m.undo(guard, log) }

// moveLogFrom returns the caller-supplied log, or a fresh one this call owns
// (owned == true => this call is responsible for undoing it on failure).
func moveLogFrom(ctx context.Context) (ml *moveLog, owned bool) {
	if v, ok := ctx.Value(moveLogKey{}).(*moveLog); ok && v != nil {
		return v, false
	}
	return &moveLog{}, true
}

// moveLog records the file moves a trash operation has made so they can be
// undone if the surrounding DB transaction fails: the moves are real
// filesystem renames, but a rollback only reverts rows. Without this a
// failed trash left the node ACTIVE in the DB with its file sitting in
// .trash/ (later marked MISSING by the sweeper).
type moveLog struct {
	moves []trashMove
}

type trashMove struct{ from, to string }

func (m *moveLog) record(from, to string) {
	if m != nil {
		m.moves = append(m.moves, trashMove{from: from, to: to})
	}
}

// undo renames every recorded move back, newest first, best effort.
func (m *moveLog) undo(guard *storage.Guard, log *slog.Logger) {
	if m == nil {
		return
	}
	for i := len(m.moves) - 1; i >= 0; i-- {
		mv := m.moves[i]
		if err := guard.Rename(mv.to, mv.from); err != nil {
			log.Error("trash: failed to undo file move after rollback; file remains in .trash/",
				"from", mv.to, "to", mv.from, "err", err)
		}
	}
	m.moves = nil
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
	return moveToTrashLogged(guard, rootPath, filePath, nodeUUID, log, nil)
}

// moveToTrashLogged is moveToTrash that also records the move in ml (may be
// nil) so a failed transaction can put the file back.
func moveToTrashLogged(guard *storage.Guard, rootPath, filePath, nodeUUID string, log *slog.Logger, ml *moveLog) error {
	relPath, err := filepath.Rel(rootPath, filePath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return fmt.Errorf("compute rel path for %s under %s: %w", filePath, rootPath, err)
	}
	if nodeUUID == "" {
		return fmt.Errorf("moveToTrash requires a non-empty nodeUUID for filename-uniqueness")
	}
	// Embed the node_uuid in the trash filename so the same rel_path can
	// be trashed by N nodes with N distinct, recoverable copies.
	trashPath := trashPathFor(rootPath, relPath, nodeUUID)
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
	if err := guard.Rename(filePath, trashPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", filePath, trashPath, err)
	}
	ml.record(filePath, trashPath)
	// The retention clock is node_trash_times.trashed_at (markTrashed),
	// not this mtime. Stamping it still keeps the purge's mtime fallback
	// -- for copies no TRASHED node owns -- counting from the move, so a
	// failure here is only worth a warning.
	now := time.Now().UTC()
	if chErr := guard.Chtimes(trashPath, now, now); chErr != nil {
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
func restoreFromTrash(guard *storage.Guard, filePath, nodeUUID string, log *slog.Logger, ml *moveLog) error {
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

	trashPath := trashPathFor(loc.RootPath, relPath, nodeUUID)
	if _, statErr := os.Stat(trashPath); os.IsNotExist(statErr) {
		// Copies trashed before the full-uuid naming change.
		if legacy := legacyTrashPathFor(loc.RootPath, relPath, nodeUUID); legacy != trashPath {
			if _, lerr := os.Stat(legacy); lerr == nil {
				trashPath = legacy
			}
		}
	}

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
		if err := guard.Rename(trashPath, filePath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", trashPath, filePath, err)
		}
		ml.record(trashPath, filePath)
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

// isCascadeExportEdge reports whether a master's trash may cascade over edge:
// a FINAL_EXPORT or immich_export edge that is AUTO_ACCEPTED or CONFIRMED.
// NEEDS_REVIEW is an unreviewed guess and REJECTED is a human saying "not
// linked"; trashing a file the user never agreed belongs to the master is
// data loss, so both are skipped. ListEdgesBySource already drops inactive
// edges.
func isCascadeExportEdge(edge sqlcgen.MediaEdge) bool {
	if edge.RelationshipType != "FINAL_EXPORT" && edge.Resolver != "immich_export" {
		return false
	}
	return edge.ReviewState == "AUTO_ACCEPTED" || edge.ReviewState == "CONFIRMED"
}

// purgeLinkedExports moves the files for every Tier-2 export node linked to
// the master via a reviewed FINAL_EXPORT or immich_export edge (see
// isCascadeExportEdge) into .trash/ and marks those nodes TRASHED. The caller
// runs MarkNodeTrashed on the master AFTER this returns. Each export trashed
// here is recorded in trash_cascades against the master, so restoring the
// master brings back exactly these exports (restoreLinkedExports) and not
// ones the user trashed on their own.
//
// Note: this runs inside the same transaction as the master's MarkNodeTrashed
// call. If a linked export's file is in a read-only tier (Immich's exports
// can be on TIER2_EXPORTS which is read-write per the storage policy), the
// guard refuses the Remove and the transaction rolls back.
func purgeLinkedExports(ctx context.Context, q *sqlcgen.Queries, guard *storage.Guard, log *slog.Logger, parentID int64, ml *moveLog) error {
	edges, err := q.ListEdgesBySource(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list edges by source: %w", err)
	}
	for _, edge := range edges {
		if !isCascadeExportEdge(edge) {
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
			if err := moveToTrashLogged(guard, expLoc.RootPath, exp.FilePath, exp.NodeUuid, log, ml); err != nil {
				return fmt.Errorf("move export to trash: %w", err)
			}
		}
		if err := markTrashed(ctx, q, exp.ID); err != nil {
			return fmt.Errorf("export: %w", err)
		}
		if err := q.RecordTrashCascade(ctx, sqlcgen.RecordTrashCascadeParams{ParentNodeID: parentID, ExportNodeID: exp.ID}); err != nil {
			return fmt.Errorf("record trash cascade: %w", err)
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
		if !isCascadeExportEdge(edge) {
			continue
		}
		if err := q.DeleteRemoteSyncStateForNode(ctx, edge.TargetNodeID); err != nil {
			return err
		}
	}
	return nil
}

// restoreLinkedExports restores the exports this master's own trash cascaded
// into .trash/ (the open trash_cascades rows purgeLinkedExports recorded),
// moving their files back from .trash/<rel> to the export's original path.
// An export the user trashed on its own -- before or after the master -- has
// no such row and stays in the trash.
//
// Tolerates missing .trash/ copies (TTL pruner may have already purged them):
// the export's lifecycle_state still flips back to ACTIVE so the master's
// restore succeeds as a whole, and a warning is logged. The user can
// re-export if needed.
func restoreLinkedExports(ctx context.Context, q *sqlcgen.Queries, guard *storage.Guard, log *slog.Logger, parentID int64, ml *moveLog) error {
	exportIDs, err := q.ListOpenTrashCascadeExports(ctx, parentID)
	if err != nil {
		return fmt.Errorf("list trash cascades: %w", err)
	}
	for _, expID := range exportIDs {
		exp, err := q.GetMediaNodeByID(ctx, expID)
		if err != nil {
			log.Warn("load export node", "nodeID", expID, "err", err)
			continue
		}
		// Whatever state the export is in now, this cascade is spent: a
		// later, independent trash of the export must not be undone by
		// this master's next restore.
		if err := q.CloseTrashCascadesForExport(ctx, exp.ID); err != nil {
			return fmt.Errorf("close trash cascade for export %d: %w", exp.ID, err)
		}
		if exp.LifecycleState != "TRASHED" {
			continue
		}
		if guard != nil && exp.FilePath != "" {
			if err := restoreFromTrash(guard, exp.FilePath, exp.NodeUuid, log, ml); err != nil {
				if errors.Is(err, ErrAssetTrashFileMissing) {
					log.Warn("trash copy for export is missing (TTL expired or never moved); export restored logically only -- re-export may be needed",
						"exportID", exp.ID, "filePath", exp.FilePath)
				} else {
					return fmt.Errorf("restore export %s: %w", exp.FilePath, err)
				}
			}
		}
		if err := markUntrashed(ctx, q, exp.ID); err != nil {
			return fmt.Errorf("export: %w", err)
		}
	}
	return nil
}
