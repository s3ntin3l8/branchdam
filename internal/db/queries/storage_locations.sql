-- name: CreateStorageLocation :one
INSERT INTO storage_locations (name, root_path, tier, read_only, prunable)
VALUES (?1, ?2, ?3, ?4, ?5)
RETURNING id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at;

-- name: ListStorageLocations :many
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
FROM storage_locations
ORDER BY id;

-- name: UpsertStorageLocation :one
-- Backs config-driven seeding at startup (cmd/branchdam): config.yaml's
-- storageLocations list is applied idempotently on every restart, keyed on
-- root_path's UNIQUE constraint, so re-running it against an
-- already-seeded database updates tier/read_only/prunable in place rather
-- than failing on the second startup. is_active is unconditionally reset
-- to 1 here -- a location present in config is presumed active until
-- storage.LoadGuard's post-seed resolvability check (M6) says otherwise
-- via SetStorageLocationActive, which is what makes a location that
-- vanished and came back self-heal on the next successful startup.
INSERT INTO storage_locations (name, root_path, tier, read_only, prunable)
VALUES (?1, ?2, ?3, ?4, ?5)
ON CONFLICT (root_path) DO UPDATE SET
    name = excluded.name,
    tier = excluded.tier,
    read_only = excluded.read_only,
    prunable = excluded.prunable,
    is_active = 1,
    updated_at = unixepoch()
RETURNING id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at;

-- name: SetStorageLocationActive :exec
-- Backs M6: storage.LoadGuard calls this to deactivate a location whose
-- root_path can't be resolved at startup (mount vanished) rather than
-- treating that as a fatal error that prevents the whole server from
-- booting -- see LoadGuard's doc comment.
UPDATE storage_locations SET is_active = ?2, updated_at = unixepoch() WHERE id = ?1;

-- name: DeactivateStorageLocationsNotIn :execrows
-- Backs M6: after seeding every location config.yaml currently lists,
-- deactivate any PREVIOUSLY active location whose root_path is no longer
-- among them -- an operator removing a location from config, rather than
-- it merely failing to resolve, is a deliberate decision that should also
-- self-heal storage-health/UI state without a manual DB edit. MUST NOT be
-- called with an empty currentRootPaths (cmd/branchdam guards this):
-- json_each(?1) returns ZERO rows for both NULL and '[]' (verified against
-- SQLite directly), which makes `root_path NOT IN (SELECT ... FROM
-- json_each(?1))` true for EVERY row -- the opposite failure direction
-- from MarkUnseenNodesMissing's empty-array gotcha (that one silently
-- matches nothing; this one would silently deactivate every location in
-- the database on a misconfigured or empty config.yaml).
UPDATE storage_locations
SET is_active = 0, updated_at = unixepoch()
WHERE is_active = 1
  AND root_path NOT IN (SELECT value FROM json_each(?1));

-- name: GetStorageLocationByID :one
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
FROM storage_locations
WHERE id = ?1;

-- name: GetStorageLocationByPath :one
-- Used by storage.Guard (PR 2) to resolve a canonicalized path to its tier --
-- the single source of truth for tier is this table, never a hardcoded
-- prefix. See docs/schema.md fix #1.
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
FROM storage_locations
WHERE root_path = ?1;

-- name: ListNodeCountsByLocation :many
SELECT storage_location_id, COUNT(*) AS node_count
FROM media_nodes
WHERE lifecycle_state != 'ARCHIVED'
GROUP BY storage_location_id;
