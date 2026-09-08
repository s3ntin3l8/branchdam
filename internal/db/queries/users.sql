-- users: one row per human principal that has ever authenticated via
-- Authentik ForwardAuth. Lazily provisioned on first sight by
-- internal/users.Service.ResolveOrCreate, keyed on the stable
-- (auth_provider, external_uid) so Authentik username renames don't
-- fragment attribution.
--
-- All positional params use bare ?1/?2 (not sqlc.arg) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: GetAttributionUserByExternalUID :one
SELECT id, auth_provider, external_uid, username, email, created_at, last_seen_at
FROM users
WHERE auth_provider = ?1 AND external_uid = ?2;

-- name: GetAttributionUserByID :one
SELECT id, auth_provider, external_uid, username, email, created_at, last_seen_at
FROM users
WHERE id = ?1;

-- name: CreateAttributionUser :one
-- Lazy-provisioning insert. The caller resolves auth_provider +
-- external_uid from the Principal; username/email are denormalized
-- display fields refreshed on every ResolveOrCreate. source='forward-link'
-- with NULL password_hash matches PR #407's existing CHECK constraint --
-- these rows are attribution-only, not local-auth credentials.
-- Returns the row id on both insert and on conflict (no-op) so the
-- caller never has to distinguish "created" from "already existed" --
-- matching the ResolveOrCreate contract: get-or-create with stable id.
-- SQLite's RETURNING on ON CONFLICT DO NOTHING returns nothing for the
-- no-op case; the workaround is DO UPDATE SET on the conflict target
-- column with a no-op value so the row is "updated" (still returned) but
-- nothing actually changes.
INSERT INTO users (auth_provider, external_uid, username, email, source, password_hash, last_seen_at, created_at, created_by)
VALUES (?1, ?2, ?3, ?4, 'forward-link', NULL, unixepoch(), unixepoch(), 'attribution-bootstrap')
ON CONFLICT (auth_provider, external_uid) DO UPDATE SET external_uid = excluded.external_uid
RETURNING id;

-- name: RefreshAttributionUserSeen :exec
-- Updates the denormalized username/email (a user may have renamed since
-- their last request) and bumps last_seen_at. Called by
-- ResolveOrCreate after a cache miss, in the same transaction as the
-- create-or-no-op insert above. No-op on a missing row (the create
-- branch above will have just inserted one in the same tx).
UPDATE users
SET username = ?2, email = ?3, last_seen_at = unixepoch()
WHERE id = ?1;

-- name: EnsureSystemUser :one
-- Lazy-provisions the "system" attribution sentinel: background workers
-- (SweeperSupervisor's INCREMENTAL passes, prune, anything that has no
-- request Principal) attribute their writes to this user. Idempotent:
-- re-running returns the same row. source='forward-link' with NULL
-- password_hash matches PR #407's existing CHECK; auth_provider='system'
-- / external_uid='system' can't collide with a real Authentik uid (those
-- are UUIDs). This is the canonical "no human behind this" attribution in
-- actor_audit and scan_jobs.started_by_user_id. The DO UPDATE SET trick
-- (see CreateAttributionUser) keeps RETURNING returning a row on conflict.
INSERT INTO users (auth_provider, external_uid, username, email, source, password_hash, last_seen_at, created_at, created_by)
VALUES ('system', 'system', 'system', '', 'forward-link', NULL, unixepoch(), unixepoch(), 'attribution-bootstrap')
ON CONFLICT (auth_provider, external_uid) DO UPDATE SET external_uid = excluded.external_uid
RETURNING id;

-- name: GetSystemUserID :one
-- The EnsureSystemUser companion query: lookup the system user by its
-- stable sentinel (external_uid='system'), so the boot sequence can
-- capture its id before any background worker starts. Same shape as
-- GetAttributionUserByExternalUID -- named distinctly so the boot path
-- reads self-documentingly and a future "system user has been renamed"
-- rename has one obvious place to land.
SELECT id
FROM users
WHERE auth_provider = 'system' AND external_uid = 'system';

-- name: ListAttributionUsers :many
-- Backs GET /api/v1/users (admin-only). Used by the pairing UI's
-- "Owned by" selector (when it lands -- today defaults to the creating
-- admin, see internal/pairing's CreatePairing). Capped at 200 rows in
-- the handler; this query is unbounded by design (an admin-only endpoint,
-- no pagination UI yet).
SELECT id, auth_provider, external_uid, username, email, created_at, last_seen_at
FROM users
ORDER BY username ASC, id ASC
LIMIT ?1 OFFSET ?2;

-- name: CountAttributionUsers :one
SELECT COUNT(*) FROM users;
