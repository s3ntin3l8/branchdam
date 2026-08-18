-- name: EnqueueAgentEvent :one
-- Backs POST /api/v1/agent/events: persists and returns 202 in increment 1.
-- Actually draining/processing these rows ships with the deferred
-- workstation-agent increment -- this table and endpoint exist now so that
-- increment is additive, not a schema migration.
INSERT INTO event_queue (event_uuid, agent_id, event_type, payload_json, status)
VALUES (?1, ?2, ?3, ?4, 'PENDING')
RETURNING id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at;

-- name: ListPendingAgentEvents :many
SELECT id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at
FROM event_queue
WHERE status = 'PENDING'
ORDER BY created_at ASC, id ASC
LIMIT ?1;

-- name: MarkAgentEventProcessed :exec
UPDATE event_queue
SET status = 'PROCESSED', processed_at = unixepoch(), error_log = NULL
WHERE id = ?1;

-- name: MarkAgentEventFailed :exec
UPDATE event_queue
SET status = 'FAILED', processed_at = unixepoch(), error_log = ?2
WHERE id = ?1;

-- name: IncrementAgentEventRetry :exec
UPDATE event_queue
SET retry_count = retry_count + 1, error_log = ?2
WHERE id = ?1;

-- name: GetAgentEventByUUID :one
SELECT id, event_uuid, agent_id, event_type, payload_json, status, retry_count, error_log, created_at, processed_at
FROM event_queue
WHERE event_uuid = ?1;

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
