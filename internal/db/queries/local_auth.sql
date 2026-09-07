-- Local auth queries. The handlers in internal/httpapi/local_auth.go
-- and the middleware in internal/auth/session.go both go through these --
-- no raw SQL outside this file (per the project's sqlc convention,
-- documented in CONTRIBUTING.md).
--
-- All positional params use bare ?1/?2/?3 (not sqlc.arg(name)) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: CountUsers :one
-- Returns the total number of users. /setup/status returns readyForSetup:
-- (count == 0) so the SPA can branch between setup form and login form.
-- Cheap (no WHERE), single indexed-ish scan; called on every unauthenticated
-- request that hits the SPA shell, so kept O(1)-ish.
SELECT COUNT(*) FROM users;

-- name: GetUserByID :one
SELECT id, username, email, password_hash, is_admin, source, created_at, created_by, disabled_at
FROM users
WHERE id = ?1;

-- name: GetUserByUsername :one
-- Used by /api/v1/login to resolve the presented username to a user row.
-- The lookup is username-only; password verification happens in Go against
-- password_hash. Index: users.username UNIQUE already covers this.
SELECT id, username, email, password_hash, is_admin, source, created_at, created_by, disabled_at
FROM users
WHERE username = ?1;

-- name: GetUserByEmailSource :one
-- Used by the JIT provisioning path: a forward-auth request with email E
-- and source 'forward-jit' either matches an existing admin or triggers
-- a fresh INSERT in CreateForwardJITUser. Partial unique index
-- users_email_source_uniq covers this.
SELECT id, username, email, password_hash, is_admin, source, created_at, created_by, disabled_at
FROM users
WHERE email = ?1 AND source = ?2;

-- name: CreateLocalUser :one
-- Inserts a source='local' user. password_hash is the argon2id encoded
-- string. created_by is 'setup' for the first admin, 'self' for self-
-- registration flows (none in v1), or 'user:<principal name>' for admin-
-- created users. Returns the inserted row.
INSERT INTO users (
    username, email, password_hash, is_admin, source, created_at, created_by
) VALUES (
    ?1, ?2, ?3, ?4, 'local', ?5, ?6
)
RETURNING id, username, email, password_hash, is_admin, source, created_at, created_by, disabled_at;

-- name: CreateForwardJITUser :one
-- Inserts a source='forward-jit' user with password_hash = NULL. The
-- schema CHECK constraint enforces this. email is required (callers
-- refuse the JIT when the forward-auth email header is empty). created_by is
-- 'forward:<forward-auth username>'.
INSERT INTO users (
    username, email, password_hash, is_admin, source, created_at, created_by
) VALUES (
    ?1, ?2, NULL, ?3, 'forward-jit', ?4, ?5
)
RETURNING id, username, email, password_hash, is_admin, source, created_at, created_by, disabled_at;

-- name: DisableUser :exec
-- Sets disabled_at. Idempotent. Does NOT revoke existing sessions --
-- that's a separate admin action (RevokeAllUserSessions) so an admin can
-- disable future logins without immediately logging the user out.
UPDATE users SET disabled_at = ?2 WHERE id = ?1;

-- name: ListUsers :many
-- Paginated user list for the admin UI. Order by id ASC so paging is
-- stable across inserts (new users go to the END, not the middle).
SELECT id, username, email, password_hash, is_admin, source, created_at, created_by, disabled_at
FROM users
ORDER BY id ASC
LIMIT ?1 OFFSET ?2;

-- name: CreateSession :one
-- Inserts a new session row. cookie_id is 32 random bytes hex-encoded
-- (length 64, CHECK-enforced). The HMAC tag over cookie_id is computed
-- at cookie-write time, NOT stored here -- sessions.cookie_id alone is
-- sufficient for lookup, the HMAC is for tamper detection (rejected by
-- the middleware before the DB lookup).
INSERT INTO sessions (
    cookie_id, user_id, created_at, last_seen_at, expires_at,
    idle_expires_at, ip, user_agent
) VALUES (
    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8
)
RETURNING id, cookie_id, user_id, created_at, last_seen_at, expires_at, idle_expires_at, ip, user_agent, revoked_at;

-- name: GetSessionByCookieID :one
-- Hot path: called by SessionMiddleware on every authenticated browser
-- request. Returns the full row including revoked_at so the middleware
-- can reject post-revoke cookies in one query. The partial index
-- sessions_user_active_idx covers the active-set variant but the
-- revoke check needs the full row, so we don't use it here.
SELECT id, cookie_id, user_id, created_at, last_seen_at, expires_at, idle_expires_at, ip, user_agent, revoked_at
FROM sessions
WHERE cookie_id = ?1;

-- name: TouchSession :exec
-- Updates last_seen_at and idle_expires_at. Called by SessionMiddleware
-- after a successful auth (sliding idle window). One statement per
-- request, but the WHERE matches the active-set partial index path
-- already loaded above so the planner is happy.
UPDATE sessions
SET last_seen_at = ?2, idle_expires_at = ?3
WHERE id = ?1 AND revoked_at IS NULL;

-- name: RevokeSession :exec
-- Sets revoked_at on a single session (used by DELETE /api/v1/session).
UPDATE sessions SET revoked_at = ?2 WHERE id = ?1;

-- name: RevokeAllUserSessions :exec
-- Used by admin "log out everywhere" action and by DisableUser's
-- companion flow (future admin endpoint). Idempotent.
UPDATE sessions SET revoked_at = ?2 WHERE user_id = ?1 AND revoked_at IS NULL;

-- name: InsertLoginAudit :exec
-- Append-only. The two indexes on (user_id, created_at) and (ip, created_at)
-- cover the admin UI's tail queries without a sort.
INSERT INTO login_audit (
    user_id, username_presented, source, outcome, ip, user_agent, details, created_at
) VALUES (
    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8
);
