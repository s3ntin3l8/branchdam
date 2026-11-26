-- +goose Up
-- User and assistant tags and descriptions stay off node_metadata so
-- extracted EXIF can never be confused with something an assistant wrote.
CREATE TABLE asset_tags (
    id INTEGER PRIMARY KEY,
    node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    tag TEXT NOT NULL CHECK (length(tag) BETWEEN 1 AND 64 AND tag = lower(trim(tag))),
    source TEXT NOT NULL CHECK (source IN ('user', 'assistant')),
    created_by_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    pat_id INTEGER REFERENCES user_pats(id) ON DELETE RESTRICT,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (node_id, tag)
);
CREATE INDEX idx_asset_tags_tag ON asset_tags (tag);
CREATE INDEX idx_asset_tags_revert ON asset_tags (source, pat_id, created_at);

CREATE TABLE asset_annotations (
    id INTEGER PRIMARY KEY,
    node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK (kind IN ('description', 'note')),
    text TEXT NOT NULL CHECK (length(text) BETWEEN 1 AND 4000),
    source TEXT NOT NULL CHECK (source IN ('user', 'assistant')),
    model TEXT,
    created_by_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    pat_id INTEGER REFERENCES user_pats(id) ON DELETE RESTRICT,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);
-- One description per node and source. Notes are not unique.
CREATE UNIQUE INDEX ux_asset_annotations_description
    ON asset_annotations (node_id, source) WHERE kind = 'description';
CREATE INDEX idx_asset_annotations_node ON asset_annotations (node_id);
CREATE INDEX idx_asset_annotations_revert ON asset_annotations (source, pat_id, created_at);

-- The search view gains tags and annotation text. Triggers that read
-- the view have to be dropped before the view can be replaced.
DROP TRIGGER storage_locations_fts_update;
DROP TRIGGER node_metadata_fts_delete;
DROP TRIGGER node_metadata_fts_update;
DROP TRIGGER node_metadata_fts_insert;
DROP TRIGGER media_nodes_fts_delete;
DROP TRIGGER media_nodes_fts_update;
DROP TRIGGER media_nodes_fts_insert;
DROP VIEW v_media_nodes_search;

CREATE VIEW v_media_nodes_search AS
SELECT n.id AS rowid, n.file_name,
       CASE WHEN substr(n.file_path, 1, length(rtrim(s.root_path, '/')) + 1) = rtrim(s.root_path, '/') || '/'
            THEN substr(n.file_path, length(rtrim(s.root_path, '/')) + 2)
            ELSE n.file_path END AS relative_path,
       coalesce(n.camera_model, '') || ' ' || coalesce(n.lens_model, '') || ' ' ||
       coalesce((SELECT group_concat(value, ' ') FROM node_metadata
                 WHERE node_id = n.id AND key IN ('EXIF:Make', 'EXIF:Model', 'EXIF:LensModel', 'EXIF:DateTimeOriginal',
                      'project_name', 'timeline_name')), '') || ' ' ||
       coalesce((SELECT group_concat(tag, ' ') FROM asset_tags WHERE node_id = n.id), '') || ' ' ||
       coalesce((SELECT group_concat(text, ' ') FROM asset_annotations WHERE node_id = n.id), '') AS meta
FROM media_nodes n JOIN storage_locations s ON s.id = n.storage_location_id;

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

-- +goose StatementBegin
CREATE TRIGGER asset_tags_fts_insert AFTER INSERT ON asset_tags BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = new.node_id;
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid = new.node_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER asset_tags_fts_update AFTER UPDATE ON asset_tags BEGIN
    DELETE FROM media_nodes_fts WHERE rowid IN (old.node_id, new.node_id);
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid IN (old.node_id, new.node_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER asset_tags_fts_delete AFTER DELETE ON asset_tags BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = old.node_id;
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid = old.node_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER asset_annotations_fts_insert AFTER INSERT ON asset_annotations BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = new.node_id;
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid = new.node_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER asset_annotations_fts_update AFTER UPDATE ON asset_annotations BEGIN
    DELETE FROM media_nodes_fts WHERE rowid IN (old.node_id, new.node_id);
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid IN (old.node_id, new.node_id);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER asset_annotations_fts_delete AFTER DELETE ON asset_annotations BEGIN
    DELETE FROM media_nodes_fts WHERE rowid = old.node_id;
    INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
    SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search WHERE rowid = old.node_id;
END;
-- +goose StatementEnd

DELETE FROM media_nodes_fts;
INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search;

-- +goose Down
DROP TRIGGER asset_annotations_fts_delete;
DROP TRIGGER asset_annotations_fts_update;
DROP TRIGGER asset_annotations_fts_insert;
DROP TRIGGER asset_tags_fts_delete;
DROP TRIGGER asset_tags_fts_update;
DROP TRIGGER asset_tags_fts_insert;
DROP TRIGGER storage_locations_fts_update;
DROP TRIGGER node_metadata_fts_delete;
DROP TRIGGER node_metadata_fts_update;
DROP TRIGGER node_metadata_fts_insert;
DROP TRIGGER media_nodes_fts_delete;
DROP TRIGGER media_nodes_fts_update;
DROP TRIGGER media_nodes_fts_insert;
DROP VIEW v_media_nodes_search;
DROP TABLE asset_annotations;
DROP TABLE asset_tags;

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

DELETE FROM media_nodes_fts;
INSERT INTO media_nodes_fts(rowid, file_name, relative_path, meta)
SELECT rowid, file_name, relative_path, meta FROM v_media_nodes_search;
