-- +goose Up
-- Strict dedup: ensure full_hash is unique across all live (non-ARCHIVED) media nodes.
CREATE UNIQUE INDEX IF NOT EXISTS ux_media_nodes_live_full_hash
ON media_nodes(full_hash)
WHERE full_hash IS NOT NULL AND full_hash != '' AND lifecycle_state != 'ARCHIVED';

-- +goose Down
DROP INDEX IF EXISTS ux_media_nodes_live_full_hash;
