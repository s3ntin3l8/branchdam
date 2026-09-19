-- +goose Up
--
-- Personal Access Tokens (PATs) for unattended admin operations.
--
-- Issue #453 PR E: enables the kubeadm-init-style "one-shot operator
-- setup, ongoing Ansible-only flow" for provisioning many branchDAM
-- workstations/operators without a human session cookie. Each token
-- belongs to a single user, can be scoped to a subset of
-- capabilities, and authenticates into the same Principal that a
-- local-auth user session would attach -- so RequireAdmin and
-- downstream admin handlers work unchanged. Tokens carry a prefix
-- ("bdam_pat_") for grep-ability in logs and rotation tooling, and
-- are stored as HMAC-SHA256 hashes of the plaintext under the same
-- pepper pairing.KeyLookup uses for device-pairing key lookup hashes
-- (defense-in-depth: a DB-only compromise cannot forge a token without
-- the pepper).
--
-- Scope model: scopes_json is a JSON array of strings, e.g.
--   ["pairings:write", "settings:write"]
-- The bootstrap PAT uses ["*"] (admin wildcard); subsequent tokens
-- minted by admins via POST /api/v1/users/me/pats are scoped to the
-- scopes the calling admin grants. RequirePAT enforces scope on
-- every request: an admin route that requires "settings:write" with
-- a token that only carries "pairings:write" gets a 403, NOT a
-- silent downgrade.
--
-- Lifecycle: last_used_at is bumped async on every authenticated
-- request (so the write is non-blocking on the auth path; a flush
-- failure is logged but never propagates). expires_at NULLABLE for
-- non-expiring tokens; revoked_at NULLABLE for active tokens. We
-- never delete rows -- RESTRICT on user delete (audit-trail
-- preservation across user retirement) matches actor_audit's model.
--
-- Storage note: scopes_json is a TEXT column with JSON content. A
-- future migration could promote to a JSONB column or a normalized
-- scope_grants child table; for v1 the inline-JSON shape matches the
-- token-volume reality (each user has tens of tokens, each with
-- a small scope list) and avoids the JOIN cost of normalization.

CREATE TABLE user_pats (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name            TEXT    NOT NULL,
    -- Hex-encoded HMAC-SHA256(pepper, plaintext) -- 64 chars, lower-case.
    -- UNIQUE so a hash collision surfaces as a constraint violation
    -- rather than silently authenticating the wrong token. Same hash
    -- function as internal/pairing.Service.hashKey (HMAC under the
    -- pairing pepper) -- shared pepper means a leaked DB without the
    -- pepper can't reconstruct either kind of key.
    hashed_key      TEXT    NOT NULL UNIQUE,
    scopes_json     TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(scopes_json)),
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    last_used_at    INTEGER,
    expires_at      INTEGER,
    revoked_at      INTEGER
);
-- The UNIQUE constraint on hashed_key already creates the lookup
-- index this table needs (an equality probe on a UNIQUE column is an
-- indexed lookup in SQLite), so no separate ix_user_pats_hash index
-- is created -- that would double the per-insert index-write cost
-- for zero lookup benefit.
CREATE INDEX ix_user_pats_user_id ON user_pats(user_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS ix_user_pats_user_id;
DROP TABLE IF EXISTS user_pats;
