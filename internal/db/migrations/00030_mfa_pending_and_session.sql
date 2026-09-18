-- +goose Up
-- +goose StatementBegin
--
-- MFA session support: mfa_verified_at on sessions (nullable = half-
-- authenticated) and mfa_pending_secret on users (nullable, set during
-- TOTP setup, cleared on enable or timeout).

ALTER TABLE sessions ADD COLUMN mfa_verified_at INTEGER;
ALTER TABLE users ADD COLUMN mfa_pending_secret TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN mfa_pending_secret;
ALTER TABLE sessions DROP COLUMN mfa_verified_at;
-- +goose StatementEnd
