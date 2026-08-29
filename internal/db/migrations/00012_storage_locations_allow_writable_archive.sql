-- +goose Up
-- Allow TIER3_MASTER_ARCHIVE storage locations to be configured as read_only=0 for server-governed archive ingestion.

PRAGMA foreign_keys = OFF;

CREATE TABLE storage_locations_new (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL,
    root_path       TEXT    NOT NULL UNIQUE,   -- absolute, no trailing slash, as seen INSIDE the container
    tier            TEXT    NOT NULL
        CHECK (tier IN ('TIER0_LOCAL_STAGING','TIER1_LOCAL_SCRATCH','TIER2_EXPORTS','TIER3_MASTER_ARCHIVE','PROJECTS')),
    read_only       INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0,1)),
    prunable        INTEGER NOT NULL DEFAULT 0 CHECK (prunable IN (0,1)),
    is_active       INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0,1)),
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    cache_ttl_hours INTEGER NOT NULL DEFAULT 0,
    CHECK (tier = 'TIER1_LOCAL_SCRATCH' OR prunable = 0)      -- only Tier 1 (scratch) may be prunable
);

INSERT INTO storage_locations_new (id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at, cache_ttl_hours)
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at, cache_ttl_hours
FROM storage_locations;

DROP TABLE storage_locations;

ALTER TABLE storage_locations_new RENAME TO storage_locations;

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

CREATE TABLE storage_locations_old (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL,
    root_path       TEXT    NOT NULL UNIQUE,
    tier            TEXT    NOT NULL
        CHECK (tier IN ('TIER0_LOCAL_STAGING','TIER1_LOCAL_SCRATCH','TIER2_EXPORTS','TIER3_MASTER_ARCHIVE','PROJECTS')),
    read_only       INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0,1)),
    prunable        INTEGER NOT NULL DEFAULT 0 CHECK (prunable IN (0,1)),
    is_active       INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0,1)),
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    cache_ttl_hours INTEGER NOT NULL DEFAULT 0,
    CHECK (tier <> 'TIER3_MASTER_ARCHIVE' OR read_only = 1),
    CHECK (tier = 'TIER1_LOCAL_SCRATCH' OR prunable = 0)
);

INSERT INTO storage_locations_old (id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at, cache_ttl_hours)
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at, cache_ttl_hours
FROM storage_locations;

DROP TABLE storage_locations;

ALTER TABLE storage_locations_old RENAME TO storage_locations;

PRAGMA foreign_keys = ON;
