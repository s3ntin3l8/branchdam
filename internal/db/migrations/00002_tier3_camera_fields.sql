-- +goose Up
-- 00002_tier3_camera_fields.sql — promote camera_serial and lens_model onto media_nodes
--
-- Promotes camera_serial and lens_model from node_metadata key-value rows onto
-- media_nodes so Tier-3 heuristic spatial-temporal queries can run efficiently in SQL.

ALTER TABLE media_nodes ADD COLUMN camera_serial TEXT;
ALTER TABLE media_nodes ADD COLUMN lens_model TEXT;

CREATE INDEX ix_media_nodes_camera_time ON media_nodes(camera_serial, captured_at_unix) WHERE camera_serial IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS ix_media_nodes_camera_time;
ALTER TABLE media_nodes DROP COLUMN lens_model;
ALTER TABLE media_nodes DROP COLUMN camera_serial;
