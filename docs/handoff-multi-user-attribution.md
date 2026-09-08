# Handoff: `feat/multi-user-attribution`

Local branch `feat/multi-user-attribution` (checked out at
`.worktrees/feat-multi-user-attribution`, current HEAD after rebase + schema reconciliation;
never pushed). Implements multi-user attribution on top of PR #407's `users` table.

## State

Rebased onto `origin/main` past issue #413's root-cause fix (commit `cccb2c1`,
merged via PR #417). All 28 packages now pass `go test ./... -count=1`; `go build ./...` clean.
Migration landed as `00020_users_and_audit.sql` (PR #407's `00019_password_reset.sql` already
lives on main, so this branch's original `00019_` was renumbered). sqlc generation is plain —
no `.bin/sqlc-fixup.sh`, no post-processing. `.worktrees` is now in the no_leak_test skip list
so this worktree itself doesn't trip the test.

## What's shipped on this branch (schema + queries)

- `internal/db/migrations/00020_users_and_audit.sql`
  - `users.auth_provider`, `users.external_uid`, `users.last_seen_at` columns + backfill
  - `UNIQUE (auth_provider, external_uid)` index; drops PR #407's redundant `ix_users_username`
  - Nullable `device_pairings.user_id` (RESTRICT)
  - Nullable `media_nodes.uploaded_by_user_id` (RESTRICT) + partial index
  - Nullable `scan_jobs.started_by_user_id` (RESTRICT)
  - `actor_audit` table (cross-cutting audit log; separate from `login_audit`)
- `internal/db/queries/users.sql`, `actor_audit.sql`, `local_auth.sql`,
  `media_nodes.sql`, `media_edges.sql`, `scan_jobs.sql`, `companion_pairing.sql` —
  all column sets and INSERTs reconciled.
- `internal/pipeline/commit.go` carries nullable `UploadedByUserID` through
  `InsertMediaNode`; the upload path uses the resolved user, scanner/sweeper leave it NULL.

## Still to do (real PR scope)

- `internal/users`: lazy provisioning, `ResolveOrCreate(auth_provider, external_uid, ...) -> user_id`,
  `SystemUserID` (provisioned at boot, used by background scans/watchers).
- `internal/audit`: write helper for `actor_audit` + read for the merged audit route.
- `internal/auth.BrowserChain`: surface the `X-Authentik-Uid` header on the `Principal`
  (so `ResolveOrCreate` has a stable identity across username renames).
- Pairing: `CreateDevicePairing` accepts the creating user's id; paired-upload path
  uses the pairing's owner_user_id.
- HTTP routes: `POST /api/v1/scan` (and any future operator endpoints) write
  `actor_audit` and set `scan_jobs.started_by_user_id`; `POST /api/v1/settings`,
  `POST /api/v1/storage-location` similarly. Single `GET /api/v1/audit?type=activity|login`
  merges `actor_audit` and `login_audit` sorted by timestamp.
- SPA: `My uploads` filter, `Uploaded by` column on assets/edges.
- Tests: end-to-end attribution in `internal/auth/users` and `internal/httpapi`;
  `actor_audit` write/read; pairing owner inheritance.

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
- The unused throwaway `.bin/sqlc-fixup.sh` is gone; don't reintroduce it — plain
  `sqlc generate` works on this branch.
