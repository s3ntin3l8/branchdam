-- name: RegisterResolveTimelineScope :exec
INSERT OR IGNORE INTO resolve_timeline_scopes (agent_id, scope_id, timeline_node_id)
VALUES (?1, ?2, ?3);

-- name: ListResolveTimelineScopes :many
SELECT timeline_node_id FROM resolve_timeline_scopes
WHERE agent_id = ?1 AND scope_id = ?2;

-- name: ListResolveEdgesForTimeline :many
SELECT id, source_node_id, target_node_id, resolver, evidence_json,
       review_state, is_active
FROM media_edges
WHERE target_node_id = ?1 AND relationship_type = 'PROJECT_SIDECAR';

-- name: RefreshResolveEdge :exec
UPDATE media_edges
SET evidence_json = ?2, is_active = 1, updated_at = unixepoch()
WHERE id = ?1 AND resolver = 'resolve_project_db'
  AND review_state NOT IN ('CONFIRMED', 'REJECTED');

-- name: InactivateResolveEdge :exec
UPDATE media_edges
SET is_active = 0, updated_at = unixepoch()
WHERE id = ?1 AND resolver = 'resolve_project_db'
  AND review_state NOT IN ('CONFIRMED', 'REJECTED');

-- name: UpdateResolveTimelineDisplayName :exec
UPDATE media_nodes SET file_name = ?2, updated_at = unixepoch()
WHERE id = ?1;

-- name: AgentCreatedVirtualNode :one
SELECT EXISTS(
    SELECT 1 FROM event_queue
    WHERE agent_id = ?1 AND event_type = 'EVENT_VIRTUAL_NODE_CREATED'
      AND status = 'PROCESSED'
      AND json_extract(payload_json, '$.nodeUuid') = sqlc.arg(node_uuid)
) AS created_by_agent;

-- name: ResolveTimelineScopeOwner :one
SELECT EXISTS(
    SELECT 1 FROM resolve_timeline_scopes
    WHERE timeline_node_id = ?1 AND agent_id = ?2 AND scope_id = ?3
) AS scope_owner;
