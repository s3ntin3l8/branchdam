-- +goose Up
--
-- 00020_users_and_audit.sql (lines 20-24) promised a follow-up that never
-- shipped: pairings created before that migration have user_id NULL, and
-- 00020 stated the backfill source would be companion_pairing_audit.actor
-- (the creating Principal's Name, written at PAIR_CREATED time) resolved
-- against users.username.
--
-- actorFromCtx (internal/httpapi/companion_pairings.go) writes a KindUser
-- principal's actor as "user:" + p.Name, not bare p.Name -- so the join
-- below must strip that prefix before comparing to users.username.
-- KindMachine actors (bare agent_id, e.g. "env-bootstrap") and the
-- unauthenticated "system"/"user:anonymous" sentinels never match the
-- "user:" || username shape and correctly stay unresolved.
--
-- This is best-effort, not deterministic: users rows for Authentik
-- principals are created lazily by ResolveOrCreate on first authenticated
-- request, while this migration runs at boot. If the pairing's creator
-- never authenticated a request that resolves attribution (fresh DB,
-- restored DB, or a deployment where the actor never hit an attribution
-- route), the username join finds nothing and the row is left untouched --
-- goose will not re-run this once applied. Re-pairing the device remains
-- the reliable fix for any pairing this migration cannot backfill.
--
-- The EXISTS guard is load-bearing: without it the correlated subquery
-- would write NULL over NULL when no user match is found, and the
-- `WHERE user_id IS NULL` filter would stop meaning anything.
--
-- No triggers (AGENTS.md invariant #1); plain UPDATE, no CASCADE.
UPDATE device_pairings
SET user_id = (
    SELECT u.id FROM users u
    WHERE 'user:' || u.username = (
        SELECT a.actor FROM companion_pairing_audit a
        WHERE a.pairing_id = device_pairings.id
          AND a.event = 'PAIR_CREATED'
        ORDER BY a.created_at ASC LIMIT 1
    )
)
WHERE user_id IS NULL
  AND EXISTS (
    SELECT 1 FROM users u
    WHERE 'user:' || u.username = (
        SELECT a.actor FROM companion_pairing_audit a
        WHERE a.pairing_id = device_pairings.id
          AND a.event = 'PAIR_CREATED'
        ORDER BY a.created_at ASC LIMIT 1
    )
  );

-- +goose Down
-- No-op: the pre-backfill NULL/non-NULL split is not recoverable.
