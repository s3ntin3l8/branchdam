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
-- No secondary indexes: UNIQUE on hashed_key covers the lookup
-- query, the PK covers revoke-by-id, and no query in this PR
-- predicates revoked_at or scans by user_id with a selective filter
-- (ListUserPATs/CountUserPATs include revoked rows on purpose, so a
-- partial index on user_id would never be picked -- round-3 review's
-- EXPLAIN QUERY PLAN check). Per-user token volume is tens of rows;
-- a sequential scan there is the right plan.

-- +goose Down
DROP TABLE IF EXISTS user_pats;
