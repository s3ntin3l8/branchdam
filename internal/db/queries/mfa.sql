-- MFA queries. All positional params use bare ?1/?2 per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: GetMFACredentials :one
SELECT user_id, secret_encrypted, algo, digits, period, last_used_step, recovery_code_salt
FROM mfa_credentials
WHERE user_id = ?1;

-- name: UpsertMFACredentials :exec
INSERT INTO mfa_credentials (user_id, secret_encrypted, algo, digits, period, last_used_step, recovery_code_salt)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
ON CONFLICT (user_id) DO UPDATE SET
    secret_encrypted = excluded.secret_encrypted,
    algo = excluded.algo,
    digits = excluded.digits,
    period = excluded.period,
    last_used_step = excluded.last_used_step,
    recovery_code_salt = excluded.recovery_code_salt;

-- name: DeleteMFACredentials :exec
DELETE FROM mfa_credentials WHERE user_id = ?1;

-- name: UpdateLastUsedStep :exec
UPDATE mfa_credentials SET last_used_step = ?2 WHERE user_id = ?1;

-- name: InsertRecoveryCodes :exec
INSERT INTO mfa_recovery_codes (user_id, code_hash)
VALUES (?1, ?2);

-- name: FindUnusedRecoveryCode :one
SELECT id, code_hash
FROM mfa_recovery_codes
WHERE user_id = ?1 AND code_hash = ?2 AND used_at IS NULL
LIMIT 1;

-- name: MarkRecoveryCodeUsed :exec
UPDATE mfa_recovery_codes SET used_at = ?2 WHERE id = ?1;

-- name: DeleteRecoveryCodes :exec
DELETE FROM mfa_recovery_codes WHERE user_id = ?1;

-- name: CountRecoveryCodesForUser :one
-- Used by tests to assert mfa_recovery_codes rows are gone after
-- password reset (Issue 10). Returns 0 when the user has no rows.
SELECT COUNT(*) FROM mfa_recovery_codes WHERE user_id = ?1;

-- name: SetMFAPendingSecret :exec
UPDATE users SET mfa_pending_secret = ?2, mfa_pending_secret_created_at = ?3 WHERE id = ?1;

-- name: GetMFAPendingSecret :one
SELECT id, mfa_pending_secret, mfa_pending_secret_created_at FROM users WHERE id = ?1;

-- name: ClearMFAPendingSecret :exec
UPDATE users SET mfa_pending_secret = NULL, mfa_pending_secret_created_at = NULL WHERE id = ?1;

-- name: SetSessionMFAVerified :exec
UPDATE sessions SET mfa_verified_at = ?2 WHERE id = ?1;

-- name: GetSessionWithMFA :one
SELECT id, cookie_id, user_id, created_at, last_seen_at, expires_at, idle_expires_at, ip, user_agent, revoked_at, mfa_verified_at
FROM sessions
WHERE cookie_id = ?1;
