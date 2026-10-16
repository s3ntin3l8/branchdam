-- name: RecordTrashCascade :exec
-- pipeline.TrashAsset records each export it cascade-trashes with its master
-- (keepExports=false), in the same transaction as the export's
-- MarkNodeTrashed.
INSERT INTO trash_cascades (parent_node_id, export_node_id)
VALUES (?1, ?2);

-- name: ListOpenTrashCascadeExports :many
-- Exports cascade-trashed with master ?1 that have not been restored since.
-- pipeline.RestoreTrashedAsset restores only these, so an export the user
-- trashed on its own stays in the trash when its master comes back.
SELECT DISTINCT export_node_id
FROM trash_cascades
WHERE parent_node_id = ?1 AND restored_at IS NULL
ORDER BY export_node_id;

-- name: CloseTrashCascadesForExport :exec
-- Marks every open cascade row for export ?1 restored. Called whenever the
-- export leaves TRASHED (restored with its master, or restored on its own),
-- so a stale row can never resurrect a later, independent trash of it.
UPDATE trash_cascades
SET restored_at = unixepoch()
WHERE export_node_id = ?1 AND restored_at IS NULL;
