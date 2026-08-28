-- name: ListAgentScratchTelemetry :many
SELECT *
FROM agent_scratch_telemetry
ORDER BY agent_id ASC;

-- name: GetAgentScratchTelemetry :one
SELECT *
FROM agent_scratch_telemetry
WHERE agent_id = ?1;

-- name: UpsertAgentScratchTelemetry :one
INSERT INTO agent_scratch_telemetry (
    agent_id,
    client_version,
    timestamp_unix,
    mount_path,
    total_bytes,
    free_bytes,
    used_bytes,
    mirrors_size_bytes,
    render_cache_size_bytes,
    proxies_size_bytes,
    prunable_bytes,
    last_prune_timestamp_unix,
    last_reclaimed_bytes,
    last_prune_duration_ms,
    pruned_item_counts,
    updated_at
) VALUES (
    ?1, ?2, ?3, ?4,
    ?5, ?6, ?7,
    ?8, ?9, ?10, ?11,
    ?12, ?13, ?14, ?15,
    unixepoch()
)
ON CONFLICT (agent_id) DO UPDATE SET
    client_version = excluded.client_version,
    timestamp_unix = excluded.timestamp_unix,
    mount_path = excluded.mount_path,
    total_bytes = excluded.total_bytes,
    free_bytes = excluded.free_bytes,
    used_bytes = excluded.used_bytes,
    mirrors_size_bytes = excluded.mirrors_size_bytes,
    render_cache_size_bytes = excluded.render_cache_size_bytes,
    proxies_size_bytes = excluded.proxies_size_bytes,
    prunable_bytes = excluded.prunable_bytes,
    last_prune_timestamp_unix = excluded.last_prune_timestamp_unix,
    last_reclaimed_bytes = excluded.last_reclaimed_bytes,
    last_prune_duration_ms = excluded.last_prune_duration_ms,
    pruned_item_counts = excluded.pruned_item_counts,
    updated_at = unixepoch()
RETURNING *;

-- name: DeleteAgentScratchTelemetry :exec
DELETE FROM agent_scratch_telemetry
WHERE agent_id = ?1;
