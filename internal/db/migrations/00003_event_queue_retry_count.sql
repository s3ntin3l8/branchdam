-- +goose Up
ALTER TABLE event_queue ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE event_queue DROP COLUMN retry_count;
