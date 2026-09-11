# Multi-user attribution

This feature models "who did what" across the library: which user uploaded
an asset, which user started a scan, which user created a device pairing.
Visibility stays a shared library (no ACL split), but every writeable row
now carries a nullable attribution FK, and a separate cross-cutting
audit log records admin actions that don't otherwise have an audit
trail.

## Identity model

The `users` table is the canonical identity table, shared with PR #407's
local-auth layer. Three additive columns (migration 00020) carry the
attribution identity:

| Column          | Meaning                                                   |
|-----------------|-----------------------------------------------------------|
| `auth_provider` | `"authentik"` for browser requests, `"forward-link"` / `"forward-jit"` for local-auth rows, `"system"` for the background sentinel. |
| `external_uid`  | Stable per-user id from Authentik's stable-id header. Never renames. Falls back to `username` on older Authentik deployments. |
| `last_seen_at`  | Refreshed on every authenticated request via `ResolveOrCreate`. |

`UNIQUE (auth_provider, external_uid)` is the dedup key — Authentik
username renames don't fragment attribution because the stable id
stays the same.

## First-class user provisioning

`internal/users.Service.ResolveOrCreate(ctx, Principal)` is the
canonical attribution entry point. It:

1. INSERTs ON CONFLICT DO UPDATE the row keyed on
   `(auth_provider, external_uid)`, returning the row id either way
   (the `DO UPDATE SET` on the conflict target is the SQLite idiom
   that keeps `RETURNING` returning a row on conflict; the more
   obvious `DO NOTHING` swallows the no-op case).
2. UPDATEs the denormalized `username`, `email`, `last_seen_at` in
   the same transaction.
3. Refuses `KindMachine`, `Authenticated=false`, or empty
   `ExternalUID` with `ErrInvalidPrincipal`.

The system sentinel `("system", "system")` is provisioned once at boot
via `EnsureSystemUser`. Every background worker (SweeperSupervisor's
INCREMENTAL passes, prune, anything that has no request Principal)
attributes its writes to this row.

## Attribution columns

| Table | Column | FK semantics |
|---|---|---|
| `media_nodes` | `uploaded_by_user_id` | nullable, RESTRICT. Browser upload = resolved user; scanner/sweeper/agent-without-pairing = NULL. |
| `scan_jobs` | `started_by_user_id` | nullable, RESTRICT. Browser `POST /api/v1/scan` = resolved user; SweeperSupervisor = system user; WatcherSupervisor's RUNNING WATCH rows = NULL (long-lived server-owned). |
| `device_pairings` | `user_id` | nullable, RESTRICT. Set at pairing create time from the creating request's resolved user id; paired-device uploads attribute to this row. |

All FKs are RESTRICT and nullable. The NOT NULL follow-up lands in a
separate PR after `companion_pairing_audit.actor` is backfilled into
existing pairings.

## Audit logs

Two parallel logs:

- `actor_audit`: cross-cutting admin event log. Written by every
  write route (settings, restart, storage-location, prune, scan).
  Resolves `Principal` → `(actor_user_id, actor_kind, actor_name)`:
  user / machine / system / anonymous.
- `login_audit`: PR #407's local-auth login/reset log. Owned by
  `internal/auth/users`.

`GET /api/v1/audit?type=activity|login` merges both, newest-first,
with offset pagination. The `type=login` query reads `login_audit`
directly; `type=activity` reads `actor_audit`.

Audit writes are best-effort: a failed audit write is logged, not
surfaced as a 5xx. A successful write followed by a failed response
is preferred over a failed response with no audit trail.

## HTTP routes

| Route | Effect |
|---|---|
| `GET /api/v1/me` | Adds `attributionUserId` (resolved via `ResolveOrCreate`). |
| `POST /api/v1/scan` | Resolves `Principal` → user_id, sets `scan_jobs.started_by_user_id`, writes `actor_audit('scan.started')`. |
| `PUT /api/v1/settings` | Writes `actor_audit('settings.updated')` with the diff. |
| `POST /api/v1/restart` | Writes `actor_audit('restart.executed')` before the restart goroutine. |
| `PUT /api/v1/storage-locations/{id}` | Writes `actor_audit('storage_location.upserted')`. |
| `POST /api/v1/prune` (execute) | Writes `actor_audit('prune.executed')`. |
| `POST /api/v1/companion/pairings` | Sets `device_pairings.user_id` from the creating request's resolved user id. |
| `GET /api/v1/audit?type=activity\|login` | Merged actor_audit + login_audit read. |
| `GET /api/v1/users` | Admin-only read of the users table. |
| `GET /api/v1/assets?uploadedByUserId=N` | "My uploads" filter. |

## SPA

- `AssetListPage`: "My Uploads" checkbox (disabled when the request
  has no resolved attribution id), "Uploaded by" column with per-row
  username resolution via `/api/v1/users`.
- `Me.attributionUserId` plumbed through `useMe()`; the SPA pins it
  on the assets query for the filter.

## Cross-references

- `docs/handoff-multi-user-attribution.md` — the implementation
  handoff written during the rebase + schema reconciliation phase.
- `docs/schema.md` — schema invariants and migration history.
- `docs/forward-auth.md` — Authentik ForwardAuth header mapping,
  including `X-Authentik-Uid` (the stable id the attribution layer
  keys on).
