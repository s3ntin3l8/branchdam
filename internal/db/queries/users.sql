-- users: one row per human principal that has ever authenticated via
-- Authentik ForwardAuth. Lazily provisioned on first sight by
-- internal/users.Service.ResolveOrCreate, keyed on the stable
-- (auth_provider, external_uid) so Authentik username renames don't
-- fragment attribution.
--
-- All positional params use bare ?1/?2 (not sqlc.arg) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: GetAttributionUserByExternalUID :one
SELECT id, auth_provider, external_uid, username, email, created_at, last_seen_at, is_admin
FROM users
WHERE auth_provider = ?1 AND external_uid = ?2;

-- name: GetAttributionUserByID :one
SELECT id, auth_provider, external_uid, username, email, created_at, last_seen_at, is_admin
FROM users
WHERE id = ?1;

-- name: CreateAttributionUser :one
-- Lazy-provisioning insert. The caller resolves auth_provider +
-- external_uid from the Principal; username/email/is_admin are
-- denormalized display fields refreshed on every ResolveOrCreate.
-- is_admin is derived by the caller from the Principal's group
-- membership against the configured admin groups (auth.IsAdmin) --
-- this is the same policy live request authorization already applies,
-- just persisted so GET /api/v1/users reflects it (issue: forward-auth
-- users' Role column was stuck at the is_admin column default).
-- source='forward-link' with NULL password_hash matches PR #407's
-- existing CHECK constraint -- these rows are attribution-only, not
-- local-auth credentials.
-- Returns the row id on both insert and on conflict (no-op) so the
-- caller never has to distinguish "created" from "already existed" --
-- matching the ResolveOrCreate contract: get-or-create with stable id.
-- SQLite's RETURNING on ON CONFLICT DO NOTHING returns nothing for the
-- no-op case; the workaround is DO UPDATE SET on the conflict target
-- column with a no-op value so the row is "updated" (still returned) but
-- nothing actually changes. is_admin sync for an EXISTING row happens
-- via RefreshAttributionUserSeenWithAdmin below, not here.
INSERT INTO users (auth_provider, external_uid, username, email, is_admin, source, password_hash, last_seen_at, created_at, created_by)
VALUES (?1, ?2, ?3, ?4, ?5, 'forward-link', NULL, unixepoch(), unixepoch(), 'attribution-bootstrap')
ON CONFLICT (auth_provider, external_uid) DO UPDATE SET external_uid = excluded.external_uid
RETURNING id;

-- name: RefreshAttributionUserSeen :exec
-- Updates the denormalized username/email (a user may have renamed since
-- their last request) and bumps last_seen_at. Called by resolveLocal
-- (source='local' branch of ResolveOrCreate) -- deliberately does NOT
-- touch is_admin, which for local accounts is only ever changed via the
-- explicit admin-UI promote/demote actions (PromoteUserToAdmin /
-- DemoteUserFromAdmin), never resynced from a Principal on sight.
-- See RefreshAttributionUserSeenWithAdmin for the forward-link
-- counterpart that DOES resync is_admin.
UPDATE users
SET username = ?2, email = ?3, last_seen_at = unixepoch()
WHERE id = ?1;

-- name: RefreshAttributionUserSeenWithAdmin :exec
-- Forward-link counterpart of RefreshAttributionUserSeen: also syncs
-- is_admin to the caller-computed value (auth.IsAdmin against the
-- Principal's current groups) so an Authentik group promotion or
-- demotion is reflected in users.is_admin -- and therefore in
-- GET /api/v1/users' Role column -- on the very next request, without
-- a separate reconciliation job. Called only from the forward-link
-- branch of ResolveOrCreate; resolveLocal uses the plain
-- RefreshAttributionUserSeen above so a local account's is_admin is
-- never silently overwritten by this sync.
--
-- The is_admin write is guarded by source='forward-link' rather than
-- applied unconditionally: the (auth_provider, external_uid) row this
-- targets can, on some deployments, actually be a source='forward-jit'
-- row -- 00020_users_and_audit.sql's backfill gave every pre-existing
-- row (including forward-jit admins provisioned before that migration)
-- auth_provider='authentik' and external_uid=username, which is exactly
-- the key BrowserChain.pickExternalUID's username fallback resolves to
-- when the identity proxy's stable per-user id header isn't sent.
-- Without the guard, a plain forward-auth request from that same
-- username would overwrite a JIT admin's is_admin using authz.groups --
-- a different, unrelated admin policy from the one that provisioned
-- them (internal/auth/users/jit.go's own adminGroups config). The CASE
-- is a no-op for the common case (source really is 'forward-link');
-- username/email/last_seen_at still refresh unconditionally, matching
-- RefreshAttributionUserSeen's existing behavior for a same-key row of
-- any source.
UPDATE users
SET username = ?2, email = ?3,
    is_admin = CASE WHEN source = 'forward-link' THEN ?4 ELSE is_admin END,
    last_seen_at = unixepoch()
WHERE id = ?1;

-- name: ReconcileLocalExternalUID :exec
-- Realigns external_uid AND auth_provider on an existing user row.
-- Used by the local reconciliation path in
-- internal/users.Service.resolveLocal when a username-based lookup
-- finds a source='local' row whose stored external_uid no longer
-- matches the current username -- typically because an admin renamed
-- the local user without an accompany-side sync to external_uid, or
-- because migration 00028 Down rewrote auth_provider to
-- 'forward-link' and external_uid to the raw username. The
-- session/middleware sets ExternalUID=username for local principals,
-- so a renamed or Down-affected user lands here on their next
-- request and attribution silently NULLs until this UPDATE runs.
-- Caller (resolveLocal) verifies source='local' and the pre-update
-- external_uid mismatch before invoking, so this query is a
-- targeted single-row UPDATE with no further filtering.
UPDATE users
SET external_uid = ?2,
    auth_provider = 'local'
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

-- name: EnsureBootstrapUser :one
-- Lazy-provisions the service-account user the bootstrap PAT belongs
-- to (issue #453 PR E). auth_provider='system' / external_uid=
-- 'ansible-bootstrap' can't collide with a real Authentik uid (those
-- are UUIDs) or with the system sentinel (whose external_uid is just
-- 'system'). The user is created with is_admin=1 by the caller --
-- this query only handles the existence half; the admin bit lives on
-- a separate UPDATE because is_admin isn't part of the INSERT
-- columns above (admin is set by the local-auth migration, and the
-- existing forward-link rows use NULL password_hash which the local-
-- auth schema accepts).
--
-- email is NULL, NOT '': 00018's partial unique index
-- users_email_source_uniq ON (email, source) WHERE email IS NOT NULL
-- treats '' as a value, and EnsureSystemUser already owns the
-- ('', 'forward-link') pair -- a second forward-link row with '' would
-- fail the boot with UNIQUE constraint violated (round-3 review).
-- NULL is excluded from the index predicate, so any number of
-- forward-link service rows can carry it.
INSERT INTO users (auth_provider, external_uid, username, email, source, password_hash, last_seen_at, created_at, created_by)
VALUES ('system', 'ansible-bootstrap', 'ansible-bootstrap', NULL, 'forward-link', NULL, unixepoch(), unixepoch(), 'admin-bootstrap')
ON CONFLICT (auth_provider, external_uid) DO UPDATE SET external_uid = excluded.external_uid
RETURNING id;

-- name: PromoteUserToAdmin :exec
-- Set users.is_admin = 1 for the supplied user id. Does NOT touch
-- disabled_at: a promote action must never silently re-enable a
-- disabled account (round-5 review) -- reactivation is a separate,
-- explicit decision (see PromoteBootstrapUserToAdmin for the one
-- caller whose semantics include it). Idempotent.
UPDATE users SET is_admin = 1 WHERE id = ?1;

-- name: PromoteBootstrapUserToAdmin :exec
-- Bootstrap-specific variant of PromoteUserToAdmin: sets is_admin = 1
-- AND clears disabled_at for the ansible-bootstrap service account.
-- Reactivation is correct HERE and only here: the operator re-running
-- the bootstrap sequence intends the minted PAT to work, and the PAT
-- lookup query (GetUserPATByHash) requires disabled_at IS NULL --
-- promoting without reactivating would mint a wildcard token that
-- 401s on every request while the boot logs success (round-3 review).
-- Named distinctly so a future admin-UI promote action can't pick it
-- up and inherit the reactivation side effect. Idempotent.
UPDATE users SET is_admin = 1, disabled_at = NULL WHERE id = ?1;

-- name: DemoteUserFromAdmin :exec
-- Set users.is_admin = 0 for the supplied user id. The mirror of
-- PromoteUserToAdmin, used by user-management flows (an admin-UI
-- demote action is planned in the issue #453 follow-ups) and, today,
-- by the PAT live-authority tests that exercise a demoted owner's
-- token failing closed. Idempotent -- demoting a non-admin row is a
-- no-op.
UPDATE users SET is_admin = 0 WHERE id = ?1;

-- name: GetUserAdminStatus :one
-- Live authority check for PAT minting (issue #453 PR E): the PAT
-- lookup query requires the OWNER to be is_admin=1 with disabled_at
-- NULL, so minting for anyone else would hand back a well-formed
-- token that can never authenticate. Mint runs this check in the
-- same transaction as the insert; both columns are returned so the
-- caller decides (is_admin is 0/1 per 00018's CHECK; disabled_at
-- NULL means the account can authenticate).
SELECT is_admin, disabled_at FROM users WHERE id = ?1;

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
