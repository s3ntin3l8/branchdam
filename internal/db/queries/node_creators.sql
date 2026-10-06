-- name: SetNodeCreator :exec
-- Records the paired device that created a node through the agent API.
-- First writer wins: a node's creator never changes.
INSERT INTO node_creators (node_uuid, agent_id)
VALUES (?1, ?2)
ON CONFLICT (node_uuid) DO NOTHING;

-- name: GetNodeCreator :one
SELECT agent_id FROM node_creators WHERE node_uuid = ?1;
