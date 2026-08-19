-- +goose Up
ALTER TABLE remote_sync_state ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE remote_sync_state DROP COLUMN retry_count;
