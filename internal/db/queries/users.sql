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
-- display fields refreshed on every ResolveOrCreate.
-- Returns the row id even on conflict (no-op) so the
-- caller never has to distinguish "created" from "already existed" --
-- matching the ResolveOrCreate contract: get-or-create with stable id.
INSERT INTO users (auth_provider, external_uid, username, email, last_seen_at)
VALUES (?1, ?2, ?3, ?4, unixepoch())
ON CONFLICT (auth_provider, external_uid) DO NOTHING
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
-- Lazy-provisions the "system" user used as the attribution owner for
-- background work (SweeperSupervisor's INCREMENTAL passes, prune, anything
-- that runs without a request principal). Idempotent: re-running returns
-- the same row. external_uid is a sentinel that can never collide with a
-- real Authentik uid (those are UUIDs); this is the canonical "no human
-- behind this" attribution in actor_audit and scan_jobs.started_by_user_id.
INSERT INTO users (auth_provider, external_uid, username, email, last_seen_at)
VALUES ('system', 'system', 'system', '', unixepoch())
ON CONFLICT (auth_provider, external_uid) DO NOTHING
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
