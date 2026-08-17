-- 00001_init.sql — initial schema.
--
-- This is a corrected version of the design spec's DDL (docs/spec/original-spec.md
-- §6). Every deviation from that document is numbered and explained in
-- docs/schema.md; the numbers below (fix #N) cross-reference that ledger.
--
-- Deliberate omissions, so review can check them:
--   - No CREATE TRIGGER anywhere (fixes #4 and #5 — the spec's two triggers
--     were unsound; see docs/schema.md).
--   - No ON DELETE CASCADE anywhere (fix #6) — every FK is RESTRICT. A file
--     that disappears sets media_nodes.lifecycle_state = 'MISSING'; rows are
--     never deleted, because lineage history is the product.
--   - PRAGMA foreign_keys = ON is NOT set here — pragmas are connection-scoped
--     in SQLite, not persisted in the schema, so it is set by internal/db's
--     ConnectHook on every pooled connection instead. Fix #6 is INERT without it.

-- +goose Up

-- Storage tiers. Single source of truth for tier + access mode — fix #1.
-- media_nodes has NO separate location_type column; a node's tier is always
-- derived by joining storage_location_id here.
CREATE TABLE storage_locations (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    root_path       TEXT    NOT NULL UNIQUE,   -- absolute, no trailing slash, as seen INSIDE the container
    tier            TEXT    NOT NULL
        CHECK (tier IN ('TIER0_LOCAL_STAGING','TIER1_LOCAL_SCRATCH','TIER2_EXPORTS','TIER3_MASTER_ARCHIVE','PROJECTS')),
    read_only       INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0,1)),
    prunable        INTEGER NOT NULL DEFAULT 0 CHECK (prunable IN (0,1)),
    is_active       INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0,1)),
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    CHECK (tier <> 'TIER3_MASTER_ARCHIVE' OR read_only = 1),  -- Tier 3 is ALWAYS read-only
    CHECK (tier = 'TIER1_LOCAL_SCRATCH' OR prunable = 0)      -- only Tier 1 (scratch) may be prunable
);

-- Media asset nodes.
--   id INTEGER PRIMARY KEY is the internal join key (rowid alias — cheap).
--   node_uuid is the EXTERNAL identity: the API surface, XMP tags pushed to
--     Immich/Google Photos, and IDs the offline workstation agent mints in
--     its own queue.db before the server has ever seen the file — fix #9.
--   indexing_status / graph_status / lifecycle_state replace the spec's
--     single overloaded `status` column with three orthogonal axes — fix #2.
--     The Pillar-3 sync axis (PENDING_CLOUD_PUSH -> PUSHED) lives ONLY in
--     remote_sync_state, which already had it.
--   fast_hash is xxHash64 (16 hex); full_hash is BLAKE3-256 (64 hex) — the
--     spec called full_hash "cryptographic" but specified xxHash64, which
--     isn't — fix #8. Length CHECKs make it structurally impossible to write
--     a 64-bit value into the "cryptographic" column.
CREATE TABLE media_nodes (
    id                   INTEGER PRIMARY KEY,
    node_uuid            TEXT    NOT NULL UNIQUE,
    storage_location_id  INTEGER NOT NULL REFERENCES storage_locations(id) ON DELETE RESTRICT,
    file_path            TEXT    NOT NULL,
    file_name            TEXT    NOT NULL,
    file_ext             TEXT    NOT NULL DEFAULT '',
    size_bytes           INTEGER NOT NULL DEFAULT 0,
    mtime_unix           INTEGER NOT NULL DEFAULT 0,

    fast_hash TEXT CHECK (fast_hash IS NULL OR length(fast_hash) = 16),
    full_hash TEXT CHECK (full_hash IS NULL OR length(full_hash) = 64),
    phash     INTEGER,

    indexing_status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (indexing_status IN ('PENDING','INDEXED_SHALLOW','INDEXED_FULL','INDEX_FAILED')),
    graph_status TEXT NOT NULL DEFAULT 'UNLINKED'
        CHECK (graph_status IN ('UNLINKED','LINKED','NEEDS_REVIEW','ROOT')),
    lifecycle_state TEXT NOT NULL DEFAULT 'ACTIVE'
        CHECK (lifecycle_state IN ('ACTIVE','MISSING','ARCHIVED','HIDDEN')),

    -- fix #3: where a superseded version's successor lives. A row may only
    -- point at a successor once it has itself been retired.
    superseded_by INTEGER REFERENCES media_nodes(id) ON DELETE RESTRICT,

    -- promoted EXIF/probe fields the Tier-2 resolvers query on directly;
    -- everything else lands in node_metadata.
    original_document_id TEXT,   -- XMP:OriginalDocumentID
    document_id           TEXT,  -- XMP:DocumentID
    derived_from_id        TEXT, -- XMP:DerivedFrom
    captured_at_unix      INTEGER,
    camera_model          TEXT,
    filename_stem         TEXT,  -- normalized stem, computed in Go

    first_seen_at INTEGER NOT NULL DEFAULT (unixepoch()),
    last_seen_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    created_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    -- fix #5: no trigger sets this. Every UPDATE in internal/db/queries sets
    -- updated_at explicitly.
    updated_at    INTEGER NOT NULL DEFAULT (unixepoch()),

    CHECK (superseded_by IS NULL OR superseded_by <> id),
    CHECK (superseded_by IS NULL OR lifecycle_state = 'ARCHIVED')
);

-- fix #3: partial unique index replaces `file_path UNIQUE`. A re-export over
-- an existing filename archives the old row (lifecycle_state = 'ARCHIVED',
-- superseded_by = <new id>) and inserts a NEW node at the same path — the
-- spec's own version-collision requirement, made possible by the WHERE
-- clause. Verified working on SQLite 3.45.1 before this migration was written.
CREATE UNIQUE INDEX ux_media_nodes_live_path
    ON media_nodes(file_path) WHERE lifecycle_state <> 'ARCHIVED';

CREATE INDEX ix_media_nodes_fast_hash  ON media_nodes(fast_hash) WHERE fast_hash IS NOT NULL;
CREATE INDEX ix_media_nodes_full_hash  ON media_nodes(full_hash) WHERE full_hash IS NOT NULL;
CREATE INDEX ix_media_nodes_stem       ON media_nodes(filename_stem);
CREATE INDEX ix_media_nodes_origdocid  ON media_nodes(original_document_id) WHERE original_document_id IS NOT NULL;
CREATE INDEX ix_media_nodes_location   ON media_nodes(storage_location_id, lifecycle_state);
CREATE INDEX ix_media_nodes_work       ON media_nodes(indexing_status, id);
CREATE INDEX ix_media_nodes_superseded ON media_nodes(superseded_by) WHERE superseded_by IS NOT NULL;

-- Version graph lineage edges.
--   No DERIVED_FROM_MISSING_PARENT value — that was a STATE encoded as a
--   relationship type in the spec, not a relationship kind — fix #4.
--   Parent/child liveness is derived in v_media_edges_resolved below.
--   CHECK (source <> target) blocks self-loops (fix #7); longer cycles are
--   blocked at insert time in internal/graph via a recursive CTE walk, not
--   in the schema — SQLite CHECK constraints cannot see other rows.
CREATE TABLE media_edges (
    id                INTEGER PRIMARY KEY,
    source_node_id    INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT, -- parent / ancestor
    target_node_id    INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT, -- child / derivative
    relationship_type TEXT NOT NULL
        CHECK (relationship_type IN ('DERIVED_FROM','FINAL_EXPORT','PROXY_OF','PROJECT_SIDECAR','DUPLICATE_OF')),

    confidence    REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    tier          INTEGER NOT NULL CHECK (tier IN (1,2,3)),  -- which resolution tier produced it
    resolver      TEXT NOT NULL,                              -- e.g. 'xmp_original_document_id'
    evidence_json TEXT NOT NULL DEFAULT '{}',

    -- the audit queue (spec §7) is a QUERY over review_state, not a second table
    review_state TEXT NOT NULL DEFAULT 'NEEDS_REVIEW'
        CHECK (review_state IN ('AUTO_ACCEPTED','NEEDS_REVIEW','CONFIRMED','REJECTED')),
    reviewed_at INTEGER,
    reviewed_by TEXT,

    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch()),  -- fix #5: set in Go, no trigger

    CHECK (source_node_id <> target_node_id),  -- fix #7
    CHECK (review_state NOT IN ('CONFIRMED','REJECTED') OR reviewed_at IS NOT NULL),
    UNIQUE (source_node_id, target_node_id, relationship_type)
);

CREATE INDEX ix_media_edges_target ON media_edges(target_node_id);
CREATE INDEX ix_media_edges_source ON media_edges(source_node_id);
CREATE INDEX ix_media_edges_audit  ON media_edges(review_state, confidence DESC)
    WHERE review_state = 'NEEDS_REVIEW';

-- fix #4 (continued): parent-liveness derived, never stored — so there is
-- nothing to "restore" lossily the way the deleted trigger did, and it works
-- uniformly across every relationship_type, which the trigger never did
-- (it only ever rewrote DERIVED_FROM edges).
CREATE VIEW v_media_edges_resolved AS
SELECT
    e.id, e.source_node_id, e.target_node_id, e.relationship_type,
    e.confidence, e.tier, e.resolver, e.evidence_json,
    e.review_state, e.reviewed_at, e.reviewed_by,
    e.created_at, e.updated_at,
    p.lifecycle_state AS parent_lifecycle_state,
    c.lifecycle_state AS child_lifecycle_state,
    (p.lifecycle_state IN ('ACTIVE','HIDDEN')) AS parent_alive,
    (p.lifecycle_state = 'MISSING')            AS parent_missing
FROM media_edges e
JOIN media_nodes p ON p.id = e.source_node_id
JOIN media_nodes c ON c.id = e.target_node_id;

-- Pillar-3 sync axis (spec's PENDING_CLOUD_PUSH -> PUSHED) lives here and
-- ONLY here — fix #2. Unused until the Immich/Google Photos push increment;
-- created now so that increment is additive, not a schema migration.
CREATE TABLE remote_sync_state (
    node_id         INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    remote          TEXT    NOT NULL CHECK (remote IN ('IMMICH','GOOGLE_PHOTOS')),
    sync_status     TEXT    NOT NULL DEFAULT 'NOT_QUEUED'
        CHECK (sync_status IN ('NOT_QUEUED','PENDING_CLOUD_PUSH','PUSHING','PUSHED','PUSH_FAILED')),
    remote_asset_id TEXT,
    last_error      TEXT,
    last_attempt_at INTEGER,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (node_id, remote)
);

-- Generic EXIF/ffprobe key-value overflow. Hot fields the resolvers actually
-- query on are promoted onto media_nodes above; everything else lands here.
CREATE TABLE node_metadata (
    node_id INTEGER NOT NULL REFERENCES media_nodes(id) ON DELETE RESTRICT,
    source  TEXT    NOT NULL CHECK (source IN ('exiftool','ffprobe','internal')),
    key     TEXT    NOT NULL,
    value   TEXT    NOT NULL,
    PRIMARY KEY (node_id, source, key)
);

-- Agent replay buffer (spec's event_queue). Created now so
-- POST /api/v1/agent/events has somewhere to persist and can return 202 in
-- increment 1; actually draining/processing these rows ships with the
-- deferred workstation-agent increment.
CREATE TABLE event_queue (
    id           INTEGER PRIMARY KEY,
    event_uuid   TEXT NOT NULL UNIQUE,  -- client-minted UUIDv7
    agent_id     TEXT NOT NULL,
    event_type   TEXT NOT NULL
        CHECK (event_type IN ('EVENT_NODE_CREATED','EVENT_EDGE_ATTACHED','EVENT_NODE_MOVED','EVENT_NODE_DELETED','EVENT_PATH_REBASED')),
    payload_json TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','PROCESSED','FAILED')),
    error_log    TEXT,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    processed_at INTEGER
);

CREATE INDEX ix_event_queue_status_time ON event_queue(status, created_at);

-- Drives the SSE progress payload (internal/pipeline, PR 6).
CREATE TABLE scan_jobs (
    id                   INTEGER PRIMARY KEY,
    storage_location_id  INTEGER REFERENCES storage_locations(id) ON DELETE RESTRICT,
    kind                 TEXT NOT NULL CHECK (kind IN ('FULL_SCAN','INCREMENTAL','WATCH')),
    state                TEXT NOT NULL DEFAULT 'RUNNING'
        CHECK (state IN ('RUNNING','COMPLETED','FAILED','CANCELLED')),
    files_seen    INTEGER NOT NULL DEFAULT 0,
    files_hashed  INTEGER NOT NULL DEFAULT 0,
    files_failed  INTEGER NOT NULL DEFAULT 0,
    edges_created INTEGER NOT NULL DEFAULT 0,
    started_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    finished_at   INTEGER,
    last_error    TEXT,
    updated_at    INTEGER NOT NULL DEFAULT (unixepoch())  -- fix #5: set in Go, no trigger
);

CREATE INDEX ix_scan_jobs_active ON scan_jobs(state, started_at DESC);

-- +goose Down

DROP VIEW  IF EXISTS v_media_edges_resolved;
DROP TABLE IF EXISTS scan_jobs;
DROP TABLE IF EXISTS event_queue;
DROP TABLE IF EXISTS node_metadata;
DROP TABLE IF EXISTS remote_sync_state;
DROP TABLE IF EXISTS media_edges;
-- DROP TABLE IF EXISTS media_nodes;
DROP TABLE IF EXISTS storage_locations;
