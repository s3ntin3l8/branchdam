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