-- +goose Up
CREATE INDEX idx_media_nodes_file_path ON media_nodes (file_path, id DESC);

-- +goose Down
DROP INDEX idx_media_nodes_file_path;
