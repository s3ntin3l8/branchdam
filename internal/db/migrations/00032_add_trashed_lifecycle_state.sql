-- +goose NO TRANSACTION
-- +goose Up
--
-- Add TRASHED lifecycle state for deliberate user-initiated deletes.
--
--   ACTIVE   -- normal, queryable
--   MISSING  -- file vanished, scan detected
--   ARCHIVED -- version-collision superseded, or non-trash user delete
--   HIDDEN   -- user-visible but excluded from default listings
--   TRASHED  -- NEW: file physically moved to <root>/.trash/<rel_path>;
--              auto-purged after 30 days. Differs from MISSING in that
--              TRASHED is a deliberate user action (DELETE, POST /trash,
--              or agent EVENT_NODE_DELETED) whereas MISSING is scan-detected
--              disappearance. Differs from ARCHIVED in that the bytes are
--              no longer at the original path -- only in .trash/.
--
-- SQLite cannot ALTER CHECK constraints; rebuild the table per the 12-step
-- recipe (same pattern as 00024_virtual_node_and_event.sql).
--
-- All partial indexes that filter on lifecycle_state must be dropped and
-- recreated after the rebuild, with their WHERE clauses widened from
--   != 'ARCHIVED'
-- to
--   NOT IN ('ARCHIVED','TRASHED')
-- so TRASHED rows are excluded from default listings, lineage walks,
-- sync state, and storage health counts. Same predicate change applies
-- to 00013_dedup_existing_hashes's archival CTE -- recreated here so the
-- data invariant survives the rebuild.

-- Goose executes NO TRANSACTION statements one at a time through the writer
-- pool. internal/db pins that pool to one connection, so these statements
-- share one SQLite session and the PRAGMA applies to the explicit transaction.
PRAGMA foreign_keys = OFF;
BEGIN TRANSACTION;

-- This transaction is owned by the migration because Goose is in NO
-- TRANSACTION mode. If a statement fails before COMMIT, Goose does not roll
-- it back or restore the connection-scoped PRAGMA; the caller must discard
-- the connection rather than reuse it.
DROP TABLE IF EXISTS media_nodes_new;

CREATE TABLE media_nodes_new (
    id                INTEGER PRIMARY KEY,
    node_uuid         TEXT NOT NULL UNIQUE,
    storage_location_id INTEGER NOT NULL REFERENCES storage_locations(id) ON DELETE RESTRICT,
    file_path         TEXT NOT NULL,
    file_name         TEXT NOT NULL,
    file_ext          TEXT NOT NULL DEFAULT '',
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    mtime_unix        INTEGER NOT NULL DEFAULT 0,

    fast_hash TEXT CHECK (fast_hash IS NULL OR length(fast_hash) = 16),
    full_hash TEXT CHECK (full_hash IS NULL OR length(full_hash) = 64),
    phash     INTEGER,

    indexing_status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (indexing_status IN ('PENDING','INDEXED_SHALLOW','INDEXED_FULL','INDEX_FAILED')),
    graph_status TEXT NOT NULL DEFAULT 'UNLINKED'
        CHECK (graph_status IN ('UNLINKED','LINKED','NEEDS_REVIEW','ROOT')),
    lifecycle_state TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (lifecycle_state IN ('ACTIVE','MISSING','ARCHIVED','HIDDEN','TRASHED')),

    superseded_by INTEGER REFERENCES media_nodes(id) ON DELETE RESTRICT,

    original_document_id TEXT,
    document_id           TEXT,
    derived_from_id        TEXT,
    captured_at_unix      INTEGER,
    camera_model          TEXT,
    filename_stem         TEXT,

    first_seen_at INTEGER NOT NULL DEFAULT (unixepoch()),
    last_seen_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    created_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at    INTEGER NOT NULL DEFAULT (unixepoch()),

    camera_serial TEXT,
    lens_model    TEXT,
    thumb_state    TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (thumb_state IN ('PENDING','READY','UNSUPPORTED','FAILED')),
    thumb_attempts INTEGER NOT NULL DEFAULT 0,

    source_path_hash TEXT CHECK (source_path_hash IS NULL OR length(source_path_hash) = 64),
    uploaded_by_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT,

    CHECK (superseded_by IS NULL OR superseded_by <> id),
    CHECK (superseded_by IS NULL OR lifecycle_state = 'ARCHIVED')
);

INSERT INTO media_nodes_new (
    id, node_uuid, storage_location_id, file_path, file_name, file_ext,
    size_bytes, mtime_unix, fast_hash, full_hash, phash,
    indexing_status, graph_status, lifecycle_state, superseded_by,
    original_document_id, document_id, derived_from_id,
    captured_at_unix, camera_model, filename_stem,
    first_seen_at, last_seen_at, created_at, updated_at,
    camera_serial, lens_model, thumb_state, thumb_attempts,
    source_path_hash, uploaded_by_user_id
)
SELECT
    id, node_uuid, storage_location_id, file_path, file_name, file_ext,
    size_bytes, mtime_unix, fast_hash, full_hash, phash,
    indexing_status, graph_status, lifecycle_state, superseded_by,
    original_document_id, document_id, derived_from_id,
    captured_at_unix, camera_model, filename_stem,
    first_seen_at, last_seen_at, created_at, updated_at,
    camera_serial, lens_model, thumb_state, thumb_attempts,
    source_path_hash, uploaded_by_user_id
FROM media_nodes;

-- The view v_media_edges_resolved depends on media_nodes, so it must be
-- dropped before media_nodes can be dropped. Recreated below after the
-- rebuild. (This is the same view recreated in 00026_resolve_snapshot.sql
-- with the same parent_alive/parent_missing predicates; widening it to
-- handle TRASHED is intentional -- a TRASHED parent is not "alive".)
DROP VIEW IF EXISTS v_media_edges_resolved;
DROP TABLE media_nodes;
ALTER TABLE media_nodes_new RENAME TO media_nodes;

-- Partial unique index on file_path: a re-export over an existing filename
-- archives the old row (lifecycle_state = 'ARCHIVED', superseded_by = <new id>)
-- and inserts a NEW node at the same path. TRASHED is added to the exclusion
-- set so a trashed master does not block a same-name re-import.
CREATE UNIQUE INDEX ux_media_nodes_live_path
    ON media_nodes(file_path) WHERE lifecycle_state NOT IN ('ARCHIVED','TRASHED');

CREATE INDEX ix_media_nodes_fast_hash  ON media_nodes(fast_hash) WHERE fast_hash IS NOT NULL;
CREATE INDEX ix_media_nodes_full_hash  ON media_nodes(full_hash) WHERE full_hash IS NOT NULL;
CREATE INDEX ix_media_nodes_stem       ON media_nodes(filename_stem);
CREATE INDEX ix_media_nodes_origdocid  ON media_nodes(original_document_id) WHERE original_document_id IS NOT NULL;
CREATE INDEX ix_media_nodes_location   ON media_nodes(storage_location_id, lifecycle_state);
CREATE INDEX ix_media_nodes_work       ON media_nodes(indexing_status, id);
CREATE INDEX ix_media_nodes_superseded ON media_nodes(superseded_by) WHERE superseded_by IS NOT NULL;

-- 00014: strict full_hash dedup across ACTIVE+HIDDEN. ARCHIVED+TRASHED are
-- excluded so a re-import of an old archive's bytes can succeed after a trash.
CREATE UNIQUE INDEX ux_media_nodes_live_full_hash
    ON media_nodes(full_hash)
    WHERE full_hash IS NOT NULL AND full_hash != '' AND lifecycle_state IN ('ACTIVE', 'HIDDEN');

-- 00015: source_path_hash index for agent dedup checks. Widened to exclude
-- TRASHED so a trashed master does not block a same-source-path re-ingest.
CREATE INDEX ix_media_nodes_source_path_hash
    ON media_nodes(source_path_hash)
    WHERE source_path_hash IS NOT NULL AND lifecycle_state NOT IN ('ARCHIVED','TRASHED');

-- 00016: compound (source_path_hash, id DESC) for GetMediaNodeBySourcePathHash.
-- Same widening as 00015.
CREATE INDEX idx_media_nodes_source_path_hash_id
    ON media_nodes(source_path_hash, id DESC)
    WHERE source_path_hash IS NOT NULL AND lifecycle_state IN ('ACTIVE', 'HIDDEN');

-- 00007: partial index over the worker's claim query filter
-- (thumb_state = 'PENDING'). Recreated after the rebuild so the worker's
-- ListPendingThumbnails lookup stays cheap.
CREATE INDEX idx_media_nodes_thumb_pending ON media_nodes (id) WHERE thumb_state = 'PENDING';

-- 00023: compound (file_path, id DESC) for GetMediaNodeByFilePath. WHERE
-- clause intentionally omits lifecycle_state filter so a TRASHED or
-- ARCHIVED row at a path is still discoverable by full-path lookup.
CREATE INDEX idx_media_nodes_file_path ON media_nodes (file_path, id DESC);

-- 00026 view, rebuilt with TRASHED-aware parent_alive predicate.
-- parent_alive: ACTIVE or HIDDEN. TRASHED, MISSING, ARCHIVED are all
-- not-alive for lineage purposes (TRASHED is not "alive" because the
-- bytes are no longer at the original path; restoring re-makes it alive).
DROP VIEW IF EXISTS v_media_edges_resolved;
CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING') AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id
WHERE e.is_active = 1;

CREATE TEMP TABLE migration_00032_fk_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
-- Use the table-valued PRAGMA form so a violation becomes a CHECK failure;
-- Goose discards rows returned by a bare PRAGMA in NO TRANSACTION mode.
INSERT INTO migration_00032_fk_guard (ok)
SELECT 0 FROM pragma_foreign_key_check LIMIT 1;
DROP TABLE migration_00032_fk_guard;

COMMIT;
PRAGMA foreign_keys = ON;

-- +goose Down
--
-- Reverting this migration is a no-op for TRASHED rows: a downgrade sees
-- 'TRASHED' as not in the old CHECK and will reject the INSERT. Refuse a
-- downgrade when any TRASHED rows exist; otherwise the rebuild below would
-- silently drop them.
DROP TABLE IF EXISTS trashed_downgrade_guard;
CREATE TEMP TABLE trashed_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO trashed_downgrade_guard (ok)
SELECT 0 WHERE EXISTS (SELECT 1 FROM media_nodes WHERE lifecycle_state = 'TRASHED');
DROP TABLE trashed_downgrade_guard;

PRAGMA foreign_keys = OFF;
BEGIN TRANSACTION;

DROP TABLE IF EXISTS media_nodes_old;

CREATE TABLE media_nodes_old (
    id                INTEGER PRIMARY KEY,
    node_uuid         TEXT NOT NULL UNIQUE,
    storage_location_id INTEGER NOT NULL REFERENCES storage_locations(id) ON DELETE RESTRICT,
    file_path         TEXT NOT NULL,
    file_name         TEXT NOT NULL,
    file_ext          TEXT NOT NULL DEFAULT '',
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    mtime_unix        INTEGER NOT NULL DEFAULT 0,

    fast_hash TEXT CHECK (fast_hash IS NULL OR length(fast_hash) = 16),
    full_hash TEXT CHECK (full_hash IS NULL OR length(full_hash) = 64),
    phash     INTEGER,

    indexing_status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (indexing_status IN ('PENDING','INDEXED_SHALLOW','INDEXED_FULL','INDEX_FAILED')),
    graph_status TEXT NOT NULL DEFAULT 'UNLINKED'
        CHECK (graph_status IN ('UNLINKED','LINKED','NEEDS_REVIEW','ROOT')),
    lifecycle_state TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (lifecycle_state IN ('ACTIVE','MISSING','ARCHIVED','HIDDEN')),

    superseded_by INTEGER REFERENCES media_nodes(id) ON DELETE RESTRICT,

    original_document_id TEXT,
    document_id           TEXT,
    derived_from_id        TEXT,
    captured_at_unix      INTEGER,
    camera_model          TEXT,
    filename_stem         TEXT,

    first_seen_at INTEGER NOT NULL DEFAULT (unixepoch()),
    last_seen_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    created_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at    INTEGER NOT NULL DEFAULT (unixepoch()),

    camera_serial TEXT,
    lens_model    TEXT,
    thumb_state    TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (thumb_state IN ('PENDING','READY','UNSUPPORTED','FAILED')),
    thumb_attempts INTEGER NOT NULL DEFAULT 0,

    source_path_hash TEXT CHECK (source_path_hash IS NULL OR length(source_path_hash) = 64),
    uploaded_by_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT,

    CHECK (superseded_by IS NULL OR superseded_by <> id),
    CHECK (superseded_by IS NULL OR lifecycle_state = 'ARCHIVED')
);

-- The guard above rejects TRASHED rows before this rebuild, so every row
-- copied here is valid under the pre-00032 lifecycle CHECK constraint.
INSERT INTO media_nodes_old (
    id, node_uuid, storage_location_id, file_path, file_name, file_ext,
    size_bytes, mtime_unix, fast_hash, full_hash, phash,
    indexing_status, graph_status, lifecycle_state, superseded_by,
    original_document_id, document_id, derived_from_id,
    captured_at_unix, camera_model, filename_stem,
    first_seen_at, last_seen_at, created_at, updated_at,
    camera_serial, lens_model, thumb_state, thumb_attempts,
    source_path_hash, uploaded_by_user_id
)
SELECT
    id, node_uuid, storage_location_id, file_path, file_name, file_ext,
    size_bytes, mtime_unix, fast_hash, full_hash, phash,
    indexing_status,
    CASE WHEN graph_status = 'ROOT' THEN 'ROOT' ELSE 'UNLINKED' END,
    lifecycle_state,
    superseded_by,
    original_document_id, document_id, derived_from_id,
    captured_at_unix, camera_model, filename_stem,
    first_seen_at, last_seen_at, created_at, updated_at,
    camera_serial, lens_model, thumb_state, thumb_attempts,
    source_path_hash, uploaded_by_user_id
FROM media_nodes;

-- Drop the view before dropping the table; rebuilt below.
DROP VIEW IF EXISTS v_media_edges_resolved;
DROP TABLE media_nodes;
ALTER TABLE media_nodes_old RENAME TO media_nodes;

CREATE UNIQUE INDEX ux_media_nodes_live_path
    ON media_nodes(file_path) WHERE lifecycle_state <> 'ARCHIVED';

CREATE INDEX ix_media_nodes_fast_hash  ON media_nodes(fast_hash) WHERE fast_hash IS NOT NULL;
CREATE INDEX ix_media_nodes_full_hash  ON media_nodes(full_hash) WHERE full_hash IS NOT NULL;
CREATE INDEX ix_media_nodes_stem       ON media_nodes(filename_stem);
CREATE INDEX ix_media_nodes_origdocid  ON media_nodes(original_document_id) WHERE original_document_id IS NOT NULL;
CREATE INDEX ix_media_nodes_location   ON media_nodes(storage_location_id, lifecycle_state);
CREATE INDEX ix_media_nodes_work       ON media_nodes(indexing_status, id);
CREATE INDEX ix_media_nodes_superseded ON media_nodes(superseded_by) WHERE superseded_by IS NOT NULL;

CREATE UNIQUE INDEX ux_media_nodes_live_full_hash
    ON media_nodes(full_hash)
    WHERE full_hash IS NOT NULL AND full_hash != '' AND lifecycle_state IN ('ACTIVE', 'HIDDEN');

CREATE INDEX ix_media_nodes_source_path_hash
    ON media_nodes(source_path_hash)
    WHERE source_path_hash IS NOT NULL AND lifecycle_state != 'ARCHIVED';

CREATE INDEX idx_media_nodes_source_path_hash_id
    ON media_nodes(source_path_hash, id DESC)
    WHERE source_path_hash IS NOT NULL AND lifecycle_state IN ('ACTIVE', 'HIDDEN');

-- 00007 partial index for the thumbnail worker's claim query, narrowed
-- back to the pre-TRASHED shape (lifecycle_state filter would otherwise
-- silently change between this migration and 00007's reverse).
CREATE INDEX idx_media_nodes_thumb_pending ON media_nodes (id) WHERE thumb_state = 'PENDING';

-- 00023 compound (file_path, id DESC) index for GetMediaNodeByFilePath.
CREATE INDEX idx_media_nodes_file_path ON media_nodes (file_path, id DESC);

DROP VIEW IF EXISTS v_media_edges_resolved;
CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING') AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id
WHERE e.is_active = 1;

CREATE TEMP TABLE migration_00032_fk_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
-- Keep this check inside the rebuild transaction so a violation can roll it
-- back before the migration commits.
INSERT INTO migration_00032_fk_guard (ok)
SELECT 0 FROM pragma_foreign_key_check LIMIT 1;
DROP TABLE migration_00032_fk_guard;

COMMIT;
PRAGMA foreign_keys = ON;
