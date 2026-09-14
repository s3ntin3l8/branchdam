-- +goose Up

-- Expand node_metadata.source CHECK to allow integration evidence sources.
-- Previous constraint only allowed ('exiftool','ffprobe','internal').
-- Virtual node evidence uses projectType-derived sources
-- (see internal/agent/drainer.go:applyVirtualNodeCreated):
--   projectType='resolve_project'  -> source='resolve_project_evidence'
--   projectType='premiere_project' -> source='premiere_project_evidence'
--   projectType='fcpxml_bundle'    -> source='fcpxml_bundle_evidence'
--   projectType=''                 -> source='virtual_evidence'
-- Use a LIKE pattern for extensibility rather than enumerating each integration.
CREATE TABLE node_metadata_new (
    node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    source  TEXT    NOT NULL CHECK (source IN ('exiftool','ffprobe','internal')
                                    OR source LIKE '%_evidence'),
    key     TEXT    NOT NULL,
    value   TEXT    NOT NULL,
    PRIMARY KEY (node_id, source, key)
);
INSERT INTO node_metadata_new SELECT * FROM node_metadata;
DROP TABLE node_metadata;
ALTER TABLE node_metadata_new RENAME TO node_metadata;

-- +goose Down

-- Restore node_metadata with original source CHECK.
CREATE TABLE node_metadata_old (
    node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    source  TEXT    NOT NULL CHECK (source IN ('exiftool','ffprobe','internal')),
    key     TEXT    NOT NULL,
    value   TEXT    NOT NULL,
    PRIMARY KEY (node_id, source, key)
);
INSERT INTO node_metadata_old SELECT * FROM node_metadata
    WHERE source IN ('exiftool','ffprobe','internal');
DROP TABLE node_metadata;
ALTER TABLE node_metadata_old RENAME TO node_metadata;
