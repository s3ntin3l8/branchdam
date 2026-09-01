-- +goose Up
-- 00015_source_path_hash.sql — add source_path_hash to media_nodes for agent dedup checks

ALTER TABLE media_nodes ADD COLUMN source_path_hash TEXT CHECK (source_path_hash IS NULL OR length(source_path_hash) = 64);

CREATE INDEX IF NOT EXISTS ix_media_nodes_source_path_hash
ON media_nodes(source_path_hash)
WHERE source_path_hash IS NOT NULL AND lifecycle_state != 'ARCHIVED';

-- +goose Down
DROP INDEX IF EXISTS ix_media_nodes_source_path_hash;
ALTER TABLE media_nodes DROP COLUMN source_path_hash;
