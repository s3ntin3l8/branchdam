-- +goose Up
-- actor_audit records who acted, but not which credential they used.
-- A PAT request and the owner's own click otherwise look identical.
-- Both columns are nullable: rows written before this migration stay
-- NULL, and machine / system / anonymous actors have no provider.
-- auth_provider follows the users.auth_provider vocabulary. 'oauth'
-- is reserved for a later connector; nothing writes it yet.
ALTER TABLE actor_audit ADD COLUMN auth_provider TEXT CHECK (auth_provider IN ('authentik','local','pat','oauth'));
ALTER TABLE actor_audit ADD COLUMN pat_id INTEGER REFERENCES user_pats(id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE actor_audit DROP COLUMN pat_id;
ALTER TABLE actor_audit DROP COLUMN auth_provider;
