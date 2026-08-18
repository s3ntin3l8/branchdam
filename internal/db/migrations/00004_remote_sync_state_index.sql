-- +goose Up
-- #165: every hot query against remote_sync_state (internal/db/queries/
-- remote_sync_state.sql) filters on remote + sync_status -- the sync
-- worker's claim query (ListRemoteSyncStateByStatus, ORDER BY
-- last_attempt_at ASC, node_id ASC) and both crash-recovery re-claim
-- queries (ResetRemoteSyncStateStale, ResetRemoteSyncStateFailed, both
-- filtering last_attempt_at as a range on top of remote + sync_status) --
-- but the only index on this table is the (node_id, remote) PRIMARY KEY,
-- which doesn't lead with remote or sync_status. Every one of those
-- queries was a full table scan (plus a filesort for the ORDER BY) with no
-- index to use.
--
-- node_id is included as the trailing column (not just remote, sync_status,
-- last_attempt_at) so the index also satisfies ListRemoteSyncStateByStatus's
-- full ORDER BY, including its node_id tiebreaker: last_attempt_at is a
-- second-granularity unixepoch() column, so ties within one worker poll's
-- claim batch are the normal case, not an edge case, and without node_id
-- trailing, SQLite still needs a small in-memory sort to break those ties
-- even though the range/LIMIT itself is already satisfied by the index.
-- Verified via EXPLAIN QUERY PLAN against both an empty table and a
-- 200k-row ANALYZE'd table with realistic tie density.
--
-- Named ix_remote_sync_state_remote_status, not _pending: it also serves
-- the two Reset* queries' PUSHING/PUSH_FAILED lookups, not just the
-- PENDING_CLOUD_PUSH claim query.
CREATE INDEX ix_remote_sync_state_remote_status
    ON remote_sync_state(remote, sync_status, last_attempt_at, node_id);

-- +goose Down
DROP INDEX IF EXISTS ix_remote_sync_state_remote_status;
