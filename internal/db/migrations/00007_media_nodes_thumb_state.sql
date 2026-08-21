-- +goose Up
-- thumb_state tracks whether internal/thumbs has generated (or attempted
-- to generate) a cached JPEG thumbnail for this node. Every existing row
-- defaults to PENDING, so internal/thumbs.Worker backfills the whole
-- library on first start -- no data-correction migration needed, matching
-- the additive-by-default pattern in docs/schema.md's Post-Increment-1
-- Additions note (unlike #132's 00006, which needed one).
ALTER TABLE media_nodes ADD COLUMN thumb_state TEXT NOT NULL DEFAULT 'PENDING'
    CHECK (thumb_state IN ('PENDING', 'READY', 'UNSUPPORTED', 'FAILED'));
ALTER TABLE media_nodes ADD COLUMN thumb_attempts INTEGER NOT NULL DEFAULT 0;

-- Partial index over exactly the predicate the worker's claim query
-- filters on (thumb_state = 'PENDING'), so the index stays small as the
-- PENDING backlog drains rather than covering every row regardless of
-- state -- same pattern as 00002's ix_media_nodes_camera_time.
CREATE INDEX idx_media_nodes_thumb_pending ON media_nodes (id) WHERE thumb_state = 'PENDING';

-- +goose Down
DROP INDEX IF EXISTS idx_media_nodes_thumb_pending;
ALTER TABLE media_nodes DROP COLUMN thumb_attempts;
ALTER TABLE media_nodes DROP COLUMN thumb_state;
