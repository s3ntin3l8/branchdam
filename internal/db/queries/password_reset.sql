-- name: CreatePasswordResetToken :one
INSERT INTO password_reset_tokens (
    user_id, token_hash, created_at, expires_at, created_by
) VALUES (
    ?1, ?2, ?3, ?4, ?5
)
RETURNING id, user_id, token_hash, created_at, expires_at, used_at, created_by;

-- name: ConsumePasswordResetToken :one
-- Atomic single-use consume. The WHERE used_at IS NULL guard is the
-- whole point: a second attempt with the same token id matches zero
-- rows, and the caller treats that as "token already consumed". The
-- expires_at < ?3 guard means an expired token fails the same way
-- (no row, no error), so a stolen-expired-token doesn't reveal that
-- it was once valid.
UPDATE password_reset_tokens
SET used_at = ?3
WHERE id = ?1
  AND used_at IS NULL
  AND expires_at > ?2
RETURNING id, user_id, token_hash, created_at, expires_at, used_at, created_by;

-- name: ListActivePasswordResetTokens :many
-- "Pending" panel: list of un-consumed, un-expired tokens for a given
-- user. The /admin/users/{id}/resets endpoint will use this in #408.
SELECT id, user_id, token_hash, created_at, expires_at, used_at, created_by
FROM password_reset_tokens
WHERE user_id = ?1
  AND used_at IS NULL
  AND expires_at > ?2
ORDER BY created_at DESC;

-- name: ListAllActivePasswordResetTokens :many
-- Global pending list -- used by the admin-UI pending-resets panel
-- (PR #408). The query is intentionally cheap: the
-- password_reset_tokens_user_idx covers user_id + created_at, and the
-- partial active unique covers used_at IS NULL. With a homelab user
-- count this is < 100 rows; production-scale is a follow-up.
SELECT id, user_id, token_hash, created_at, expires_at, used_at, created_by
FROM password_reset_tokens
WHERE used_at IS NULL
  AND expires_at > ?1
ORDER BY created_at DESC
LIMIT ?2 OFFSET ?3;

-- name: RevokePasswordResetToken :exec
-- Admin "delete" / revoke. Idempotent: revoking an already-used or
-- already-expired token matches the row but is a no-op.
UPDATE password_reset_tokens
SET used_at = ?2
WHERE id = ?1
  AND used_at IS NULL;

-- name: GetPasswordResetTokenByHash :one
-- Confirm-time lookup: scan the active-token index for a specific
-- token_hash. The active-partial unique index keeps the candidate
-- set small. The handler iterates and finds the row whose hash
-- matches -- the index narrows the scan to the small set of
-- in-flight tokens, not the entire history.
SELECT id, user_id, token_hash, created_at, expires_at, used_at, created_by
FROM password_reset_tokens
WHERE token_hash = ?1
  AND used_at IS NULL
  AND expires_at > ?2
LIMIT 1;
