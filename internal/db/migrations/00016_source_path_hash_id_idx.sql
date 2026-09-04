-- +goose Up
-- 00016_source_path_hash_id_idx.sql — index for GetMediaNodeBySourcePathHash query performance
CREATE INDEX IF NOT EXISTS idx_media_nodes_source_path_hash_id
ON media_nodes(source_path_hash, id DESC)
WHERE source_path_hash IS NOT NULL AND lifecycle_state IN ('ACTIVE', 'HIDDEN');

-- +goose Down
DROP INDEX IF EXISTS idx_media_nodes_source_path_hash_id;
