-- name: CreateStorageLocation :one
INSERT INTO storage_locations (name, root_path, tier, read_only, prunable)
VALUES (?1, ?2, ?3, ?4, ?5)
RETURNING id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at;

-- name: ListStorageLocations :many
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
FROM storage_locations
ORDER BY id;

-- name: GetStorageLocationByPath :one
-- Used by storage.Guard (PR 2) to resolve a canonicalized path to its tier --
-- the single source of truth for tier is this table, never a hardcoded
-- prefix. See docs/schema.md fix #1.
SELECT id, name, root_path, tier, read_only, prunable, is_active, created_at, updated_at
FROM storage_locations
WHERE root_path = ?1;
