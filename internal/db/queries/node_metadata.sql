-- name: InsertNodeMetadata :exec
-- Phase 1 (#33): EXIF/ffprobe overflow. Upsert on the table's natural key so
-- a re-scan that re-derives metadata replaces rather than duplicates rows.
INSERT INTO node_metadata (node_id, source, key, value)
VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (node_id, source, key) DO UPDATE SET value = excluded.value;

-- name: ListNodeMetadata :many
-- Backs tests and any future metadata inspector UI.
SELECT node_id, source, key, value
FROM node_metadata
WHERE node_id = ?1
ORDER BY source, key;

-- name: PruneArchivedNodeMetadata :execrows
-- Phase 1 (#89): remove node_metadata rows whose owning media_nodes row is
-- ARCHIVED. ARCHIVED nodes are superseded versions that no longer participate
-- in the live graph; their metadata is write-once historical data that grows
-- monotonically with editing activity. Pruning these rows bounds table size
-- without deleting media_nodes rows themselves (the "rows are never deleted"
-- invariant for media_nodes stands).
DELETE FROM node_metadata
WHERE node_id IN (
    SELECT id FROM media_nodes WHERE lifecycle_state = 'ARCHIVED'
);
