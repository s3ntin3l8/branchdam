-- +goose Up
-- An assistant confirm is a weaker signal than a human one. review_state
-- stays CONFIRMED either way (its CHECK is fixed in 00001). review_source
-- records who decided: human from the REST routes, assistant from MCP.
-- NULL is a resolver row (or a decision that was reset).
ALTER TABLE media_edges ADD COLUMN review_source TEXT CHECK (review_source IN ('human','assistant'));

DROP VIEW IF EXISTS v_media_edges_resolved;
CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by, e.review_source,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING') AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id
WHERE e.is_active = 1;

-- +goose Down
DROP VIEW IF EXISTS v_media_edges_resolved;
ALTER TABLE media_edges DROP COLUMN review_source;

-- The 00032 view, without review_source.
CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING') AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id
WHERE e.is_active = 1;
