-- +goose Up
-- +goose StatementBegin
--
-- MFA / TOTP support (issue #410). Two new tables:
--
--   mfa_credentials: one row per user who has enrolled TOTP. The
--   secret is envelope-encrypted via internal/secrets.Box (v1:<base64>).
--   last_used_step prevents TOTP replay within the same 30s window.
--
--   mfa_recovery_codes: 8 single-use backup codes per enrollment.
--   Partial unique index on (user_id, code_hash) WHERE used_at IS NULL
--   ensures each unused code is unique; the application layer marks
--   codes used atomically.
--
-- Also extends login_audit with 'mfa-challenge' source and MFA-specific
-- outcomes. Uses the standard SQLite rename-copy-drop pattern for CHECK
-- constraint changes (immutable in SQLite).

CREATE TABLE mfa_credentials (
    user_id          INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE RESTRICT,
    secret_encrypted TEXT    NOT NULL,
    algo             TEXT    NOT NULL DEFAULT 'SHA1',
    digits           INTEGER NOT NULL DEFAULT 6,
    period           INTEGER NOT NULL DEFAULT 30,
    last_used_step   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE mfa_recovery_codes (
    id        INTEGER PRIMARY KEY,
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    code_hash TEXT    NOT NULL,
    used_at   INTEGER
);
CREATE UNIQUE INDEX mfa_recovery_codes_unique_unused ON mfa_recovery_codes(user_id, code_hash) WHERE used_at IS NULL;
CREATE INDEX mfa_recovery_codes_user_idx ON mfa_recovery_codes(user_id);

-- Extend login_audit source CHECK to include 'mfa-challenge'.
ALTER TABLE login_audit RENAME TO login_audit_old;

CREATE TABLE login_audit (
    id                  INTEGER PRIMARY KEY,
    user_id             INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    username_presented  TEXT    NOT NULL,
    source              TEXT    NOT NULL
        CHECK (source IN (
            'local',
            'forward-jit-create',
            'forward-noop',
            'password-reset',
            'mfa-challenge'
        )),
    outcome             TEXT    NOT NULL
        CHECK (outcome IN (
            'ok',
            'bad-password',
            'rate-limited',
            'user-disabled',
            'no-such-user',
            'user-locked',
            'mfa-required',
            'mfa-invalid',
            'mfa-recovery-used'
        )),
    ip                  TEXT    NOT NULL,
    user_agent          TEXT    NOT NULL,
    details             TEXT    NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL,
    CHECK (length(ip) <= 64),
    CHECK (length(username_presented) <= 255),
    CHECK (length(user_agent) <= 512)
);

INSERT INTO login_audit (id, user_id, username_presented, source, outcome, ip, user_agent, details, created_at)
SELECT id, user_id, username_presented, source, outcome, ip, user_agent, details, created_at
FROM login_audit_old;

DROP TABLE login_audit_old;

CREATE INDEX login_audit_user_time_idx ON login_audit(user_id, created_at DESC);
CREATE INDEX login_audit_ip_time_idx ON login_audit(ip, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE login_audit RENAME TO login_audit_old;

CREATE TABLE login_audit (
    id                  INTEGER PRIMARY KEY,
    user_id             INTEGER REFERENCES users(id) ON DELETE RESTRICT,
    username_presented  TEXT    NOT NULL,
    source              TEXT    NOT NULL
        CHECK (source IN (
            'local',
            'forward-jit-create',
            'forward-noop',
            'password-reset'
        )),
    outcome             TEXT    NOT NULL
        CHECK (outcome IN (
            'ok',
            'bad-password',
            'rate-limited',
            'user-disabled',
            'no-such-user',
            'user-locked'
        )),
    ip                  TEXT    NOT NULL,
    user_agent          TEXT    NOT NULL,
    details             TEXT    NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL,
    CHECK (length(ip) <= 64),
    CHECK (length(username_presented) <= 255),
    CHECK (length(user_agent) <= 512)
);

INSERT INTO login_audit (id, user_id, username_presented, source, outcome, ip, user_agent, details, created_at)
SELECT id, user_id, username_presented, source, outcome, ip, user_agent, details, created_at
FROM login_audit_old
WHERE source IN ('local', 'forward-jit-create', 'forward-noop', 'password-reset');

DROP TABLE login_audit_old;

CREATE INDEX login_audit_user_time_idx ON login_audit(user_id, created_at DESC);
CREATE INDEX login_audit_ip_time_idx ON login_audit(ip, created_at DESC);

DROP INDEX mfa_recovery_codes_user_idx;
DROP INDEX mfa_recovery_codes_unique_unused;
DROP TABLE mfa_recovery_codes;
DROP TABLE mfa_credentials;
-- +goose StatementEnd
