# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Guidelines

- **Use the repo's templates.** When opening a PR, fill in
  [`.github/pull_request_template.md`](.github/pull_request_template.md) rather than writing a
  free-form body -- its checklist encodes what actually breaks CI (golangci-lint separately from
  `make lint`, sqlc codegen, DTO hand-sync) and is meant to be ticked honestly, not skipped. When
  filing an issue, use the
  [issue blueprint](.github/ISSUE_TEMPLATE/issue-blueprint.md) (Context/Scope/Out of
  scope/Acceptance criteria). See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full pre-PR
  checklist.
- **Addressing review feedback (Hermes or human).** Fixing the code alone is not enough --
  always reply to and resolve the inline conversation too. Thread resolution is GraphQL-only
  (not derivable from the REST comment id), so it's two calls:
  ```sh
  # 1. Reply to the inline comment (REST, uses the comment id)
  gh api repos/s3ntin3l8/branchdam/pulls/<PR>/comments/<comment_id>/replies -f body="Fixed in <sha>"
  # 2. Resolve the thread (GraphQL only -- needs the thread node id from a reviewThreads
  #    query on the PR, NOT the REST comment id)
  gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "<thread_id>"}) { thread { isResolved } } }'
  ```
- **Mullion dock/actions config.** `.crs/dock.json` (persistent click-to-start monitors) and
  `.crs/actions.json` (one-shot launchers) are Mullion dashboard config, not part of the app.
  Every `command` in both files deliberately goes through the Makefile (`make dev-api`, `make
  check`, ...) rather than inlining a raw shell command, so the dock/launcher surface and a
  plain terminal never drift. Hand-editing either file hits Mullion's *lenient* reader
  (`project-config.js`'s `normalizeRawControl`/`normalizeRawAction`) -- a malformed entry, a
  missing required field, or a typo'd key is silently dropped with only a server-side
  `console.warn`, never surfaced in the UI. The stricter validator
  (`dock-config.js`'s `validateDockConfig`, which rejects unknown keys) only runs on a save made
  through the dashboard's own editor. After hand-editing, verify with `mullion project dock
  <id> --json` / `mullion project actions <id> --json` and confirm every field you set actually
  came back -- "the JSON parses" proves nothing here.

## Commands

### Backend (Go)

```sh
# Build (stubs web/dist first if the SPA hasn't been built -- see below)
make build
go build ./...

# Test (all packages, race detector)
make test
go test -race ./...

# Test a single package
go test -race ./internal/pipeline/...

# Format check (CI enforces this)
gofmt -l $(git ls-files '*.go')

# Vet
go vet ./...

# Run the server
make dev
go run ./cmd/branchdam -config config.yaml -debug

# Run API + frontend together, or one at a time. `make dev` above is the
# original, unchanged target -- it does NOT bootstrap config.yaml and still
# fails on a fresh clone. `dev-api`/`dev-all` below do, via `dev-config`
# (see below) -- prefer these for a first-time local setup.
make dev-api    # Go API only, :8080
make dev-web    # Vite dev server only, :5173, proxies /api -> :8080
make dev-all    # both in one terminal (Ctrl-C stops both)

# Pinned golangci-lint (required CI check; NOT covered by `make lint`)
make golangci-lint

# One-shot pre-PR gates
make check       # lint + test + build + golangci-lint
make check-web   # cd web && lint + typecheck + build
```

> The Go binary embeds `web/dist` via `//go:embed` (`web/embed.go`). If `web/dist` doesn't
> exist, backend builds/tests fail. `make build`/`make test`/`make dev` all depend on
> `web-stub`, which runs `.github/ci-prebuild.sh` to create a placeholder if none exists yet --
> the same script CI's Go-only lane uses. Run `npm run build` in `web/` first if you want the
> real SPA embedded instead of the stub.

> `config.yaml` is gitignored and not present in a fresh clone; `make dev-api`/`dev-all` depend
> on `dev-config`, which renders `config.dev.yaml` (a committed template, absolute paths under
> `data/`, which it also creates) into `config.yaml` the first time, and leaves an existing
> `config.yaml` alone on every run after. `database.path` there must stay absolute --
> `storage.Guard`'s `canonicalize` (`internal/storage/guard.go`) rejects a relative `rootPath`
> outright, which degrades to that storage location being silently skipped rather than a fatal
> error.

Neither `exiftool` nor `ffprobe` is installed on every machine that runs these tests (notably:
the CI Go job doesn't install them). `internal/probe`'s integration tests `t.Skip` cleanly when
the binaries are absent rather than failing -- that's intentional, not a gap to fix.

### Data layer (sqlc)

```sh
# After editing internal/db/migrations/*.sql or internal/db/queries/*.sql:
sqlc generate

# internal/db/sqlcgen/ is committed -- ci-go.yml has no codegen step, so
# `go build ./...` in CI depends on the generated code already being current.
```

See `docs/schema.md`'s sqlc risk note before writing a recursive CTE query: sqlc's SQLite
parser needs every column in a recursive CTE's anchor `SELECT` explicitly named or aliased
(`SELECT sqlc.arg(x) AS id`, not `SELECT sqlc.arg(x)`) or it fails with `*ast.ResTarget has nil
name`.

**`sqlc generate` (v1.31.1, pinned) silently corrupts unrelated files -- always diff file-by-file
and revert anything you didn't intend to touch.** Running it after editing only
`internal/db/queries/scan_jobs.sql` and `media_edges_resolve.sql` also rewrote
`media_nodes.sql.go` -- a file whose own `.sql` source wasn't touched -- and the rewrite dropped
characters inside embedded SQL string constants (`'ARCHIVED` missing its closing quote,
`lens_model` truncated to `lens_mode`, `WHERE id = ?1` losing its `1`). Reproduced on a clean
`origin/main` checkout with zero edits, so it's a real regen bug in this sqlc version against
this schema, not something introduced by any one PR. The corrupted Go still compiles (the SQL
lives inside a backtick string), so `go build`/`go vet` won't catch it -- only a query that hits
the broken string at runtime would. After any `sqlc generate`, `git diff --stat
internal/db/sqlcgen/` and manually inspect every file you did not deliberately change; harmless
drift (interface method reordering, previously-missing doc comments) is fine to keep, but revert
any file with unexplained diffs and re-add only the query you actually meant to add/change.

**sqlc's SQLite grammar doesn't support `GLOB`, and mixes up `sqlc.arg(name)` numbering when a
plain `?N` placeholder appears earlier in the same query file (#61).** `GLOB` fails outright
(`no viable alternative at input 'GLOB'`) -- use `length(x) = N` / other operators instead. Less
obviously: a new query using `sqlc.arg(name)` placed in a file that *also* has an earlier query
using bare positional params (`?1`, `?2`, ...) fails to parse with a cascade of confusing
"extraneous input ')'" / "no viable alternative" errors that look like a syntax problem in the
new query itself -- reproduced by bisection on a clean, working query in isolation. The fix is to
use plain `?1`/`?2` instead of `sqlc.arg(name)` in that file (matching what most of the codebase
already does), not to debug the SQL. Confirmed as a real generator bug for this sqlc version by
reproducing with a trivial two-query file, not something specific to any one query's shape.

### Frontend (Vite + React + TypeScript)

```sh
cd web
npm ci             # install
npm run build      # typecheck + production build -> web/dist/
npm run dev         # HMR dev server on :5173, proxies /api -> localhost:8080
npm run test        # vitest
npm run test:coverage
npm run lint         # eslint
npm run typecheck    # tsc -b --noEmit
```

`web/src/api/types.ts` and `client.ts` are hand-kept in sync with `internal/httpapi/routes.go`'s
DTOs -- there's no generated client yet (Huma can emit `/openapi.json`, off by default via `http.exposeOpenAPI`; wiring a generator is a
reasonable follow-up, not done in increment 1).

### Docker

```sh
# Production-like (pulls GHCR image)
docker compose up -d

# Dev (live source mount + Vite HMR)
docker compose -f compose-dev.yaml up --build

# Build the image locally
docker build -t branchdam:dev .
```

## Architecture

### Data flow (a scan)

```
POST /api/v1/scan {storageLocationId}
    │
    ▼
pipeline.RunScan -- creates a scan_jobs row, returns the job id immediately
    │
    ▼ (background goroutine)
indexer.Walk(root)  -- Lstat only, never opens a file
    │  submits one workers.Job per file
    ▼
workers.Pool  -- bounded goroutines, per-path dedup, backpressure via Submit returning false
    │  each job, off the HTTP/watcher thread (spec directive 9.2):
    ├── storage.Guard.OpenRead        -- refuses Tier 3 writes before any syscall
    ├── hashing.FastHash              -- always
    ├── hashing.FullHash              -- if needsFullHash(policy, tier3, collision)
    ├── probe.Exif / probe.FFProbe    -- if the binaries are present; graceful skip if not
    ▼
pipeline.drainAndCommit  -- batches every 64 results or 250ms into one pipeline.Commit
    │  (single write transaction, db.InTx -- see Key invariants)
    ├── version collision  -> archive old node, insert new, link superseded_by
    ├── move detection     -> MISSING node's fast_hash reappears elsewhere -> rebase in place
    └── new/unchanged      -> insert / touch
    │
    ▼ (per committed node, same batch)
graph.Engine.ResolveAndCommit
    │  runs every registered Resolver, merges candidates, cycle-checks (WouldCreateCycle),
    │  commits survivors -- never downgrades a human CONFIRMED/REJECTED review_state
    ▼
sse.Hub.Broadcast()  -- coalescing nudge; the SPA re-fetches via TanStack Query, not
                         by parsing the SSE payload (see internal/sse's package doc)
```

### Go packages (`internal/`)

| Package | Responsibility |
|---|---|
| `config` | Load + env-expand `config.yaml`; `${VAR}` resolved at load time |
| `db` | The two SQLite pools (single-conn writer, multi-conn read-only reader), `ConnectHook` pragmas, embedded goose migrations. Nothing outside this package gets a raw `*sql.DB` |
| `db/sqlcgen` | Generated typed data layer. Never hand-edited |
| `storage` | `Guard` -- the only place a filesystem write may originate; resolves tier from `storage_locations`, refuses Tier 3 before any syscall, defeats symlink escapes |
| `hashing` | Pure functions: `FastHash` (xxHash64), `FullHash` (BLAKE3-256), `PerceptualHash`/`HammingDistance` (DCT). No file I/O |
| `probe` | `exiftool`/`ffprobe` subprocess wrappers, fixed argv allowlist, graceful `ErrToolUnavailable` when a binary is missing |
| `indexer` | `Walk` (directory scan) and `Watch` (fsnotify), both Lstat-level only |
| `workers` | Generic bounded goroutine pool: fixed worker count, bounded queue, per-key dedup, non-blocking `Submit` |
| `pipeline` | Owns the only write access to `media_nodes`: `Commit`'s collision/move/insert logic, `RunScan`'s orchestration, the join point that calls `graph.Engine` after each committed batch |
| `graph` | Tier-2 edge resolvers (`XMPOriginalDocumentIDResolver`, `FilenameStemResolver`), `Engine` merges candidates and commits inside one transaction with a cycle guard |
| `auth` | `Principal` + `BrowserChain`/`AgentChain`/`Route` -- the *only* code permitted to read `X-Authentik-*` (enforced by `TestNoDirectAuthentikHeaderReads`, not just convention) |
| `sse` | Coalescing-nudge `Hub`, copied verbatim from `traefik-viewer/internal/sse/hub.go` |
| `httpapi` | Huma v2 route registration, middleware chain, SSE handler (registered directly on the mux, not through Huma), SPA fallback |

### Frontend (`web/src/`)

- `api/client.ts`, `api/types.ts` -- thin fetch wrapper + hand-kept-in-sync DTOs
- `hooks/queries.ts` -- TanStack Query hooks per endpoint
- `hooks/useEventStream.ts` -- SSE subscription; invalidates query keys on every nudge, never parses the event payload
- `pages/` -- `AssetListPage`, `AssetDetailPage` (metadata inspector + one-hop lineage graph), `AuditQueuePage` (Confirm/Reject)
- `components/AssetGraphCanvas.tsx` -- `@xyflow/react` rendering of `/api/v1/assets/{id}/graph`'s one-hop response

## Key invariants

- **No triggers, no `ON DELETE CASCADE`, anywhere in the schema.** The original spec's DDL had
  both, and both were unsound (see `docs/schema.md` fixes #4, #5, #6). Every FK is `RESTRICT`; a
  vanished file sets `lifecycle_state = 'MISSING'`, rows are never deleted. `updated_at` is set
  explicitly in every query, never by a trigger. Parent-liveness (`parent_missing` /
  `parent_alive`) is a computed view column (`v_media_edges_resolved`), not a stored,
  triggerinvalidated one.
- **`PRAGMA foreign_keys = ON` is what makes the RESTRICT-not-CASCADE fix real.** It defaults
  off in SQLite and is connection-scoped, not persisted in the DB header -- `internal/db`'s
  `ConnectHook` sets it on *every* pooled connection, both writer and reader. Forgetting this on
  a new connection path silently makes every FK a no-op.
- **All filesystem writes go through `storage.Guard`.** No other package calls `os.Create`,
  `os.WriteFile`, `os.MkdirAll`, or `os.Remove` on a path that could be under a configured
  storage location. `Guard.CheckWrite` resolves the canonical (symlink-resolved) path against
  `storage_locations` and refuses a read-only tier with a typed `*ErrReadOnlyTier` before any
  syscall -- a `:ro` bind mount alone only surfaces `EROFS` at whatever call depth first happens
  to touch it. `internal/prune.Execute` (#61) is `Guard.Remove`'s first and, by design, only
  production caller -- every purge still resolves through `CheckWrite` first, so a symlink from a
  prunable Tier-1 location into Tier 3 is refused the same way any other Guard caller is.
  **`internal/thumbs.Cache` is the one deliberate exception**: it writes JPEGs under `/data/thumbs`
  (`thumbnails.cacheDir`), which is app-owned state on the same volume as `branchdam.db`, not a
  path under any configured `storage_locations` tier -- `Guard.Resolve` returns
  `ErrUnknownLocation` for anything outside `storage_locations`, so routing the cache through
  `Guard` is not just unnecessary but impossible. Reading the *source* media a thumbnail is
  generated from still goes through `Guard.OpenRead`; only the cached JPEG's own
  `os.CreateTemp`/`os.Rename`/`os.Remove` bypass `Guard`.
- **A Tier-1 cache file is only prunable with a verified Tier-3 master.** `ListPrunableNodes`
  (#61) requires a *live* ancestor (`lifecycle_state IN ('ACTIVE','HIDDEN')`, walked via
  `media_edges` target→source, `REJECTED` edges excluded) on a `TIER3_MASTER_ARCHIVE` location
  with a non-NULL 64-length `full_hash` -- a MISSING or ARCHIVED master, or one with no verified
  hash, never authorizes a purge. TTL itself is config-only (`StorageLocation.CacheTTLHours`,
  compared against `mtime_unix`, not `last_seen_at`) -- `prunable: true` alone never makes a node
  eligible.
- **`prune.Execute` re-verifies both the DB and the disk immediately before deleting, not just
  at `Plan` time.** Two independent TOCTOU windows exist between a dry-run `Plan` and a later
  `Execute`: (1) DB-side -- the Tier-3 ancestor's verified-hash status can change (master goes
  MISSING, hash invalidated) between the two calls, closed by re-running `Plan` itself inside the
  same transaction as the delete, aborting with `ErrNoLongerEligible` if the candidate no longer
  appears; (2) filesystem-side -- `media_nodes.mtime_unix` is only as fresh as the last scan/sweep
  that touched the node, so the row can look eligible while the real file was modified or
  regenerated moments earlier with no scan yet observing it. This is closed by an `os.Lstat`
  (deliberately not `os.Stat` -- same symlink-defeat direction as `Guard.CheckWrite`'s own
  canonicalize step) taken immediately before `guard.Remove`: an `(mtime, size)` mismatch against
  what `Plan` recorded aborts with `ErrFileChangedSincePlan` and leaves the file untouched; a file
  already gone on its own (`fs.ErrNotExist`) is not an error -- the node still lands in `MISSING`,
  since there is nothing left for `Guard` to remove either way.
- **Identity is read in exactly one place.** `internal/auth.BrowserChain` is the only code
  permitted to reference `X-Authentik-*`. `AgentChain` deletes those headers unconditionally
  (before even checking the API key) on the router that bypasses Authentik ForwardAuth by
  design -- `TestNoDirectAuthentikHeaderReads` greps the rest of the repo and fails the build if
  anything else reads them directly.
- **The writer pool is a single connection, on purpose.** `db.DB`'s writer has
  `SetMaxOpenConns(1)` -- that's what makes `graph.Engine`'s cycle-check-then-insert
  (`WouldCreateCycle` inside the same transaction as the edge upsert) sound without any
  additional application-level locking. Never raise this without re-deriving that invariant.
- **A human review decision outranks any resolver, permanently.** `UpsertMediaEdge`'s `ON
  CONFLICT ... DO UPDATE ... WHERE review_state NOT IN ('CONFIRMED', 'REJECTED')` means a
  `CONFIRMED`/`REJECTED` edge is never touched by a later automated re-resolve, no matter how
  confident the new candidate is.
- **`fast_hash` (xxHash64, 16 hex) is a cheap remap key, never a duplicate-detection oracle by
  itself.** `full_hash` (BLAKE3-256, 64 hex) is what backs real integrity verification. The
  schema's own length `CHECK`s make it structurally impossible to write a 64-bit value into the
  `full_hash` column -- this caught a real test-fixture bug during PR 6's development (a
  placeholder `"aa"`/`"bb"` full_hash instead of a real 64-char value). If you see this
  constraint fire, the fixture is wrong, not the constraint.
- **Version collisions archive-then-insert, never insert-then-archive.** The partial unique
  index on `media_nodes(file_path)` only excludes `ARCHIVED` rows -- archiving the old row
  first is what keeps a live old row and a live new row from ever coexisting at the same path,
  even for an instant within the transaction.
- **A manually-triggered differential scan (`POST /api/v1/scan {differential:true}`, #226,
  Tier-3 only) is allowed to run concurrently with a `FULL_SCAN` against the same location --
  this is a deliberate, safe gap, not an oversight.** `createScanJob`'s already-running guard
  (#163, widened by #226) is same-kind-only: it blocks two concurrent `FULL_SCAN`s or two
  concurrent `INCREMENTAL`s, but not a `FULL_SCAN` racing a differential `INCREMENTAL`, which
  couldn't happen before #226 since a manually-triggered `INCREMENTAL` didn't exist. The safety
  net for that combination is `TouchMediaNode`'s own `CASE WHEN lifecycle_state = 'MISSING'
  THEN 'ACTIVE' ELSE lifecycle_state END` (`internal/db/queries/media_nodes.sql`): if a
  concurrent `FULL_SCAN`'s version collision archives a node between the differential sweep's
  `sweepUnchanged` check and its deferred `touchBatcher` flush, the flush's `UPDATE` lands on a
  now-`ARCHIVED` row id and is a no-op on `lifecycle_state` (the `ELSE` branch) -- never a
  resurrection into a live duplicate alongside the `FULL_SCAN`'s freshly inserted successor.
  See `TestConcurrentFullScanArchiveDoesNotResurrectViaDifferentialTouch`
  (`internal/pipeline/sweep_test.go`) for the regression test.
- **The asset graph endpoint is one hop, deliberately.** `GET /api/v1/assets/{id}/graph`
  returns direct parents/children only, not a bounded recursive traversal. This is a stated
  scope line for increment 1, not a hidden gap -- see `internal/httpapi/routes.go`.
- **Auto-accept thresholds are derived per resolver tier.** Tier 1 and Tier 2 use `0.90` (preserving existing classification behavior), while Tier 3 uses `0.85` (so Tier 3's 0.70–0.89 confidence band splits into 0.85–0.89 auto-accepted and 0.70–0.84 audit queue). `needsReviewFloor = 0.50` applies across all tiers.
- **A `filename_stem` match derived from an index suffix ("-N", "(N)") never auto-accepts on
  its own, permanently (#132).** `internal/naming` is the single owner of the stem/suffix
  definition (`Stem`, `Kind`) shared by `internal/pipeline` (which computes and stores
  `media_nodes.filename_stem` at insert time, unchanged since PR #134's H3 fix) and
  `internal/graph` (whose `FilenameStemResolver` classifies both sides of a match). An index
  suffix asserts "I am a duplicate of some other file" -- PR #134 only bounded how far that
  could over-collapse (`-\d{1,2}`, not unbounded `-\d+`), it didn't close the class: an
  unpadded 1-2 digit hyphen scheme (`trip-1.jpg`..`trip-45.jpg`) or an OS `(N)` duplicate index
  still collapses siblings to a shared stem. `FilenameStemResolver` now requires a live, bare
  anchor node (`naming.SuffixNone`) as the parent before emitting a candidate at all, requires
  the index-suffixed node to be the child, and caps a surviving match at `0.89` -- strictly
  below `AutoAcceptThresholdForTier(2)` -- so it always lands in the audit queue unless a
  non-`filename_stem` resolver corroborates the same edge (`mergeCandidates` already takes max
  confidence per `(parent, child, rel)`; no new merge logic needed). Role suffixes (`_edit`,
  `_proxy`, `_vN`, `` copy``) are unaffected -- collapsing those siblings to a shared stem is
  the resolver's original intended case. Migration `00006` is a one-time data correction for
  edges already written `AUTO_ACCEPTED` before this fix existed; `UpsertMediaEdge`'s
  confidence-only-increases rule means a rescan alone would never have fixed them.
- **All seven promoted `media_nodes` columns refresh on the touched/rebased branches, not just
  at insert (#197, #204).** `original_document_id`, `document_id`, `derived_from_id`,
  `camera_model`, `camera_serial`, `lens_model`, and `captured_at_unix` were historically set
  exactly once, by `insertNewNode` -- no `UPDATE` anywhere touched them, so an in-place metadata
  write (`POST /api/v1/assets/{id}/inherit-metadata`) that only refreshes `fast_hash` (#188)
  left them frozen at their insert-time values forever. `internal/pipeline.reconcileAllMetadata`
  -> `reconcilePromotedColumns` now refreshes all seven from a freshly-probed `Result` on both
  the Touched branch and the rebase/move branches, using the same non-empty-only, diff-first
  contract as the `node_metadata` reconcile (a probe that ran but found nothing, or never ran at
  all, must not clear a stored value). `PersistExifMetadata` (the inherit-metadata endpoint's
  own backfill) deliberately promotes none of them -- the next scan's Touched branch is the one
  write path that owns promotion, since #188 keeps `fast_hash` in agreement so that scan takes
  the Touched branch rather than colliding. Because `UpsertMediaEdge` only ever raises
  confidence (`MAX(excluded, stored)`), re-resolution is monotone and one-directional: promoting
  a node's own `captured_at_unix` lets *that node's* next resolve see it, but a peer that
  already resolved against its old value is not re-run, so a changed `captured_at_unix` can
  leave an already-committed Tier-3 edge stale with no automatic repair. No data-correction
  migration is needed for this, unlike #132's `00006` -- a stale or `NULL` `captured_at_unix` is
  repaired by the node's own next Touched pass, whereas an edge already written
  `AUTO_ACCEPTED` never self-corrects via rescan.

## CI

- **`ci.yml`**: the Go job runs unconditionally; a `detect` job's step computes `hashFiles()`
  results and publishes them as outputs, which the frontend and Docker jobs' `if:` conditions
  read -- `hashFiles()` is only callable from `jobs.<id>.steps.*`, not directly in a job-level
  `if:`, which is why the detection is a separate job rather than inline. This is what lets both
  jobs skip cleanly (not fail) until their inputs exist. Docker builds each platform natively
  (no QEMU) per `docker-publish.yml`. On `pull_request` events the Docker job additionally only
  runs when a `dorny/paths-filter` step in `detect` finds a Docker-relevant path changed
  (`Dockerfile`, `.dockerignore`, `go.mod`, `go.sum`, `web/package-lock.json`) -- most PRs skip
  the image compile-check entirely. `push` events (merge to main) are unaffected by that filter;
  the edge build+push always runs there. `golangci-lint` runs as its own job (not inside the
  shared `ci-go.yml`), pinned to a specific v2.x release matching `.golangci.yml`'s
  `version: "2"` schema.
- **`release.yml`**: `release-please` maintains a release PR on every push to `main`; merging it
  cuts the tag and triggers the multi-arch release image push. **Its PR never gets a CI check
  reported** -- `release-please-action` opens/updates that PR using the default `GITHUB_TOKEN`,
  and GitHub's Actions recursion guard means refs/PRs created by `GITHUB_TOKEN` don't trigger
  `on: push`/`on: pull_request` workflows (the same mechanism that makes chaining
  `docker-publish.yml` off `release-please`'s own tag impossible -- see the comment in
  `release.yml`). With branch protection's required status checks in place (see below), that PR's
  merge button reads as blocked/"Expected" forever, not just slow. This is expected, not a bug:
  merge it via the "merge without waiting for requirements" path -- `enforce_admins: false` is
  what makes that available to the repo owner. A real fix (passing an app/PAT token to
  `release-please-action`, via `s3ntin3l8/.github`'s `scripts/app-token.sh`, so its PRs get a
  real trigger) is a shared-workflow change, not something to work around per-repo.
- **`codeql.yml`**: scans `go,javascript-typescript` -- CodeQL hard-fails (not a graceful no-op)
  if a requested language has zero source files, which is why this was Go-only until the SPA
  (PR 10) landed real frontend source.
- **`Docker image build` can fail on a transient `ghcr.io` login denial, unrelated to code.**
  Observed 2026-08-18 (run 32141578050, merge commit `2632ba1`): the `linux/amd64` leg's
  `docker/login-action` step failed with `Error response from daemon: Get "https://ghcr.io/v2/":
  denied: denied`, while the `linux/arm64` leg of the *same run*, same credentials, logged in
  successfully 0.6s later -- the only such failure in the prior 100 `push`-to-`main` runs. Because
  `merge` is `needs: [prepare, build]`, one denied leg skips the manifest-list job entirely and
  silently leaves `:edge` stale; the fix (a bounded `docker login --password-stdin` retry loop
  replacing `docker/login-action` in both the `build` and `merge` jobs) is in the shared
  `s3ntin3l8/.github`'s `docker-publish.yml` (PR #48), not this repo -- same "shared-workflow
  change, not a per-repo workaround" pattern as `release.yml` above. Not one of the five required
  branch-protection checks, so it never blocks a merge; if it recurs, check whether the newest
  `main` push already republished `:edge` before considering a re-run -- re-running an old failed
  run re-tags with that run's (stale) `github.sha` and can move `:edge` **backwards** if a newer
  push has since published.
- **Branch protection on `main`**: required status checks (`Go (build · vet · test) /
  lint-and-test`, `Web (typecheck · build) / lint-and-test`, `CodeQL`,
  `review / dependency-review`, `golangci-lint`), no required reviews (solo repo),
  `enforce_admins: false`, force-push and branch deletion both blocked. `strict` is `false`
  (branches need not be up to date with `main` before merging) so Dependabot PRs don't need a
  rebase on every `main` advance to merge.
- **`codecov.yml`**: both `codecov/patch` and `codecov/project` are `informational` (no agreed
  coverage floor yet), with `go`/`node` flags scoped by path matching what the shared
  `ci-go.yml`/`ci-node.yml` workflows already upload coverage under. Codecov reads this file from
  the default branch, not from whichever PR introduces it -- a PR editing `codecov.yml` is itself
  still evaluated under whatever config was already on `main`.
- **`hermes.yml`**: automated PR review by the `s3ntin3l8-hermes[bot]` GitHub App, via the
  shared `s3ntin3l8/.github/.github/workflows/hermes-review.yml@main` reusable workflow. The
  self-hosted runner (`gh-runner-01-branchdam-01`) does not run the review agent itself -- it
  mints a GitHub App token (for trigger routing only), checks the mention, and relays the prompt
  over the LAN to a Hermes API server on a separate host (hermes-01); the agent executes there
  and posts the review back to GitHub with its own token. This is why a LAN-reachable
  self-hosted runner is required and a GitHub-hosted runner cannot substitute. Two triggers:
  `auto-review` fires once per PR (`opened` if non-draft, `ready_for_review` if it started as a
  draft), `on-demand-review` fires on an `@s3ntin3l8-hermes` mention, gated to
  MEMBER/OWNER/COLLABORATOR commenters. Deliberately **not** subscribed to `synchronize`
  (produced 5 separate review submissions across 5 pushes on a mullion PR before this was
  learned) or to `pull_request_review`/`pull_request_review_comment` (a submitted review's body
  routinely contains the mention string, which would self-trigger). Not in branch protection's
  required checks -- a review bot going down shouldn't block every merge. `auto-review` is
  additionally gated to `head.repo.full_name == github.repository` -- this repo is public and
  the runner is self-hosted, so without that guard a fork PR's own copy of the workflow file
  (which a `pull_request` event runs) could execute arbitrary steps on the homelab runner. Found
  by hermes's own review of the PR that introduced this workflow (#76) and fixed same-day.

## Documentation map

- [`CONTRIBUTING.md`](CONTRIBUTING.md) -- setup, the pre-PR checklist (and precisely what
  `make lint` does and doesn't cover), codegen contracts, PR title / branch protection rules.
- [`docs/spec/original-spec.md`](docs/spec/original-spec.md) -- the design spec as received,
  committed verbatim. A historical input, not the live contract; where it disagrees with the
  code, the code is right.
- [`docs/schema.md`](docs/schema.md) -- the itemized deviation ledger against that spec (nine
  numbered fixes) and the sqlc risk note.
- [`docs/forward-auth.md`](docs/forward-auth.md) -- the Authentik/Traefik walkthrough.
- [`docs/deploy.md`](docs/deploy.md) -- first-deploy runbook: Authentik/Traefik setup sequence,
  `compose.override.yaml`, `config.yaml`/`.env`, bring-up, first scan.
- [`docs/configuration.md`](docs/configuration.md) -- field-by-field reference for every
  `config.yaml` key, including the two (`pathRewrites`, `workers.perceptualHash`) missing from
  `config.example.yaml` itself.
- [`docs/operations.md`](docs/operations.md) -- day-2: background workers, upgrades, backup/restore,
  the pruning runbook, a troubleshooting table.
- [`docs/roadmap.md`](docs/roadmap.md) -- the phased plan for everything after increment 1: the
  spec's remaining pillars plus the built-but-unwired surface increment 1 left connected to
  nothing. Filed as GitHub issues per phase, worked through mullion task master. Phases 0-9 are
  landed; only phase 10 (workstation agent) remains open.
- [`docs/agent-protocol.md`](docs/agent-protocol.md) -- ADR: REST vs. gRPC for the phase-8 agent
  event stream.
- [`docs/project-paths.md`](docs/project-paths.md) -- Tier-1 project-file path resolution
  strategy (`pathRewrites`, ambiguity policy, missing-node handling).
- [`docs/dam-manifest.md`](docs/dam-manifest.md) -- the `.dam.json` project manifest schema.
- [`docs/google-photos.md`](docs/google-photos.md) -- Google Photos push feasibility spike,
  resolved as a no-go (branchDAM's sync layer has no byte-transfer capability, not a Google-side
  restriction).
