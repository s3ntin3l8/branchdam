-- +goose Up
-- Migration 00032 rebuilt media_nodes to widen the lifecycle_state CHECK and
-- re-created only the indexes it remembered; these two (from 00002 and 00020)
-- were silently dropped, turning camera-serial lookups and per-uploader
-- filters into full table scans.
CREATE INDEX IF NOT EXISTS ix_media_nodes_camera_time
    ON media_nodes(camera_serial, captured_at_unix) WHERE camera_serial IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_media_nodes_uploader
    ON media_nodes(uploaded_by_user_id) WHERE uploaded_by_user_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS ix_media_nodes_uploader;
DROP INDEX IF EXISTS ix_media_nodes_camera_time;
