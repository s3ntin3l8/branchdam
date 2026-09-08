-- +goose Up
ALTER TABLE storage_locations ADD COLUMN is_virtual INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE storage_locations DROP COLUMN is_virtual;
