-- name: EnqueueAgentEvent :one
-- Backs POST /api/v1/agent/events: persists and returns 202 in increment 1.
-- Actually draining/processing these rows ships with the deferred
-- workstation-agent increment -- this table and endpoint exist now so that
-- increment is additive, not a schema migration.
INSERT INTO event_queue (event_uuid, agent_id, event_type, payload_json, status)
VALUES (?1, ?2, ?3, ?4, 'PENDING')
RETURNING id, event_uuid, agent_id, event_type, payload_json, status, error_log, created_at, processed_at;
