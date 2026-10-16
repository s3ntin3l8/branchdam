-- +goose Up
--
-- 1. node_trash_times: when each node was trashed.
--
-- The trash purge worker used to age .trash/ files by their mtime, which
-- pipeline.moveToTrash stamps with os.Chtimes after the rename. When that
-- Chtimes failed (logged, not fatal) the file kept its original mtime --
-- often months old -- and the next hourly purge unlinked it immediately,
-- with no retention window at all. The trash time now lives in the DB,
-- recorded in the same transaction that marks the node TRASHED, and the
-- purge reads it from here; mtime remains only the fallback for .trash/
-- files no TRASHED node owns.
--
-- A separate table (like node_creators) rather than a media_nodes column
-- keeps the many explicit-column media_nodes queries untouched.
-- trashed_at is NULL while the node is not trashed (a restore clears it,
-- a later re-trash sets it again); rows are never deleted.
CREATE TABLE node_trash_times (
    node_id    INTEGER PRIMARY KEY REFERENCES media_nodes(id) ON DELETE RESTRICT,
    trashed_at INTEGER
);

-- Backfill every node that is TRASHED today. updated_at is what
-- MarkNodeTrashed stamped in the trashing transaction, moments after
-- moveToTrash set the trash copy's mtime: it equals the mtime-based age
-- the purge used whenever that Chtimes succeeded, and it is the later,
-- safe value when the Chtimes failed (the mtime then being the file's
-- original one). Never earlier than the real trash time, so the backfill
-- can only delay a purge, never bring one forward.
INSERT INTO node_trash_times (node_id, trashed_at)
SELECT id, updated_at
FROM media_nodes
WHERE lifecycle_state = 'TRASHED';

-- 2. Uploaded files were stored INDEXED_SHALLOW although the upload
-- engine computes their BLAKE3 full hash in-stream. The scanner marks a
-- node INDEXED_FULL exactly when it has a full hash
-- (pipeline.IndexingStatusFor); bring existing rows in line with that.
-- Only the indexing label changes; hashes and updated_at are left alone.
UPDATE media_nodes
SET indexing_status = 'INDEXED_FULL'
WHERE indexing_status = 'INDEXED_SHALLOW'
  AND full_hash IS NOT NULL
  AND full_hash <> '';

-- +goose Down
-- The indexing_status correction is not reverted: INDEXED_FULL with a
-- full hash is the state the scanner would have produced.
DROP TABLE node_trash_times;
