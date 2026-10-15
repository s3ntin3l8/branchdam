-- +goose Up
-- Records which linked exports a master's trash operation cascaded into the
-- trash, so restoring that master brings back exactly those exports and
-- nothing else. Before this, restore re-activated every TRASHED export linked
-- to the master, including exports the user had trashed on their own.
--
-- One row per (master trash, export) pair. restored_at is set when the export
-- comes back -- with its master, or on its own -- so a row only ever
-- authorises one restore; rows are never deleted. Both FKs are RESTRICT,
-- matching every other FK in the schema. A separate table (rather than a
-- media_nodes column) keeps the many explicit-column media_nodes queries
-- untouched, the same choice 00035_node_creators made.
--
-- No backfill: an export cascade-trashed before this migration cannot be told
-- apart from one trashed on its own, so restoring such a master leaves its
-- trashed exports in .trash/, where each can still be restored individually
-- until trash retention expires.
CREATE TABLE trash_cascades (
    id             INTEGER PRIMARY KEY,
    parent_node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    export_node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    trashed_at     INTEGER NOT NULL DEFAULT (unixepoch()),
    restored_at    INTEGER
);

CREATE INDEX ix_trash_cascades_parent_open
    ON trash_cascades (parent_node_id)
    WHERE restored_at IS NULL;

CREATE INDEX ix_trash_cascades_export_open
    ON trash_cascades (export_node_id)
    WHERE restored_at IS NULL;

-- +goose Down
DROP TABLE trash_cascades;
