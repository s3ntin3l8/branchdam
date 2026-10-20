-- +goose Up
-- Two live-node lookups had no supporting index and did a full table
-- scan on media_nodes:
--
--   ListLiveNodesByFileName    WHERE file_name = ?1
--   ListLiveNodesByDocumentID  WHERE document_id = ?1
--
-- document_id sat next to ix_media_nodes_origdocid, but that index is
-- on original_document_id -- a different column, used by the
-- xmpOriginalDocumentID resolver's own lookup. file_name had no index
-- at all. Both are read on every lineage resolution (the project-sidecar
-- resolver's filename fallback and the xmpOriginalDocumentID resolver),
-- so on a library of any size each resolution scanned the whole table.
--
-- The indexes are plain rather than partial on the live-state
-- predicate: that predicate is
-- lifecycle_state NOT IN ('ARCHIVED','TRASHED'), which leaves nearly
-- every row live, so a partial index would not be meaningfully smaller
-- than the full one.
CREATE INDEX IF NOT EXISTS idx_media_nodes_file_name ON media_nodes (file_name);
CREATE INDEX IF NOT EXISTS idx_media_nodes_document_id ON media_nodes (document_id);

-- +goose Down
DROP INDEX IF EXISTS idx_media_nodes_file_name;
DROP INDEX IF EXISTS idx_media_nodes_document_id;
