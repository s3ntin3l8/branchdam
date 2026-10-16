-- name: InsertNodeUUIDAlias :exec
-- Records that a device's node_uuid was content-deduplicated onto an
-- existing node (see migration 00038). First writer wins: an alias never
-- changes target once recorded.
INSERT INTO node_uuid_aliases (alias_uuid, node_uuid, agent_id)
VALUES (?1, ?2, ?3)
ON CONFLICT (alias_uuid) DO NOTHING;

-- name: GetNodeUUIDAlias :one
SELECT alias_uuid, node_uuid, agent_id, created_at
FROM node_uuid_aliases
WHERE alias_uuid = ?1;

-- name: ListNodeUUIDAliases :many
-- Backs POST /api/v1/agent/node-status: batch alias lookup for the UUIDs
-- that matched no media_nodes row directly.
SELECT alias_uuid, node_uuid, agent_id
FROM node_uuid_aliases
WHERE alias_uuid IN (SELECT value FROM json_each(CAST(sqlc.arg(alias_uuids) AS TEXT)));
