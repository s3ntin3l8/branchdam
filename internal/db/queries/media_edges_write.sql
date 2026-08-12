-- name: CreateMediaEdge :one
-- Minimal edge insert, landed here because PR 6's own version-collision test
-- (T5, spec 9.5) needs to prove an existing edge survives archiving its
-- source node (docs/schema.md fix #6: RESTRICT, never CASCADE). Full edge
-- resolution -- confidence scoring, cycle detection, upsert-without-
-- downgrading-review_state -- is internal/graph's job (PR 7), built on top
-- of this.
INSERT INTO media_edges (
    source_node_id, target_node_id, relationship_type, confidence, tier,
    resolver, evidence_json, review_state
) VALUES (
    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8
)
RETURNING id, source_node_id, target_node_id, relationship_type, confidence,
          tier, resolver, evidence_json, review_state, reviewed_at, reviewed_by,
          created_at, updated_at;

-- name: GetMediaEdge :one
SELECT id, source_node_id, target_node_id, relationship_type, confidence,
       tier, resolver, evidence_json, review_state, reviewed_at, reviewed_by,
       created_at, updated_at
FROM media_edges
WHERE id = ?1;

-- name: ListEdgesBySource :many
SELECT id, source_node_id, target_node_id, relationship_type, confidence,
       tier, resolver, evidence_json, review_state, reviewed_at, reviewed_by,
       created_at, updated_at
FROM media_edges
WHERE source_node_id = ?1;
