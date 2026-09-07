-- +goose Up
-- +goose StatementBegin
--
-- password_reset_tokens: single-use tokens minted by either the
-- self-service /api/v1/password-reset/request endpoint or the admin
-- /api/v1/admin/users/{id}/reset-password endpoint. Tokens are 32
-- random bytes; only token_hash (sha256) is stored at rest. The
-- plaintext token surfaces to the operator via slog.WARN and the
-- admin-UI "Pending resets" panel -- not in the database.
--
-- Single-use is enforced by the partial unique index
-- password_reset_tokens_active_uniq, which only covers rows where
-- used_at IS NULL. The /confirm handler attempts an atomic CAS
-- UPDATE...SET used_at = ? WHERE id = ? AND used_at IS NULL; a second
-- attempt matches zero rows and returns 404.
--
-- Per AGENTS.md invariant 1: every FK is RESTRICT. Deleting a user
-- with outstanding reset tokens is refused at the database level; the
-- admin must explicitly revoke outstanding tokens first. This is
-- deliberate: orphan tokens are easier to reason about than a
-- silently-cleared used_at.

CREATE TABLE password_reset_tokens (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    token_hash  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    created_by  TEXT    NOT NULL,  -- 'self-service' or 'admin:<username>'
    CHECK (length(token_hash) = 64),  -- sha256 hex
    CHECK (length(created_by) <= 64)
);

-- Active (un-consumed, un-expired) tokens are unique per (user, hash);
-- the partial index narrows the conflict surface to the small set of
-- in-flight rows. /request can race on the same email's user and the
-- second INSERT will conflict on this index; the handler treats that
-- as success (the user already has a pending token).
CREATE UNIQUE INDEX password_reset_tokens_active_uniq
    ON password_reset_tokens(user_id, token_hash) WHERE used_at IS NULL;

-- Cleanup sweep: list-by-user + list-active queries hit this index.
CREATE INDEX password_reset_tokens_user_idx
    ON password_reset_tokens(user_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS password_reset_tokens_user_idx;
DROP INDEX IF EXISTS password_reset_tokens_active_uniq;
DROP TABLE IF EXISTS password_reset_tokens;
-- +goose StatementEnd
