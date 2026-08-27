-- +goose Up
-- app_settings backs UI-configurable overrides on top of config.yaml/.env
-- (docs/configuration.md's precedence section). A row's mere existence IS
-- the override -- an empty string is a valid, deliberate override (e.g.
-- "disable Immich from the UI" must beat a populated ${IMMICH_API_URL}), so
-- this table is never merged on "non-empty wins", only on "row present".
-- No triggers, no FKs, no cascade -- same invariants as every other table
-- here (see docs/schema.md fixes #4/#5/#6). updated_at is set explicitly in
-- every query.
CREATE TABLE app_settings (
    key         TEXT    PRIMARY KEY,
    value       TEXT    NOT NULL,   -- JSON-encoded scalar/list; secrets hold "v1:<base64(nonce||ciphertext)>"
    is_secret   INTEGER NOT NULL DEFAULT 0 CHECK (is_secret IN (0, 1)),
    updated_at  INTEGER NOT NULL,
    updated_by  TEXT    NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE IF EXISTS app_settings;
