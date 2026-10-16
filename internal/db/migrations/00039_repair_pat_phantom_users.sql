-- +goose Up
--
-- Before this release a request authenticated with a personal access token
-- (auth.PATMiddleware) carried a Principal with Name "pat:<owner users.id>"
-- and ExternalUID "pat:<first 8 hex of the token hash>", and no
-- auth_provider. internal/users.ResolveOrCreate defaulted that to
-- 'authentik' and lazily INSERTed a ('authentik', 'pat:<prefix>') users row
-- for it -- a "phantom" user, admin under the default empty authz.groups --
-- and every upload, scan, device pairing and actor_audit row made with the
-- token was attributed to the phantom instead of the person who minted it.
--
-- The PAT Principal now carries the owner's users.id and resolves to that
-- row directly (no INSERT). This migration repairs what the old code left
-- behind: every attribution FK pointing at a phantom is re-pointed at the
-- token's owner, and the phantom rows are demoted and disabled. They are
-- not deleted (AGENTS.md invariant #1: rows are never deleted, every FK is
-- RESTRICT).
--
-- A users row counts as a phantom only when all of these hold, so a real
-- Authentik identity can never be matched by accident:
--   - auth_provider = 'authentik', source = 'forward-link' (the
--     CreateAttributionUser shape);
--   - external_uid is 'pat:<prefix>' and username is exactly
--     'pat:<owner id>' for an existing users row (the old Principal's
--     Name and ExternalUID);
--   - that owner actually owns a user_pats row whose hashed_key starts
--     with <prefix>.
--
-- No triggers, no CASCADE: plain UPDATEs. The "IN (SELECT phantom_id ...)"
-- guards are load-bearing -- without them the correlated SET subquery
-- would write NULL over every unrelated attribution.

WITH phantom AS (
    SELECT ph.id AS phantom_id, ow.id AS owner_id
    FROM users AS ph
    JOIN users AS ow ON ph.username = 'pat:' || ow.id
    WHERE ph.auth_provider = 'authentik'
      AND ph.source = 'forward-link'
      AND ph.external_uid LIKE 'pat:%'
      AND ow.id <> ph.id
      AND EXISTS (
          SELECT 1 FROM user_pats AS up
          WHERE up.user_id = ow.id
            AND substr(up.hashed_key, 1, 8) = substr(ph.external_uid, 5)
      )
)
UPDATE media_nodes
SET uploaded_by_user_id = (SELECT owner_id FROM phantom WHERE phantom_id = media_nodes.uploaded_by_user_id)
WHERE uploaded_by_user_id IN (SELECT phantom_id FROM phantom);

WITH phantom AS (
    SELECT ph.id AS phantom_id, ow.id AS owner_id
    FROM users AS ph
    JOIN users AS ow ON ph.username = 'pat:' || ow.id
    WHERE ph.auth_provider = 'authentik'
      AND ph.source = 'forward-link'
      AND ph.external_uid LIKE 'pat:%'
      AND ow.id <> ph.id
      AND EXISTS (
          SELECT 1 FROM user_pats AS up
          WHERE up.user_id = ow.id
            AND substr(up.hashed_key, 1, 8) = substr(ph.external_uid, 5)
      )
)
UPDATE scan_jobs
SET started_by_user_id = (SELECT owner_id FROM phantom WHERE phantom_id = scan_jobs.started_by_user_id)
WHERE started_by_user_id IN (SELECT phantom_id FROM phantom);

WITH phantom AS (
    SELECT ph.id AS phantom_id, ow.id AS owner_id
    FROM users AS ph
    JOIN users AS ow ON ph.username = 'pat:' || ow.id
    WHERE ph.auth_provider = 'authentik'
      AND ph.source = 'forward-link'
      AND ph.external_uid LIKE 'pat:%'
      AND ow.id <> ph.id
      AND EXISTS (
          SELECT 1 FROM user_pats AS up
          WHERE up.user_id = ow.id
            AND substr(up.hashed_key, 1, 8) = substr(ph.external_uid, 5)
      )
)
UPDATE device_pairings
SET user_id = (SELECT owner_id FROM phantom WHERE phantom_id = device_pairings.user_id)
WHERE user_id IN (SELECT phantom_id FROM phantom);

WITH phantom AS (
    SELECT ph.id AS phantom_id, ow.id AS owner_id
    FROM users AS ph
    JOIN users AS ow ON ph.username = 'pat:' || ow.id
    WHERE ph.auth_provider = 'authentik'
      AND ph.source = 'forward-link'
      AND ph.external_uid LIKE 'pat:%'
      AND ow.id <> ph.id
      AND EXISTS (
          SELECT 1 FROM user_pats AS up
          WHERE up.user_id = ow.id
            AND substr(up.hashed_key, 1, 8) = substr(ph.external_uid, 5)
      )
)
UPDATE actor_audit
SET actor_user_id = (SELECT owner_id FROM phantom WHERE phantom_id = actor_audit.actor_user_id)
WHERE actor_user_id IN (SELECT phantom_id FROM phantom);

-- Demote and disable the phantoms themselves so GET /api/v1/users stops
-- listing them as active admins. Nothing references them any more.
WITH phantom AS (
    SELECT ph.id AS phantom_id, ow.id AS owner_id
    FROM users AS ph
    JOIN users AS ow ON ph.username = 'pat:' || ow.id
    WHERE ph.auth_provider = 'authentik'
      AND ph.source = 'forward-link'
      AND ph.external_uid LIKE 'pat:%'
      AND ow.id <> ph.id
      AND EXISTS (
          SELECT 1 FROM user_pats AS up
          WHERE up.user_id = ow.id
            AND substr(up.hashed_key, 1, 8) = substr(ph.external_uid, 5)
      )
)
UPDATE users
SET is_admin = 0,
    disabled_at = COALESCE(disabled_at, max(unixepoch(), created_at))
WHERE id IN (SELECT phantom_id FROM phantom);

-- +goose Down
-- No-op: which rows pointed at a phantom before the repair is not
-- recoverable, and the repaired attribution is the correct one.
