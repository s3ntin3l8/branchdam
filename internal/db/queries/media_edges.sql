-- name: WouldCreateCycle :one
-- Walk the proposed child's descendants; if the proposed PARENT is already a
-- descendant of the proposed CHILD, the new edge would close a cycle. Used
-- by internal/graph (PR 7) inside the same write transaction as the edge
-- insert -- DB.InTx's single writer connection is what makes this
-- check-then-insert sound without extra application-level locking.
--
-- The bare-parameter anchor `SELECT sqlc.arg(...)` needed an explicit alias
-- to satisfy sqlc's SQLite parser -- see docs/schema.md's sqlc risk note.
WITH RECURSIVE descendants(id) AS (
    SELECT sqlc.arg(child_node_id) AS id
    UNION
    SELECT e.target_node_id
    FROM media_edges e
    JOIN descendants d ON e.source_node_id = d.id
)
SELECT EXISTS(SELECT 1 FROM descendants WHERE id = sqlc.arg(parent_node_id)) AS would_cycle;

-- name: ListAuditQueue :many
-- The audit queue (spec §7) is this query over review_state, not a second
-- table. v_media_edges_resolved's parent_missing works for every
-- relationship_type -- the spec's deleted trigger never did. See
-- docs/schema.md fix #4.
SELECT id, source_node_id, target_node_id, relationship_type, confidence,
       resolver, evidence_json, parent_alive, parent_missing
FROM v_media_edges_resolved
WHERE review_state = 'NEEDS_REVIEW'
ORDER BY confidence DESC, id ASC
LIMIT ?1 OFFSET ?2;
