-- +goose Up
-- +goose StatementBegin
--
-- Extend the login_audit.source CHECK to include 'password-reset' so
-- the password-reset flow can write audit rows in the same table as
-- the rest of the auth surface. Per the design doc (PR #409 plan,
-- section "Audit fit"), one table is preferred over a parallel
-- password_reset_audit because operators querying "what happened to
-- this user" hit a single index.
--
-- SQLite CHECK constraints are immutable: ALTER TABLE ... ALTER COLUMN
-- ... DROP CONSTRAINT is not supported. The dance is the standard
-- SQLite migration pattern: rename the old table, create the new
-- table with the updated CHECK, copy the data over, drop the old
-- table. Per AGENTS.md invariant 1, no triggers / no CASCADE -- the
-- rename-then-insert-then-drop happens in a single transaction
-- implicitly because goose wraps each migration in BEGIN/COMMIT.
--
-- This migration must run BEFORE migration 00022_login_audit_admin_source.sql
-- (which will extend the same CHECK with 'admin-action') and 00023
-- (which extends it with 'mfa-challenge' and the outcome enum).
-- Migration numbers are sequential by merge order, not by issue number.

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
FROM login_audit_old;

DROP TABLE login_audit_old;

-- Recreate the indexes that were dropped with the old table.
CREATE INDEX login_audit_user_time_idx ON login_audit(user_id, created_at DESC);
CREATE INDEX login_audit_ip_time_idx ON login_audit(ip, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse the dance. Note: any rows that were written with the new
-- 'password-reset' source value will fail the recreated old CHECK
-- constraint; the migration is intentionally one-way if those rows
-- exist. Real down-migrations are a recovery tool, not a feature.
ALTER TABLE login_audit RENAME TO login_audit_old;

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

INSERT INTO login_audit (id, user_id, username_presented, source, outcome, ip, user_agent, details, created_at)
SELECT id, user_id, username_presented, source, outcome, ip, user_agent, details, created_at
FROM login_audit_old
WHERE source IN ('local', 'forward-jit-create', 'forward-noop');

DROP TABLE login_audit_old;

CREATE INDEX login_audit_user_time_idx ON login_audit(user_id, created_at DESC);
CREATE INDEX login_audit_ip_time_idx ON login_audit(ip, created_at DESC);
-- +goose StatementEnd
