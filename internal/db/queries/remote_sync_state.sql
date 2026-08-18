-- name: GetRemoteSyncState :one
-- The idempotency look-before-write read for Enqueue: does (node_id, remote)
-- already exist, and in what state?
SELECT node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at
FROM remote_sync_state
WHERE node_id = ?1 AND remote = ?2;

-- name: UpsertRemoteSyncState :one
-- Claims a node for push (or re-claims it): sets the target sync_status and
-- bumps last_attempt_at/updated_at. On a re-claim of an existing PUSH_FAILED
-- row, last_error is cleared and the existing remote_asset_id (if any) is
-- kept. Every transition sets updated_at explicitly -- no triggers, per this
-- repo's invariant. This is the ONLY way a (node_id, remote) row is created.
INSERT INTO remote_sync_state (node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, unixepoch())
ON CONFLICT (node_id, remote) DO UPDATE SET
    sync_status     = excluded.sync_status,
    remote_asset_id = COALESCE(excluded.remote_asset_id, remote_sync_state.remote_asset_id),
    last_error      = excluded.last_error,
    last_attempt_at = excluded.last_attempt_at,
    updated_at      = unixepoch()
RETURNING node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at;

-- name: ListRemoteSyncStateByStatus :many
-- The sync worker's claim query: oldest-attempt-first so a backlog drains in
-- order, capped at one batch. Scoped to a single remote -- a node can hold
-- both IMMICH and GOOGLE_PHOTOS rows under the (node_id, remote) PK, so
-- ProcessPending(remote) must never list (or re-flip) another remote's rows.
SELECT node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at
FROM remote_sync_state
WHERE remote = ?1 AND sync_status = ?2
ORDER BY last_attempt_at ASC, node_id ASC
LIMIT ?3;

-- name: MarkRemoteSyncStatePushed :exec
-- Terminal success: records the remote asset id (empty for a scan-trigger
-- push) and clears any prior error. updated_at set explicitly -- no trigger.
UPDATE remote_sync_state
SET sync_status = 'PUSHED', remote_asset_id = ?3, last_error = NULL,
    last_attempt_at = unixepoch(), updated_at = unixepoch()
WHERE node_id = ?1 AND remote = ?2;

-- name: MarkRemoteSyncStateFailed :exec
-- Terminal failure: records the error and the attempt time.
UPDATE remote_sync_state
SET sync_status = 'PUSH_FAILED', last_error = ?3,
    last_attempt_at = unixepoch(), updated_at = unixepoch()
WHERE node_id = ?1 AND remote = ?2;

-- name: ResetRemoteSyncStateStale :execrows
-- Crash recovery: rows left PUSHING by a process that died mid-push are reset
-- to PENDING_CLOUD_PUSH so the next worker pass re-claims them. Scoped to a
-- single remote so an IMMICH recovery can never touch GOOGLE_PHOTOS rows.
UPDATE remote_sync_state
SET sync_status = 'PENDING_CLOUD_PUSH', updated_at = unixepoch()
WHERE sync_status = 'PUSHING' AND remote = ?1 AND last_attempt_at < ?2;

-- name: ListLiveNodesForSync :many
-- #55: the sync worker's enqueue source -- live nodes under a path prefix
-- (the Immich export mount) that have NO remote_sync_state row for the given
-- remote yet. Once pushed, a row exists and the node drops out of this set.
SELECT n.id, n.file_path, n.file_name, n.file_ext, n.fast_hash, n.full_hash
FROM media_nodes n
LEFT JOIN remote_sync_state rs
       ON rs.node_id = n.id AND rs.remote = ?1
WHERE n.lifecycle_state != 'ARCHIVED'
  AND (n.file_path = ?2 OR n.file_path LIKE ?2 || '/%')
  AND rs.node_id IS NULL
ORDER BY n.id ASC
LIMIT ?3;
