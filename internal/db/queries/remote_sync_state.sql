-- name: GetRemoteSyncState :one
-- The idempotency look-before-write read for Enqueue: does (node_id, remote)
-- already exist, and in what state?
SELECT node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at, retry_count
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
RETURNING node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at, retry_count;

-- name: ListRemoteSyncStateByStatus :many
-- The sync worker's claim query: oldest-attempt-first so a backlog drains in
-- order, capped at one batch. Scoped to a single remote -- a node can hold
-- both IMMICH and GOOGLE_PHOTOS rows under the (node_id, remote) PK, so
-- ProcessPending(remote) must never list (or re-flip) another remote's rows.
SELECT node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at, retry_count
FROM remote_sync_state
WHERE remote = ?1 AND sync_status = ?2
ORDER BY last_attempt_at ASC, node_id ASC
LIMIT ?3;

-- name: MarkRemoteSyncStatePushed :exec
-- Terminal success: records the remote asset id (empty for a scan-trigger
-- push) and clears any prior error. retry_count resets to 0 -- it tracks
-- consecutive failures since the last success, not a lifetime total.
-- updated_at set explicitly -- no trigger.
--
-- remote_asset_id uses COALESCE(excluded, existing), matching
-- UpsertRemoteSyncState's pattern, not a bare overwrite: today's only
-- PushFunc (Immich's whole-library TriggerScan) never populates it, so ?3 is
-- always empty and this is currently unreachable in practice. It's still
-- fixed for symmetry -- a bare overwrite would silently NULL a previously
-- recorded id on any future call site that (unlike today's) sometimes pushes
-- without a fresh remote_asset_id in hand.
UPDATE remote_sync_state
SET sync_status = 'PUSHED', remote_asset_id = COALESCE(?3, remote_asset_id), last_error = NULL,
    retry_count = 0, last_attempt_at = unixepoch(), updated_at = unixepoch()
WHERE node_id = ?1 AND remote = ?2;

-- name: MarkRemoteSyncStateFailed :exec
-- Terminal failure: records the error, the attempt time, and increments
-- retry_count -- what ResetRemoteSyncStateFailed's bound is measured against.
UPDATE remote_sync_state
SET sync_status = 'PUSH_FAILED', last_error = ?3, retry_count = retry_count + 1,
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

-- name: ResetRemoteSyncStateFailed :execrows
-- #55/#182: worker-level retry. PUSH_FAILED rows whose last attempt is older
-- than the retry window AND whose retry_count hasn't yet reached the bound
-- are reset to PENDING_CLOUD_PUSH so the next worker pass re-attempts them --
-- a transient remote failure must not strand a batch forever. Once
-- retry_count reaches the bound, a row is left PUSH_FAILED permanently: a
-- misconfigured remote (e.g. an empty libraryId, see #182) must not retry
-- every 5 minutes forever. A human can still force a re-attempt via
-- POST /api/v1/assets/{id}/sync/retry (handleSyncRetry), which bypasses this
-- bound by design -- an explicit operator action, not an automatic loop.
-- Scoped to a single remote. last_attempt_at is set explicitly on every
-- attempt, so this only re-claims rows that have not been retried recently
-- (bounded retry frequency, not a hot loop).
UPDATE remote_sync_state
SET sync_status = 'PENDING_CLOUD_PUSH', updated_at = unixepoch()
WHERE sync_status = 'PUSH_FAILED' AND remote = ?1 AND last_attempt_at < ?2 AND retry_count < ?3;

-- name: CountRemoteSyncStateExhausted :one
-- Observability for #182's automatic-retry bound: how many PUSH_FAILED rows
-- for this remote have a retry_count at or past the bound, so
-- ResetRemoteSyncStateFailed will never re-claim them again on its own.
-- Backs Worker.drain's periodic "permanently abandoned" warning -- distinct
-- from ResetRemoteSyncStateFailed's `retry_count < ?3` claim filter (this is
-- its complement, `>=`), so a misconfigured remote's silent backlog is
-- visible in logs instead of only discoverable via a direct SQLite query.
SELECT count(*) FROM remote_sync_state
WHERE sync_status = 'PUSH_FAILED' AND remote = ?1 AND retry_count >= ?2;

-- name: ListRemoteSyncStateByNode :many
-- Backs GET /api/v1/assets/{id}/sync-status: every remote_sync_state row for
-- a node (both remotes), ordered by remote for a stable DTO.
SELECT node_id, remote, sync_status, remote_asset_id, last_error, last_attempt_at, created_at, updated_at, retry_count
FROM remote_sync_state
WHERE node_id = ?1
ORDER BY remote ASC;

-- name: ManualRetryRemoteSyncState :exec
-- Backs POST /api/v1/assets/{id}/sync/retry (handleSyncRetry, #156): an
-- explicit operator "try again" action on a PUSH_FAILED row, bypassing
-- ResetRemoteSyncStateFailed's retry_count bound by design (see that
-- query's comment). retry_count resets to 0 here -- the same rule
-- MarkRemoteSyncStatePushed applies on a real success -- so the operator's
-- action restores a full automatic-retry budget, not just one attempt
-- before immediately re-hitting the bound on the very next failure (#182).
--
-- last_attempt_at is deliberately left untouched, matching
-- ResetRemoteSyncStateFailed (the automatic recovery path for the same
-- PUSH_FAILED -> PENDING_CLOUD_PUSH transition), not UpsertRemoteSyncState.
-- ListRemoteSyncStateByStatus claims oldest-last_attempt_at-first, so
-- bumping it here would send a row an operator just explicitly asked to
-- retry to the BACK of the claim queue -- behind every row the automatic
-- recovery already reset. Leaving it alone keeps the row's original
-- position, so it claims promptly instead of behind a potentially large
-- backlog.
UPDATE remote_sync_state
SET sync_status = 'PENDING_CLOUD_PUSH', last_error = NULL, retry_count = 0,
    updated_at = unixepoch()
WHERE node_id = ?1 AND remote = ?2;

-- name: DeleteRemoteSyncStateForNode :exec
-- Deletes remote_sync_state records when an asset is deleted / unlinked.
DELETE FROM remote_sync_state
WHERE node_id = ?1;
