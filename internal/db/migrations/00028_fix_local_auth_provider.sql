-- 00028_fix_local_auth_provider.sql — backfill auth_provider for local users.
--
-- Migration 00020 added auth_provider with a DEFAULT of 'authentik'.
-- CreateLocalUser (local_auth.sql) explicitly sets auth_provider='local',
-- so any local user created AFTER that query landed is already correct.
-- But local users created before 00020 got the 'authentik' default.
--
-- This backfill sets auth_provider='local' and external_uid=username for
-- those rows. The unique index ix_users_auth_provider_external_uid won't
-- collide because forward-link users have auth_provider='authentik', so
-- ('local','alice') and ('authentik','alice') are distinct keys.

-- +goose Up
UPDATE users
SET auth_provider = 'local', external_uid = username
WHERE source = 'local'
  AND auth_provider != 'local';

-- +goose Down
-- Scope mirrors Up structurally: Up selects rows where
-- source='local' AND auth_provider != 'local'; Down selects the same
-- rows in their post-Up state (auth_provider='local'). Note this also
-- catches post-00020 local users (CreateLocalUser already tags them
-- 'local'); Down is destructive on them too, but the effect is to move
-- them off 'local' -- re-running Up restores them. The migration
-- predates row-tracking, so there's no precise WHERE distinguishing
-- pre-00020-then-Updated from post-Create rows.
--
-- The previous Down rewrote every such row to ('authentik', ''), which
-- violates ix_users_auth_provider_external_uid whenever more than one
-- pre-00020 local user exists -- two rows collapse to the same key
-- ('authentik', '') and the UPDATE errors out on the second hit.
--
-- This Down instead uses auth_provider='forward-link' and
-- external_uid=username (the 00020 backfill convention). The composite
-- key stays unique (username is UNIQUE via 00018's ix_users_username),
-- and 'forward-link' is distinct from both 'authentik' and 'local' so
-- nothing collides on downgrade. A subsequent Up re-applies cleanly.
UPDATE users
SET auth_provider = 'forward-link', external_uid = username
WHERE source = 'local'
  AND auth_provider = 'local';
