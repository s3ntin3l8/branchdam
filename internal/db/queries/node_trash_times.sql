-- node_trash_times: when each node was trashed (migration 00040). The
-- trash purge worker ages .trash/ copies by this value instead of by file
-- mtime, which a failed os.Chtimes in pipeline.moveToTrash left at the
-- file's original (old) time.

-- name: RecordNodeTrashed :exec
-- pipeline.trashAssetTx calls this right after MarkNodeTrashed, in the
-- same transaction, for the master and for every export the trash
-- cascades over. Upsert: a node restored and trashed again restarts its
-- retention window.
INSERT INTO node_trash_times (node_id, trashed_at)
VALUES (?1, unixepoch())
ON CONFLICT (node_id) DO UPDATE SET trashed_at = excluded.trashed_at;

-- name: ClearNodeTrashed :exec
-- pipeline.restoreTrashedAssetTx calls this next to UntrashNode. The row
-- stays (rows are never deleted); a NULL trashed_at means "not trashed".
UPDATE node_trash_times SET trashed_at = NULL WHERE node_id = ?1;

-- name: GetTrashedNodeTrashTime :one
-- Purge-time lookup for one .trash/ copy, keyed on the node_uuid embedded
-- in its file name. Only a node that is TRASHED right now and has a
-- recorded trash time matches; anything else is sql.ErrNoRows and the
-- purge falls back to the file's mtime.
SELECT n.id AS id, n.file_path AS file_path, t.trashed_at AS trashed_at
FROM media_nodes AS n
JOIN node_trash_times AS t ON t.node_id = n.id
WHERE n.node_uuid = ?1
  AND n.lifecycle_state = 'TRASHED'
  AND t.trashed_at IS NOT NULL;

-- name: GetNodeTrashTime :one
-- Read-back for tests and diagnostics.
SELECT node_id, trashed_at FROM node_trash_times WHERE node_id = ?1;
