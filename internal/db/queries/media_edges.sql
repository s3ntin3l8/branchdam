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

-- name: ListAncestors :many
-- Recursively list ancestor node IDs.
-- Anchor SELECT explicitly names/aliases all columns to satisfy sqlc's SQLite parser.
WITH RECURSIVE ancestors(id) AS (
    SELECT ?1 AS id
    UNION
    SELECT e.source_node_id
    FROM media_edges e
    JOIN ancestors a ON e.target_node_id = a.id
    JOIN media_nodes n ON e.source_node_id = n.id
    WHERE e.review_state <> 'REJECTED'
      AND n.lifecycle_state <> 'ARCHIVED'
)
SELECT ancestors.id FROM ancestors;

-- name: ListDescendants :many
-- Recursively list descendant node IDs.
-- Anchor SELECT explicitly names/aliases all columns to satisfy sqlc's SQLite parser.
WITH RECURSIVE descendants(id) AS (
    SELECT ?1 AS id
    UNION
    SELECT e.target_node_id
    FROM media_edges e
    JOIN descendants d ON e.source_node_id = d.id
    JOIN media_nodes n ON e.target_node_id = n.id
    WHERE e.review_state <> 'REJECTED'
      AND n.lifecycle_state <> 'ARCHIVED'
)
SELECT descendants.id FROM descendants;







-- name: ListNodesByIDs :many
SELECT id, node_uuid, storage_location_id, file_path, file_name, file_ext,
       size_bytes, mtime_unix, fast_hash, full_hash, phash,
       indexing_status, graph_status, lifecycle_state, superseded_by,
       original_document_id, document_id, derived_from_id,
       captured_at_unix, camera_model, filename_stem,
       first_seen_at, last_seen_at, created_at, updated_at,
       camera_serial, lens_model
FROM media_nodes
WHERE id IN (SELECT value FROM json_each(?1))
  AND lifecycle_state <> 'ARCHIVED';

-- name: ListEdgesForNodes :many
SELECT id, source_node_id, target_node_id, relationship_type, confidence,
       review_state, resolver
FROM media_edges
WHERE source_node_id IN (SELECT value FROM json_each(?1))
  AND target_node_id IN (SELECT value FROM json_each(?1))
  AND review_state <> 'REJECTED';
