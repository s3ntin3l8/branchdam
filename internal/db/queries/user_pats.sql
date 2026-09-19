-- user_pats: per-user admin PATs for unattended operations. See
-- 00033_user_pats.sql's header comment for the rationale and storage
-- shape. All queries here are admin-gated -- RequirePAT middleware
-- sits on the route and rejects without a valid+scoped token before
-- the handler runs.
--
-- All positional params use bare ?1/?2 (not sqlc.arg) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: CreateUserPAT :one
-- Mints a new PAT. hashed_key is HMAC-SHA256(pepper, plaintext);
-- the caller has the plaintext to return to the operator exactly
-- once and never persists. scopes_json is JSON-encoded by the Go
-- layer; SQLite's json_valid CHECK on the column protects against
-- malformed storage. expires_at is nullable for non-expiring tokens.
INSERT INTO user_pats (user_id, name, hashed_key, scopes_json, created_at, expires_at)
VALUES (?1, ?2, ?3, ?4, unixepoch(), ?5)
RETURNING id, user_id, name, hashed_key, scopes_json, created_at, last_used_at, expires_at, revoked_at;

-- name: GetUserPATByHash :one
-- Hot path: called on every authenticated PAT request. UNIQUE on
-- hashed_key makes this an indexed lookup. Returns NULL when no row
-- matches (caller distinguishes miss from error via the sql.ErrNoRows
-- sentinel).
--
-- The JOIN on users is authority enforcement, not display: the WHERE
-- requires the owner to be a live admin (is_admin = 1, disabled_at
-- NULL), so demoting or disabling the owner invalidates every one of
-- their tokens on the next request instead of freezing admin
-- authority at mint time. A token whose owner fails the check
-- surfaces as sql.ErrNoRows -> 401, identical to a revoked token.
SELECT up.id AS id, up.user_id AS user_id, up.name AS name, up.hashed_key AS hashed_key,
       up.scopes_json AS scopes_json, up.created_at AS created_at, up.last_used_at AS last_used_at,
       up.expires_at AS expires_at, up.revoked_at AS revoked_at
FROM user_pats AS up
JOIN users AS u ON u.id = up.user_id
WHERE up.hashed_key = ?1 AND up.revoked_at IS NULL
  AND u.is_admin = 1 AND u.disabled_at IS NULL;

-- name: ListUserPATs :many
-- Backs GET /api/v1/users/me/pats -- lists all the calling user's
-- PATs, including revoked (so the UI can show "last used 3 days ago,
-- revoked yesterday" history). Plaintext is never stored, so the
-- "list" response has nothing to redact -- just metadata.
SELECT id, user_id, name, hashed_key, scopes_json, created_at, last_used_at, expires_at, revoked_at
FROM user_pats
WHERE user_id = ?1
ORDER BY created_at DESC
LIMIT ?2 OFFSET ?3;

-- name: CountUserPATs :one
SELECT COUNT(*) FROM user_pats WHERE user_id = ?1;

-- name: RevokeUserPAT :execrows
-- Soft-delete by setting revoked_at. The WHERE keeps it to live rows
-- owned by the calling user, so the affected-row count is
-- meaningful: 1 = revoked, 0 = the id doesn't exist, isn't live, or
-- belongs to another user -- the caller maps 0 to a 404 instead of
-- reporting a silent false success.
UPDATE user_pats
SET revoked_at = COALESCE(revoked_at, unixepoch())
WHERE id = ?1 AND user_id = ?2 AND revoked_at IS NULL;

-- name: RevokeUserPATByHash :execrows
-- Bootstrap orphan cleanup: if the plaintext file write fails after
-- the mint transaction committed, the row is a live non-expiring
-- wildcard PAT no one can ever present. Revoke it (soft-delete, not
-- a hard delete -- the no-delete audit invariant holds) so the DB
-- matches reality. Keyed by hashed_key because the bootstrap path
-- knows the hash, not the row id.
UPDATE user_pats
SET revoked_at = COALESCE(revoked_at, unixepoch())
WHERE hashed_key = ?1 AND revoked_at IS NULL;

-- name: TouchUserPAT :exec
-- Best-effort last_used_at bump on every authenticated request. The
-- auth path calls this async (go func() + background write); a flush
-- failure is logged but never propagates back to the request -- the
-- operator-visible cost of an auth path is one indexed PK lookup,
-- not a write. The WHERE last_used_at IS NULL OR last_used_at < ?3
-- throttles the writes: only the first request in any 60s window
-- actually mutates the row, so a flood of requests from one token
-- doesn't generate a flood of disk writes. Keyed by hashed_key so
-- the middleware can call this without a second lookup.
UPDATE user_pats
SET last_used_at = ?2
WHERE hashed_key = ?1 AND (last_used_at IS NULL OR last_used_at < ?2 - 60);
