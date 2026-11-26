-- user_pats: per-user PATs for unattended operations. See
-- 00033_user_pats.sql's header comment for the rationale and storage
-- shape. Admin scopes stay behind RequirePAT; an MCP-only token
-- (every scope starts with "mcp:") may belong to a live non-admin
-- and is still rejected on every non-/mcp route.
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
-- hashed_key makes this an indexed lookup. Returns no row when nothing
-- matches (caller distinguishes miss from error via sql.ErrNoRows).
--
-- The JOIN on users is authority enforcement, not display. The WHERE
-- keeps a live owner (disabled_at IS NULL) and either a live admin
-- (is_admin = 1) or an MCP-only token: a non-empty scopes_json array
-- whose every element matches the case-sensitive glob 'mcp:*' (the
-- same rule as auth.MCPOnlyScopes / strings.HasPrefix(scope, "mcp:")).
-- Demoting an admin therefore invalidates their non-MCP tokens on the
-- next request; disabling the owner invalidates every token, MCP or
-- not. A token that fails the check surfaces as sql.ErrNoRows, which
-- the middleware turns into 401, identical to a revoked token.
-- Lookup repeats the admin-or-MCP-only check so a query change cannot
-- silently widen it.
--
-- owner_username / owner_email / owner_is_admin let the PAT middleware
-- build a Principal and a LocalUserView without a second query.
-- Attribution itself keys on user_id, never on the display fields.
SELECT up.id AS id, up.user_id AS user_id, up.name AS name, up.hashed_key AS hashed_key,
       up.scopes_json AS scopes_json, up.created_at AS created_at, up.last_used_at AS last_used_at,
       up.expires_at AS expires_at, up.revoked_at AS revoked_at,
       u.username AS owner_username, u.email AS owner_email,
       u.is_admin AS owner_is_admin
FROM user_pats AS up
JOIN users AS u ON u.id = up.user_id
WHERE up.hashed_key = ?1 AND up.revoked_at IS NULL
  AND u.disabled_at IS NULL
  AND (
    u.is_admin = 1
    OR (
      json_array_length(up.scopes_json) > 0
      AND NOT EXISTS (
        SELECT 1 FROM json_each(up.scopes_json) AS scope
        WHERE scope.value NOT GLOB 'mcp:*'
      )
    )
  );

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

-- name: RevokeLiveBootstrapPATs :execrows
-- Crash-recovery hygiene, run inside the bootstrap mint transaction:
-- a previous boot killed between the commit and the file write leaves
-- a live name='bootstrap' wildcard row whose plaintext never reached
-- disk; the next boot's re-mint would otherwise accumulate live
-- bootstrap tokens forever (round-5 review). Revoking prior live
-- bootstrap rows for this user in the same tx keeps exactly one
-- live bootstrap token at any time. Scoped to the bootstrap user +
-- the reserved name so an admin's own 'bootstrap'-named token on a
-- different account is untouched.
UPDATE user_pats
SET revoked_at = COALESCE(revoked_at, unixepoch())
WHERE user_id = ?1 AND name = 'bootstrap' AND revoked_at IS NULL;

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
