-- 00020_users_and_audit.sql — multi-user attribution + actor audit log.
--
-- Two new tables (actor_audit) plus three nullable FKs that record
-- per-asset / per-job / per-device attribution. All FKs are RESTRICT and
-- nullable so this migration lands safely on systems with existing data
-- (e.g. device_pairings rows that pre-date this PR have no owner yet --
-- they stay NULL until re-paired; the upload path treats NULL as "no
-- human attribution").
--
-- No triggers (AGENTS.md invariant #1). All FKs RESTRICT, not CASCADE.
-- Timestamps are unixepoch() integers, matching the rest of the schema.
--
-- Cross-references:
--   - docs/schema.md fixes #1-#9 for the schema's invariant design
--   - internal/auth/principal.go for the ExternalUID now read by
--     BrowserChain (X-Authentik-Uid)
--   - internal/users for ResolveOrCreate / SystemUserID lazy provisioning
--   - internal/audit for the actor_audit append-only log
--
-- Plan: this PR ships the schema, lazy provisioning, asset/scan/pairing
-- attribution, the "My uploads" filter, and the actor_audit log. A
-- follow-up PR hardens device_pairings.user_id to NOT NULL after a
-- audit-derived backfill (companion_pairing_audit.actor carries the
-- pairing creator's principal name for every existing pairing).

-- +goose Up

-- users: one row per human principal that has ever authenticated via
-- Authentik ForwardAuth. Lazily created on first sight by
-- internal/users.Service.ResolveOrCreate, keyed on the stable
-- (auth_provider, external_uid) so Authentik username renames don't
-- fragment attribution. Username/email are denormalized display
-- fields, refreshed on every ResolveOrCreate call (cheap, no extra
-- round trip -- they're already in the Principal).
--
-- The PR #407 migration 00018_local_auth.sql already created the
-- users table with columns: id, username, email, password_hash,
-- is_admin, source, created_at, created_by, disabled_at.
-- This migration adds the attribution columns and the unique index.
-- See docs/forward-auth.md for the X-Authentik-Uid header mapping.

ALTER TABLE users
    ADD COLUMN auth_provider TEXT NOT NULL DEFAULT 'authentik';

ALTER TABLE users
    ADD COLUMN external_uid TEXT NOT NULL DEFAULT '';

ALTER TABLE users
    ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT (unixepoch());

-- Backfill external_uid for existing rows (use username as a stable proxy
-- until the user re-authenticates and ResolveOrCreate updates it).
--
-- KNOWN FRAGMENTATION WINDOW: this backfill means a pre-existing user
-- is keyed on external_uid = their current username. If that user
-- later renames in Authentik BEFORE re-authenticating against branchDAM,
-- the next ResolveOrCreate sees a NEW (auth_provider, external_uid)
-- triple -- the new stable id from Authentik's stable-id header -- and
-- inserts a SECOND users row rather than upserting the original. The
-- original row stays with external_uid = old-username; audit rows that
-- referenced it still resolve through the old row; the new row
-- accumulates fresh attribution. The two rows collapse back to one on
-- the next Authentik rename or on a manual cleanup. Acceptable as a
-- documented proxy -- the alternative (forcing a re-login at deploy
-- time) is more disruptive than two-rows-per-rename for the few
-- deployments with frequent renames.
UPDATE users SET external_uid = username WHERE external_uid = '';

CREATE UNIQUE INDEX ix_users_auth_provider_external_uid
    ON users(auth_provider, external_uid);

-- Drop the now-redundant username index (PR #407's username UNIQUE
-- already creates an implicit index).
DROP INDEX IF EXISTS ix_users_username;

-- device_pairings gains a nullable owner_user_id FK. New pairings
-- default to the creating admin (handler sets it explicitly); existing
-- rows stay NULL until either re-paired or backfilled. Nullable on
-- purpose -- NOT NULL would force a destructive decision (assign to
-- which user?) into a schema migration, see companion_pairing_audit's
-- pattern for the deferred backfill.
ALTER TABLE device_pairings
    ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT;
CREATE INDEX ix_device_pairings_user_id
    ON device_pairings(user_id) WHERE user_id IS NOT NULL;

-- media_nodes gains uploaded_by_user_id: NULLABLE on purpose, so a
-- legacy scan/watcher/sweeper that doesn't carry user attribution
-- writes NULL rather than forcing the migration to invent a user row.
-- Sweeper background passes always write the "system" user (lazily
-- provisioned at boot via internal/users.SystemUserID). Direct
-- user-triggered scans and uploads write the resolved user.
ALTER TABLE media_nodes
    ADD COLUMN uploaded_by_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT;
CREATE INDEX ix_media_nodes_uploader
    ON media_nodes(uploaded_by_user_id) WHERE uploaded_by_user_id IS NOT NULL;

-- scan_jobs gains started_by_user_id: same nullable + RESTRICT contract
-- as media_nodes. Manual scan/POST /api/v1/scan writes the resolved
-- user; the background SweeperSupervisor writes the system user; a
-- future operator API could distinguish them via this column.
ALTER TABLE scan_jobs
    ADD COLUMN started_by_user_id INTEGER REFERENCES users(id) ON DELETE RESTRICT;

-- actor_audit: append-only event log for admin actions that aren't
-- otherwise audited (scan start, restart, settings PUT, storage
-- location PUT, prune execute, pairing lifecycle). Mirrors
-- companion_pairing_audit's shape but cross-cutting: one table, not
-- per-domain. Actor_kind + actor_name carry the display identity;
-- actor_user_id (nullable) carries the FK to users when one exists, so
-- admin/audit views can filter "everything by user X" or "everything
-- the system did" without parsing the name string.
CREATE TABLE actor_audit (
    id              INTEGER PRIMARY KEY,
    actor_user_id   INTEGER REFERENCES users(id) ON DELETE RESTRICT,  -- NULL = machine / system / env-bootstrap
    actor_kind      TEXT    NOT NULL CHECK (actor_kind IN ('user','machine','system','anonymous')),
    actor_name      TEXT    NOT NULL,                                 -- display string at write time
    event           TEXT    NOT NULL,                                 -- 'scan.started', 'settings.updated', ...
    resource_type   TEXT    NOT NULL,                                 -- 'scan_job', 'app_setting', ...
    resource_id     TEXT,                                             -- string id (location name, setting key, ...)
    details_json    TEXT    NOT NULL DEFAULT '{}',
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX ix_actor_audit_actor_time   ON actor_audit(actor_user_id, created_at DESC);
CREATE INDEX ix_actor_audit_resource     ON actor_audit(resource_type, resource_id, created_at DESC);
CREATE INDEX ix_actor_audit_time         ON actor_audit(created_at DESC);

-- +goose Down

DROP TABLE IF EXISTS actor_audit;

DROP INDEX IF EXISTS ix_media_nodes_uploader;
ALTER TABLE media_nodes DROP COLUMN uploaded_by_user_id;

ALTER TABLE scan_jobs DROP COLUMN started_by_user_id;

DROP INDEX IF EXISTS ix_device_pairings_user_id;
ALTER TABLE device_pairings DROP COLUMN user_id;

DROP INDEX IF EXISTS ix_users_auth_provider_external_uid;
ALTER TABLE users DROP COLUMN external_uid;
ALTER TABLE users DROP COLUMN auth_provider;
ALTER TABLE users DROP COLUMN last_seen_at;

-- PR #407's CREATE UNIQUE INDEX ix_users_username was dropped by Up;
-- recreate it on Down so existing call sites that reference the index
-- name in older DB snapshots keep working after a downgrade.
CREATE UNIQUE INDEX IF NOT EXISTS ix_users_username ON users(username);
