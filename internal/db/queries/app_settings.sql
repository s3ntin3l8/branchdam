-- name: ListAppSettings :many
-- The resolver (internal/settings) loads the whole table once at startup
-- and on every PUT; the table is expected to stay small (one row per
-- overridden field, never one row per node), so no pagination.
SELECT key, value, is_secret, updated_at, updated_by
FROM app_settings
ORDER BY key;

-- name: UpsertAppSetting :one
-- The row's mere existence is the override -- callers pass "" for value
-- exactly when the operator wants "explicitly empty", never as a signal to
-- skip the write. See internal/settings/resolver.go for why an empty
-- override still beats a populated config/env base value. updated_at is
-- set via unixepoch() here, not passed from Go, matching every other
-- UPDATE/upsert in this package (docs/schema.md fix #5).
INSERT INTO app_settings (key, value, is_secret, updated_at, updated_by)
VALUES (?1, ?2, ?3, unixepoch(), ?4)
ON CONFLICT (key) DO UPDATE SET
    value = excluded.value,
    is_secret = excluded.is_secret,
    updated_at = unixepoch(),
    updated_by = excluded.updated_by
RETURNING key, value, is_secret, updated_at, updated_by;

-- name: DeleteAppSetting :exec
-- "Revert to config" deletes the row -- this is what makes provenance
-- recoverable rather than merely resettable to some other stored value.
DELETE FROM app_settings WHERE key = ?1;
