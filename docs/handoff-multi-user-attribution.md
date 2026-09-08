# Handoff: `feat/multi-user-attribution`

Local branch `feat/multi-user-attribution` (checked out at
`.worktrees/feat-multi-user-attribution`, current HEAD after rebase + full
implementation; never pushed). Implements multi-user attribution on top of
PR #407's `users` table.

## State

Rebased onto `origin/main` past issue #413's root-cause fix (commit `cccb2c1`,
merged via PR #417). All 30 Go packages pass `go test ./...`; `make check`
+ `make check-web` both clean; 319 web tests pass. Migration landed as
`00020_users_and_audit.sql` (PR #407's `00019_password_reset.sql` already lives
on main, so this branch's original `00019_` was renumbered). sqlc generation
is plain — no `.bin/sqlc-fixup.sh`, no post-processing. `.worktrees` is in the
no_leak_test skip list so this worktree itself doesn't trip the test.

## What's shipped on this branch

### Schema + queries (`internal/db/`)

- `internal/db/migrations/00020_users_and_audit.sql`
  - `users.auth_provider`, `users.external_uid`, `users.last_seen_at` columns + backfill
  - `UNIQUE (auth_provider, external_uid)` index; drops PR #407's redundant `ix_users_username`
  - Nullable `device_pairings.user_id` (RESTRICT)
  - Nullable `media_nodes.uploaded_by_user_id` (RESTRICT) + partial index
  - Nullable `scan_jobs.started_by_user_id` (RESTRICT)
  - `actor_audit` table (cross-cutting audit log; separate from `login_audit`)
- `internal/db/queries/users.sql`, `actor_audit.sql`, `local_auth.sql`,
  `media_nodes.sql`, `media_edges.sql`, `scan_jobs.sql`, `companion_pairing.sql`,
  `login_audit_read.sql` — all column sets and INSERTs reconciled. Local-auth
  INSERTs write `auth_provider='local'/'forward-jit'` and `external_uid=username`
  explicitly so they don't collide on the unique index via the `'authentik'`/`''`
  defaults. Attribution upserts use `ON CONFLICT DO UPDATE SET` so `RETURNING`
  always yields a row (SQLite's `DO NOTHING` swallows the no-op case otherwise).

### Attribution + audit (`internal/users`, `internal/audit`)

- `internal/users.Service`:
  - `ResolveOrCreate(ctx, Principal)` returns `(id, auth_provider, external_uid,
    username, email)` for an authenticated KindUser Principal, lazy-creating the
    row + refreshing `last_seen_at` in the same transaction. Refuses KindMachine,
    Authenticated=false, or empty ExternalUID with `ErrInvalidPrincipal`.
  - `EnsureSystemUser` provisions the `(system, system)` sentinel; the SweeperSupervisor's
    `INCREMENTAL` passes attribute to this row. `SystemUser`/`SystemUserID` panic
    if called before `EnsureSystemUser`.
- `internal/audit.Service`:
  - `WriteActorAudit(ctx, Principal, event, resourceType, resourceID, details)` —
    details marshaled as JSON (nil → `"{}"`).
  - `ListActivity(ctx, Filter, limit, offset)` for the audit read route.
  - Resolves Principal → `(actor_user_id, kind, name)`: authenticated user → kind=user
    with resolved id; machine → kind=machine with name=agent_id; system → kind=system
    with cached system user id; unauthenticated → kind=anonymous with id=0.
  - `SystemActor` is the canonical Principal background workers pass.

### HTTP layer (`internal/httpapi/`)

- `Deps.Attribution`, `Deps.Audit` new dependencies; routes that depend on them
  503 when not wired (every existing test, forward-only deployments without users).
- `POST /api/v1/scan`: resolves Principal → user_id, sets `scan_jobs.started_by_user_id`,
  writes `actor_audit('scan.started', jobId, ...)`.
- `POST /api/v1/settings`: writes `actor_audit('settings.updated', ..., diff)`.
- `POST /api/v1/restart`: writes `actor_audit('restart.executed')` *before* the
  restart goroutine (a row that lands after the binary exits is useless).
- `PUT /api/v1/storage-locations/{id}`: writes `actor_audit('storage_location.upserted', name, diff)`.
- `POST /api/v1/prune` (execute only): writes `actor_audit('prune.executed', name, counts)`.
- `GET /api/v1/audit?type=activity|login`: merged actor_audit + login_audit read route.
- `GET /api/v1/users`: admin-only read of the users table; backs the SPA's "Uploaded by"
  lookup and pairing UI's "Owned by" selector.
- `GET /api/v1/me`: returns `attributionUserId` (lazy-resolved via
  `internal/users.ResolveOrCreate`) so the SPA can pin the "My uploads" filter.
- `GET /api/v1/assets?uploadedByUserId=N`: "My uploads" filter (sqlc.narg-filtered,
  mirror of the existing lifecycle/camera/graph/location filters).
- `POST /api/v1/companion/pairings`: `device_pairings.user_id` set from the creating
  request's resolved user id; paired-device uploads attribute to the human who paired.

### SPA (`web/src/`)

- `AssetListPage`: "My Uploads" checkbox (disabled when `me.attributionUserId === 0`),
  "Uploaded by" column with per-row username resolution via the new `/api/v1/users` cache.
- `AssetQueryParams.uploadedByUserId` + `Asset.uploadedByUserId` plumbed through.
- `Me.attributionUserId` plumbed through.
- New `useUsers`, `useAudit` hooks; `listUsers`, `listAudit` API methods.
- `EdgeAuditEntry` (renamed from `AuditEntry`) is the edge-review queue's row shape;
  the new `AuditEntry` is the merged actor_audit + login_audit shape.

## Notes for the next agent

- Migration 00020's `Down` cleanly reverses to `ix_users_username` (the index PR #407
  dropped), so a downgrade to a pre-PR #407 schema still leaves the schema consistent.
- `TestDowngradeIndexSuffixStemEdges` was updated to also raw-`ALTER TABLE` add
  `uploaded_by_user_id` after `goose.UpTo(7)`, matching the existing pattern it uses
  for `source_path_hash` from migration 15.
- `CreateLocalUser` and `CreateForwardJITUser` now write
  `(auth_provider, external_uid)` explicitly (`('local', username)` /
  `('forward-jit', username)`) so they don't fall back to the `'authentik'`/`''` defaults
  that would collide on the unique index.
- Attribution upserts (`CreateAttributionUser`, `EnsureSystemUser`) use
  `ON CONFLICT (auth_provider, external_uid) DO UPDATE SET external_uid =
  excluded.external_uid RETURNING id` -- the DO UPDATE on the conflict target
  is the SQLite idiom that keeps `RETURNING` returning a row on conflict
  (the more obvious `DO NOTHING` swallows the no-op case and breaks
  ResolveOrCreate's "always returns the id" contract).
- Pairing's `user_id` column is nullable on purpose; a follow-up PR can
  backfill `companion_pairing_audit.actor` into existing pairings and then
  tighten the schema to NOT NULL.
- The unused throwaway `.bin/sqlc-fixup.sh` is gone; don't reintroduce it — plain
  `sqlc generate` works on this branch.

## Verification

- `make check`: lint + 30 Go packages + build + golangci-lint all clean.
- `make check-web`: lint + typecheck + build + 319 tests all clean.
- Pre-push hook chain: trim whitespace, end-of-file fixer, gofmt, go vet,
  go mod tidy, sqlc diff — all pass.
