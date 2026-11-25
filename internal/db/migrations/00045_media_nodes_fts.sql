-- +goose Up
-- A contentless-delete index keeps only tokens, not a second copy of paths
-- and metadata. SQLite >= 3.43 is bundled by our go-sqlite3 dependency.
CREATE VIEW v_media_nodes_search AS
SELECT n.id AS rowid, n.file_name,
       CASE WHEN substr(n.file_path, 1, length(rtrim(s.root_path, '/')) + 1) = rtrim(s.root_path, '/') || '/'
            THEN substr(n.file_path, length(rtrim(s.root_path, '/')) + 2)
            ELSE n.file_path END AS relative_path,
       coalesce(n.camera_model, '') || ' ' || coalesce(n.lens_model, '') || ' ' ||
       coalesce((SELECT group_concat(value, ' ') FROM node_metadata
                 WHERE node_id = n.id AND key IN ('EXIF:Make', 'EXIF:Model', 'EXIF:LensModel', 'EXIF:DateTimeOriginal',
                      'project_name', 'timeline_name')), '') AS meta
FROM media_nodes n JOIN storage_locations s ON s.id = n.storage_location_id;

CREATE VIRTUAL TABLE media_nodes_fts USING fts5(
    file_name, relative_path, meta,
    content='', contentless_delete=1, prefix='2 3 4',
    tokenize="unicode61 tokenchars '%_'"
);

INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search;

-- Explicit DELETE + INSERT is deliberate: an outer metadata UPSERT can
-- override INSERT OR REPLACE conflict handling inside a trigger. Replacing
-- without deleting first can leave stale FTS tokens behind.
-- +goose StatementBegin
CREATE TRIGGER media_nodes_fts_insert AFTER INSERT ON media_nodes BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = new.id;
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid = new.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER media_nodes_fts_update AFTER UPDATE OF file_name, file_path, storage_location_id, camera_model, lens_model ON media_nodes BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = old.id;
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid = new.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER media_nodes_fts_delete AFTER DELETE ON media_nodes BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = old.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER node_metadata_fts_insert AFTER INSERT ON node_metadata
WHEN new.key IN ('EXIF:Make', 'EXIF:Model', 'EXIF:LensModel', 'EXIF:DateTimeOriginal', 'project_name', 'timeline_name')
BEGIN
    DELETE FROM media_nodes_fts WHERE rowid IN (new.node_id);
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid IN (new.node_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER node_metadata_fts_update AFTER UPDATE ON node_metadata
WHEN old.key IN ('EXIF:Make', 'EXIF:Model', 'EXIF:LensModel', 'EXIF:DateTimeOriginal', 'project_name', 'timeline_name') OR new.key IN ('EXIF:Make', 'EXIF:Model', 'EXIF:LensModel', 'EXIF:DateTimeOriginal', 'project_name', 'timeline_name')
BEGIN
    DELETE FROM media_nodes_fts WHERE rowid IN (old.node_id, new.node_id);
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid IN (old.node_id, new.node_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER node_metadata_fts_delete AFTER DELETE ON node_metadata
WHEN old.key IN ('EXIF:Make', 'EXIF:Model', 'EXIF:LensModel', 'EXIF:DateTimeOriginal', 'project_name', 'timeline_name')
BEGIN
    DELETE FROM media_nodes_fts WHERE rowid IN (old.node_id);
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid IN (old.node_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER storage_locations_fts_update AFTER UPDATE OF root_path ON storage_locations
WHEN old.root_path IS NOT new.root_path
BEGIN
    DELETE FROM media_nodes_fts WHERE rowid IN (SELECT id FROM media_nodes WHERE storage_location_id = new.id);
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT v.rowid, v.file_name, v.relative_path, v.meta FROM v_media_nodes_search v
    JOIN media_nodes n ON n.id = v.rowid WHERE n.storage_location_id = new.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER storage_locations_fts_update;
DROP TRIGGER node_metadata_fts_delete;
DROP TRIGGER node_metadata_fts_update;
DROP TRIGGER node_metadata_fts_insert;
DROP TRIGGER media_nodes_fts_delete;
DROP TRIGGER media_nodes_fts_update;
DROP TRIGGER media_nodes_fts_insert;
DROP TABLE media_nodes_fts;
DROP VIEW v_media_nodes_search;
