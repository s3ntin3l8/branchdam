# Handoff: `feat/multi-user-attribution`

For whoever picks up local branch `feat/multi-user-attribution` (checked out at
`.worktrees/feat-multi-user-attribution`, commit `ced2908`, never pushed). Written while
fixing issue #413 (sqlc v1.31.1 "corruption"), which that branch's own commit message
misdiagnoses — see below.

## State

One commit ahead of `fadd2d4`, so now several commits behind `origin/main` (which has
since gained `b26f483` local auth and `8b458d2`).

## What is real and worth keeping

Migration `00018_users_and_audit.sql` (users + actor_audit tables, nullable user_id FKs
on `media_nodes.uploaded_by_user_id`, `scan_jobs.started_by_user_id`,
`device_pairings.user_id`) and the accompanying
`internal/db/queries/{users,actor_audit}.sql`. The commit message's own "NOT YET DONE"
list (internal/users package, internal/audit package, BrowserChain X-Authentik-Uid,
route wiring, SPA updates, docs) is accurate and still needed.

## What is not real — do not carry it forward

1. **Everything in the commit message under "Pre-existing sqlc v1.31.1 corruption
   patterns … fixed by `.bin/sqlc-fixup.sh`".** That script does not exist and never
   did — not in the working tree, not on `main`, not anywhere in git history. The real
   cause (issue #413, fixed on `fix/sqlc-non-ascii-corruption`): sqlc v1.31.1's SQLite
   engine slices statement spans using rune offsets as if they were byte offsets, so any
   multi-byte UTF-8 character in `internal/db/queries/*.sql` (even inside a `--`
   comment) corrupts every later statement in that file. Three characters in three files
   caused all of it. See `docs/schema.md`'s "sqlc risk: non-ASCII in query comments"
   section for the full writeup, reproduction, and upstream links
   ([sqlc#4372](https://github.com/sqlc-dev/sqlc/issues/4372),
   [#4523](https://github.com/sqlc-dev/sqlc/issues/4523),
   [#4535](https://github.com/sqlc-dev/sqlc/pull/4535)).

   Any hand-edits made to `internal/db/sqlcgen/` on this branch to work around the
   "corruption" should be discarded. Once rebased past the fix branch, `internal/db/queries/*.sql` must stay ASCII-only (enforced by
   `internal/db/queries_ascii_test.go`'s `TestQueryFilesAreASCII` under `make check`) and
   a plain `sqlc generate` will produce correct output — no post-processor needed.

2. **The four "pre-existing test failures (NOT caused by this work)"** —
   `TestCreatePairing_HappyPath`, `TestGetMediaNodeByFullHash`,
   `TestHeuristicSpatialTemporalResolver`, `TestDrainer_NodeCreated_ContentDedup_*`. All
   four **pass** on `main` (verified: `go test ./internal/...` on a clean checkout is
   26 packages `ok`, one failure — `TestNoDirectAuthentikHeaderReads`, unrelated, see
   below — and none of these four are it). If they fail on this branch after rebasing,
   this branch's own changes caused it — don't carry the "pre-existing" framing forward
   without re-verifying against current `main`.

## Blocking conflict: migration number `00018` is taken

`origin/main` already ships `00018_local_auth.sql` (PR #407) with its own `users` table
— overlapping in intent and in name with this branch's `00018_users_and_audit.sql`.
Rebasing onto `origin/main` means:

- Renumbering this branch's migration to `00019_`.
- Reconciling the two `users` schemas. The shipped one already has
  `source ∈ ('local', 'forward-jit', 'forward-link')`, `is_admin`, `disabled_at`, and a
  `password_hash` CHECK enforcing the local/forward-auth split. Most of this branch's
  `users` table work is likely superseded by it.
- `actor_audit` and the three `*_user_id` FK columns (`media_nodes.uploaded_by_user_id`,
  `scan_jobs.started_by_user_id`, `device_pairings.user_id`) are what actually survive
  the rebase — they don't overlap with what shipped.

## Also found, left unfixed here (belongs with worktree usage, not #413)

`internal/auth/no_leak_test.go`'s directory skip list (around line 47) excludes
`.mullion-worktrees` but not `.worktrees/` — the actual directory name git worktrees for
this repo land in (`.gitignore`'s own entry is `.worktrees/`). Anyone with a
`.worktrees/` checkout on disk — which is exactly this branch's own setup — gets
`TestNoDirectAuthentikHeaderReads` failing on files inside that checkout, not their
actual working tree. Confirmed still present and still tripped by two stale worktrees
(`feat-multi-user-attribution` itself, plus an unrelated `feat-auth-password-reset`) as
of this writing; the same phantom-file issue also broke `golangci-lint`'s
generated-file filter via a third stale worktree (`docs-local-auth`), surfacing three
unrelated `errcheck` findings against files that no longer exist on disk. One-line fix
to the skip list (add `.worktrees`), but it's a worktree-hygiene / test-robustness issue
orthogonal to this branch's actual work, so it's flagged here rather than fixed as part
of #413.
