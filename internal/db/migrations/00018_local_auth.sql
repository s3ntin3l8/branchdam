-- +goose Up
-- Local auth: password-hashed users + server-side sessions + login audit.
--
-- Four tables, all FK RESTRICT and no triggers (same invariants as every
-- other table here, see docs/schema.md fixes #4/#5/#6 and AGENTS.md
-- invariant #1). All timestamps Unix seconds to match the rest of the
-- schema.
--
-- `users` holds one row per local-auth identity. Three sources:
--   source='local'       -- created via /setup/admin or admin endpoint
--   source='forward-jit' -- JIT-provisioned from a forward-auth admin group
--                          (see internal/auth/users/jit.go); never has a
--                          password (cannot authenticate via /login)
--   source='forward-link'-- reserved for an explicit-link flow not built
--                          in v1; column present so the schema doesn't
--                          need a migration when that lands
-- username is UNIQUE within the table. The local/forward-jit username
-- collision is handled at write time in users.CreateLocal (a forward-jit
-- write falls back to email-local-part when the asserted username is
-- already a local user) -- see internal/auth/users/jit.go's doc comment.
-- password_hash is argon2id encoded ($argon2id$...) and always present
-- for source='local' (CHECK constraint), always NULL for source='forward-jit'
-- (also CHECK). email is UNIQUE per (source, email) so a local
-- "alice@example.com" and a forward-jit "alice@example.com" can coexist
-- (they are different identities from the auth modeler's perspective --
-- forward-jit identifies by email, local by username+password). is_admin
-- is an explicit local override for the existing authz.groups config --
-- IsAdmin(p, allowedGroups, localUserView) consults both, see internal/auth/authz.go.
-- disabled_at is NULL while the account can authenticate; setting it
-- refuses every login without deleting the row (audit trail preserved).
--
-- `sessions` is the server-side session table. The browser cookie holds
-- a cookie_id (32 random bytes hex) and an HMAC tag over it; the cookie
-- value is opaque to the client. Every authenticated request loads the
-- session row, refreshes last_seen_at/idle_expires_at on success, and
-- refuses the request if revoked_at is set or either expiry has passed.
-- Absolute expiry (created_at + absoluteTimeout) is never extended --
-- a 30-day cookie lifetime is the cap even if the user clicks daily.
-- The partial index covers the active-set hot path: KeyLookup and
-- /api/v1/me's session-cookie reader only ever see WHERE-revoked_at-IS-NULL
-- rows, never the revoked history.
--
-- `login_audit` is an append-only log of authentication events. user_id
-- is NULL on failed logins for unknown usernames (so we don't leak the
-- fact that the username doesn't exist). source is 'local' for /login
-- POSTs and 'forward-jit-create' for the rare case where a forward-auth
-- request triggers JIT provisioning (no audit on every successful
-- forward-auth request -- only the provisioning event, since forwarding
-- itself is Authentik's job). outcome captures the result class. Two
-- indexes cover the two read patterns: tail-by-user (admin UI's user-
-- detail panel) and tail-by-IP (admin UI's rate-limit-investigation
-- panel).
--
-- password_reset_tokens is NOT created in v1. The admin-reset-password
-- flow lands in a follow-up PR; admin can rotate a user's password via
-- direct SQL in the meantime. Adding it later is a pure additive
-- migration (00019_password_reset.sql) with no schema conflict here.

CREATE TABLE users (
    id              INTEGER PRIMARY KEY,
    username        TEXT    NOT NULL UNIQUE,
    email           TEXT,
    password_hash   TEXT,
    is_admin        INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0,1)),
    source          TEXT    NOT NULL DEFAULT 'local'
        CHECK (source IN ('local', 'forward-jit', 'forward-link')),
    created_at      INTEGER NOT NULL,
    created_by      TEXT    NOT NULL,
    disabled_at     INTEGER,
    CHECK ((source = 'local' AND password_hash IS NOT NULL)
        OR (source IN ('forward-jit', 'forward-link') AND password_hash IS NULL)),
    CHECK (disabled_at IS NULL OR disabled_at >= created_at)
);

CREATE UNIQUE INDEX users_email_source_uniq ON users(email, source) WHERE email IS NOT NULL;

CREATE TABLE sessions (
    id              INTEGER PRIMARY KEY,
    cookie_id       TEXT    NOT NULL UNIQUE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at      INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    idle_expires_at INTEGER NOT NULL,
    ip              TEXT    NOT NULL,
    user_agent      TEXT    NOT NULL,
    revoked_at      INTEGER,
    CHECK (length(cookie_id) = 64),
    CHECK (length(ip) <= 64),
    CHECK (expires_at >= created_at),
    CHECK (idle_expires_at >= last_seen_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE INDEX sessions_user_active_idx ON sessions(user_id)
    WHERE revoked_at IS NULL;

CREATE TABLE login_audit (
    id                  INTEGER PRIMARY KEY,
    user_id             INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    username_presented  TEXT    NOT NULL,
    source              TEXT    NOT NULL
        CHECK (source IN ('local', 'forward-jit-create', 'forward-noop')),
    outcome             TEXT    NOT NULL
        CHECK (outcome IN ('ok', 'bad-password', 'rate-limited', 'user-disabled', 'no-such-user', 'user-locked')),
    ip                  TEXT    NOT NULL,
    user_agent          TEXT    NOT NULL,
    details             TEXT    NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL,
    CHECK (length(ip) <= 64),
    CHECK (length(username_presented) <= 255),
    CHECK (length(user_agent) <= 512)
);

CREATE INDEX login_audit_user_time_idx ON login_audit(user_id, created_at DESC);
CREATE INDEX login_audit_ip_time_idx ON login_audit(ip, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS login_audit_ip_time_idx;
DROP INDEX IF EXISTS login_audit_user_time_idx;
DROP TABLE IF EXISTS login_audit;
DROP INDEX IF EXISTS sessions_user_active_idx;
DROP TABLE IF EXISTS sessions;
DROP INDEX IF EXISTS users_email_source_uniq;
DROP TABLE IF EXISTS users;
