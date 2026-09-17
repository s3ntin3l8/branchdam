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
UPDATE users
SET auth_provider = 'authentik', external_uid = ''
WHERE source = 'local'
  AND auth_provider = 'local';
