-- +goose Up
-- +goose StatementBegin
--
-- MFA hardening (review feedback on PR #459).
--
--   users.mfa_pending_secret_created_at: stamped at Setup time so Enable
--   can refuse a stale pending secret past PendingSecretTTL (15 min).
--   Prior to this column the TTL constant was declared but never enforced
--   (Issue 7).
--
--   mfa_credentials.recovery_code_salt: per-user random salt used when
--   hashing the 8 recovery codes. Without a salt, the 50-bit recovery
--   codes hashed with unsalted SHA-256 are brute-forceable in seconds
--   given an offline DB dump (Issue 8). The salt is generated once at
--   Enable time and stored on the credentials row alongside the TOTP
--   secret envelope; all 8 codes for that user share the same salt.

ALTER TABLE users ADD COLUMN mfa_pending_secret_created_at INTEGER;
ALTER TABLE mfa_credentials ADD COLUMN recovery_code_salt TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE mfa_credentials DROP COLUMN recovery_code_salt;
ALTER TABLE users DROP COLUMN mfa_pending_secret_created_at;
-- +goose StatementEnd
