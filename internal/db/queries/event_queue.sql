-- name: EnqueueAgentEvent :one
-- Backs POST /api/v1/agent/events: persists and returns 202 in increment 1.
-- Actually draining/processing these rows ships with the deferred
-- workstation-agent increment -- this table and endpoint exist now so that
-- increment is additive, not a schema migration.
INSERT INTO event_queue (event_uuid, agent_id, event_type, payload_json, status)
VALUES (?1, ?2, ?3, ?4, 'PENDING')
RETURNING id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at;

-- name: ListPendingAgentEvents :many
-- Claims only rows whose retry backoff has elapsed (next_attempt_at <= now,
-- unix seconds from the drainer's clock). A row deferred by
-- ScheduleAgentEventRetry stays PENDING but is skipped until then, so it
-- neither blocks the queue head nor burns its retry budget in seconds.
SELECT id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at
FROM event_queue
WHERE status = 'PENDING' AND next_attempt_at <= sqlc.arg(now_unix)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(batch_limit);

-- name: MarkAgentEventProcessed :exec
UPDATE event_queue
SET status = 'PROCESSED', processed_at = unixepoch(), error_log = NULL
WHERE id = ?1;

-- name: MarkAgentEventFailed :exec
UPDATE event_queue
SET status = 'FAILED', processed_at = unixepoch(), error_log = ?2
WHERE id = ?1;

-- name: ScheduleAgentEventRetry :exec
-- Records one more failed attempt and defers the row until next_attempt_at
-- (unix seconds), computed by the drainer's exponential backoff.
UPDATE event_queue
SET retry_count = retry_count + 1, error_log = ?2, next_attempt_at = ?3
WHERE id = ?1;

-- name: GetAgentEventByUUID :one
SELECT id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at, next_attempt_at
FROM event_queue
WHERE event_uuid = ?1;

-- name: GetAgentEventByUUIDAndAgent :one
SELECT id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at
FROM event_queue
WHERE event_uuid = ?1 AND agent_id = ?2;

-- name: GetLatestProcessedAgentEventByAgent :one
SELECT id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at
FROM event_queue
WHERE agent_id = ?1 AND status = 'PROCESSED'
ORDER BY processed_at DESC, id DESC
LIMIT 1;

-- name: CountPendingAgentEvents :one
SELECT COUNT(*)
FROM event_queue
WHERE status = 'PENDING';

-- name: CountDueAgentEvents :one
-- PENDING rows ListPendingAgentEvents would claim right now. DrainAll stops
-- once this reaches 0 even if deferred rows remain PENDING.
SELECT COUNT(*)
FROM event_queue
WHERE status = 'PENDING' AND next_attempt_at <= sqlc.arg(now_unix);
