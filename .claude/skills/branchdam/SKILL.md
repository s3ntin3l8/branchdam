---
name: branchdam
description: branchDAM's non-obvious correctness invariants. These are changes that look fine on their own but break something another package relies on, such as the single writer connection, storage.Guard, clock determinism, edge review precedence, prune authorization, agent trust, browser auth/authz, sqlc codegen traps, DTO/web sync and the SPA's live-refresh and theme contracts. Read this before changing Go code under internal/, a migration or query, an agent or web route DTO, auth or settings code, the web/ SPA, or anything that touches files on disk or time. Also read it when reviewing such a change.
---
<!-- mullion:scaffold:branchdam -->

# branchDAM invariants

Each rule below describes a mistake that compiles, often passes a narrow test, and still breaks something elsewhere. File paths are the places that enforce or explain each rule.

## 1. Database: one writer, transactions only, nothing deleted

- **All writes go through `db.DB.InTx` / `InTxResult`.** Reads use `DB.Reader` or `ReadTx`, and the reader pool is `query_only=ON`. Never add an *exported* function, method or struct field (embedded ones included) that hands out `*sql.DB`, `*sql.Tx`, `*sqlcgen.Queries` or `sqlcgen.DBTX`. `internal/db/no_leak_test.go` fails on that. `DB.Reader` is the only allowlisted field.
- **Never open a transaction inside an `InTx` closure, directly or through a helper.** The writer pool has `SetMaxOpenConns(1)`, so a nested `InTx` deadlocks. Code that is already inside a transaction calls the `*Tx` variant and passes `q` through. For example, the agent drainer calls `pipeline.TrashAssetTx`, not `pipeline.TrashAsset`. If you add a public helper that opens its own transaction, add a `...Tx(ctx, q, ...)` sibling too.
- **Don't hold the writer connection across filesystem I/O.** `Lstat`, `Remove`, `ReadDir` or `Open` on an NFS/SMB mount can block for minutes and stall every write in the process. Stat and unlink outside the transaction, then re-check state on the writer. `internal/prune/no_fs_in_transaction_test.go` enforces this shape for `internal/prune/engine.go`. Apply the same rule everywhere else.
- **When files move and a transaction also commits, the two must agree when the transaction fails.** `pipeline`'s `moveLog` (`WithMoveLog`/`undo`) puts moved files back on rollback, including a rollback that happens *after* the inner function returned nil. If you add a disk mutation next to a transaction, record it in the move log.
- **Media lineage rows (`media_*`) are never deleted, and there are no triggers and no `CASCADE`.** (Auxiliary tables such as `app_settings`, `mfa_credentials`, `device_pairings` and `full_hash_provenance` do have `DELETE` queries; don't widen the rule to them.) Every FK is `RESTRICT`, and `foreign_keys=ON` is set per connection in `ConnectHook`. Model disappearance as a `lifecycle_state`:
  - `MISSING` means a scan found the file gone.
  - `TRASHED` means the user or agent deleted it.
  - `ARCHIVED` can't be trashed until it is restored.

  Don't mix these up. A new migration that adds `ON DELETE CASCADE`, a trigger or a `DELETE FROM media_*` breaks lineage. Table rebuild migrations (see `00032`) must keep the data invariants.
- **Timestamps:** SQLite's `unixepoch()` is replaced per connection by `internal/clock` (`registerClock`). Use only the zero-argument form in schema and queries. Unlike the built-in, it is read at each call, so two `unixepoch()` columns in one statement can differ by a second.

## 2. sqlc and migrations

- Files in `internal/db/queries/*.sql` must be **ASCII-only, comments included**. sqlc v1.31.1 corrupts every later statement after a multi-byte character. `TestQueryFilesAreASCII` enforces this.
- In a recursive CTE, the anchor `SELECT` must alias every column (`SELECT sqlc.arg(x) AS id`). Otherwise you get `*ast.ResTarget has nil name`.
- After editing a migration or query, run `sqlc generate` and commit `internal/db/sqlcgen/`. The `sqlc-diff` hook does nothing when `sqlc` isn't installed, and CI has no codegen step, so a stale `sqlcgen/` breaks `go build` on `main`.
- A table-rebuild migration must be `-- +goose NO TRANSACTION` (`PRAGMA foreign_keys` is a no-op inside a transaction), run its own `BEGIN`/`COMMIT`, and verify `pragma_foreign_key_check` through a CHECK-constrained temp table (a bare PRAGMA's rows are discarded). It must recreate every partial index that filters on `lifecycle_state`. Adding a lifecycle state means widening every `NOT IN ('ARCHIVED','TRASHED')` predicate. See `00032`.
- Check the guards in a correlated `UPDATE ... SET (subquery) WHERE id IN (...)` before simplifying them. Several backfill migrations (`00027`, `00039`) say their guard is load-bearing. In optional-filter clauses, write `col = sqlc.narg(x) OR sqlc.narg(x) IS NULL`, comparison first: sqlc infers the parameter type from the comparison, and leading with `IS NULL` turns every parameter into `interface{}` (`media_nodes.sql`).

## 3. Filesystem: everything goes through `storage.Guard`

- Every write to a storage-location path uses a `Guard` method: `Create`, `CreateExcl`, `WriteFile`, `MkdirAll`, `Remove`, `Rename` (both ends are checked), `Link` (destination is checked), or `Chtimes`. A bare `os.Create`/`os.Rename`/`os.Remove` on a media path bypasses the read-only tier check.
- Reads of media use `Guard.OpenRead`.
- The only sanctioned `os.*` write paths outside Guard are app state on the `/data` volume: the thumbnail cache (`internal/thumbs`, which still reads source media through `Guard.OpenRead`) and the database.
- Tier semantics:
  - Tier 3 (`TIER3_MASTER_ARCHIVE`) is *writable* by default for server-governed ingest. A user or agent **trash** of a Tier-3 or read-only node is still logical only: the row becomes `TRASHED` and the bytes stay where they are (`pipeline/trash.go`).
  - Virtual locations have no bytes and match by lexical prefix.
  - `Guard.ReloadLocations` refuses locations without a row id (`ID > 0`).
- `indexer.Walk`/`Watch` use `Lstat` only and never follow symlinks into traversal. Keep it that way.

## 4. Time and IDs go through `internal/clock`

- Use `clock.Now()`/`clock.Unix()` for any time that is stored, compared with a stored value, or shown.
- Use `clock.Real()` for deadlines, timeouts, durations, rate-limit windows and nonces. A frozen demo clock never advances, so using `Now` there hangs or breaks those.
- Use `clock.NewV7()`, never `uuid.NewV7()`. With the clock frozen, it produces deterministic, ordered UUIDs.
- `forbidigo` rejects `time.Now` and `uuid.NewV7` outside `internal/clock`. `_test.go` files are exempt, so this lint doesn't catch test helpers that leak into production code.
- Determinism matters because the asset list and audit queue are ordered by id, and the demo's visual baselines depend on a scan with one hash worker producing the same node and edge order on every run. Don't add concurrency or randomness to the scan's insert order.

## 5. Lineage edges: human review wins

- Human `CONFIRMED`/`REJECTED` permanently outranks any resolver or agent.
  - `UpsertMediaEdge`'s `ON CONFLICT ... WHERE review_state NOT IN (...)` returns **no row** for a human-locked edge. Through `:one`, that surfaces as `sql.ErrNoRows`, which means "intentionally untouched, re-fetch and continue". Never treat it as a failure that rolls back the batch.
  - `CreateMediaEdge` deliberately has no `ON CONFLICT`. In the agent path, a UNIQUE violation is swallowed as idempotent success. Don't "fix" either query into an unconditional upsert.
- `confidence` only ever goes up (`MAX`). A later resolve must not lower it.
- Run the cycle check (`ErrWouldCreateCycle`) before every edge insert, on the writer connection, in the same transaction. The schema has no DAG constraint.
- An index-suffixed stem node may never be the parent of a stem match (#132). Index-suffix stem matches are capped below auto-accept.

## 6. Prune and "verified": only server-computed hashes count

- Tier-1 pruning (and agent `node-status` `verified`) needs **all** of the following:
  - a live Tier-3 master whose 64-character `full_hash` has `full_hash_provenance.source = 'SERVER'`;
  - a path to it only through `CONFIRMED`/`AUTO_ACCEPTED` edges of type `PROXY_OF`, `DERIVED_FROM` or `DUPLICATE_OF`;
  - every node on that path `ACTIVE` or `HIDDEN`.
- `NEEDS_REVIEW`, `PROJECT_SIDECAR` and `FINAL_EXPORT` never authorize a purge. Project files and `.xmp` sidecars are never candidates.
- Whenever code writes a new `full_hash`, call `db.RecordFullHashSource` **in the same transaction** with the correct source:
  - `HashSourceServer` for a scan, an upload stream or a rehash match;
  - `HashSourceAgent` for anything a device asserted, including `/agent/rebase`.

  A missing or wrong provenance row either blocks pruning silently or lets a device authorize deleting the last copy.
- `prune.Execute` re-runs `Lstat` and checks mtime/size for candidates and Tier-3 ancestors, then re-reads the verified ancestor set on the writer immediately before `Guard.Remove`. If the set changed, it abandons the purge. Never cache the ancestor set across that boundary.
- The `.trash/` retention purge ages files by `node_trash_times.trashed_at`, not by file mtime. Nothing in that path may turn into an early unlink.

## 7. Auth: agent input is untrusted, browser authz is per route

- Ignore `storage_location_id` from DTOs and re-derive it with `Guard.Resolve(filePath)`.
- Devices name nodes by **UUID only**. Payloads that carry integer `sourceNodeId`/`targetNodeId` are refused, because enumerable ids would let a device name nodes it was never told about. Ownership path segments must not contain separators or free text (`agent/ownership.go`).
- A device may only trash or mutate nodes it created (`RequireNodeOwner`). A UUID that was content-deduplicated onto a canonical node (`aliased`) must **never** act on the canonical node.
- Agent edges:
  - `tier` is clamped to [1, 2].
  - If the device did not create the *target* node, the edge is stored as `NEEDS_REVIEW` whatever its confidence.
  - Otherwise `review_state` is derived exactly as `graph.Engine` derives it.
- Only `internal/auth.BrowserChain` reads `X-Authentik-*` headers (`TestNoDirectAuthentikHeaderReads`). Agent routes strip them unconditionally, and PAT principals never provision a `users` row.
- In `auth.mode=both`, `X-Authentik-*` is attacker-controlled unless the request came through a trusted proxy (`forwardTrust` in `auth/route.go`); the forward chain is skipped for untrusted sources. Local sessions decide admin by `users.is_admin` only, never by groups or the empty-groups fallback.
- The agent request signature (`method\nrequestURI\nnonce\ntimestamp\nbody`) is a contract with branchdam-agent and branchdam-mobile. Streamed-body endpoints belong in `defaultSkipSignaturePaths` (`auth/agent.go`).
- **Browser authz:** `auth.RequireAdmin` lets every GET/HEAD/OPTIONS through for any authenticated principal and every `KindMachine` request through entirely. A route that reads secrets or admin-only data, or must refuse devices, calls `auth.IsAdmin` with its own kind and method checks (as the settings routes do).
- **No state change on GET.** `crossSiteGuard` (`httpapi/csrf.go`) only checks POST/PUT/PATCH/DELETE, and `SameSite=Lax` sends the session cookie on cross-site GET navigations.
- **PAT scopes:** a new scope goes into both `patScopeFor` (`httpapi/server.go`) and the allowlist in `auth/pat.go`. Unlisted route groups fall through to `admin`, and a PAT never short-circuits `/api/v1/agent`.

## 8. Wire contracts that are synced by hand or append-only

- Agent API errors use the `internal/apierr` envelope. Codes are **append-only**: never change a published code's meaning or HTTP status. Clients classify errors by `Code`, never by `Detail`. Every new code goes into `docs/agent-api.md`'s table.
- Agent request and response DTOs: after a change, run `make contract-fixtures` and commit `docs/contract/fixtures`. Fixtures must set every field (`requireAllFieldsSet`), so a new field without a fixture value fails the test.
- Browser DTOs have no generated client: any `json:` struct served under `/api/v1` outside `/agent` (in `routes.go`, `settings.go`, `pats.go`, `local_auth.go`, `mfa_handlers.go`, `companion_pairings.go`, `storage_locations.go`, `web_upload.go`, `password_reset.go`, ...) is hand-mirrored in `web/src/api/types.ts` and `client.ts`. Update them in the same change.

## 9. Scans, settings, secrets

- The `MISSING` sweep runs only after a **clean** walk. A walk error or cancelled context must never mark files missing; that is the data-loss mode `ScanDeps.WalkFn` exists to test. Paths that were seen but are uncertain (stale `last_seen_at`, sent to the pool) are excluded from the sweep.
- Settings overrides (`app_settings`) sit on top of `config.yaml`.
  - A bad or since-invalidated override must fall back instead of stopping the server from starting.
  - Every registry field declares `ApplyLive`, `ApplyRestart` or `ApplyNever` (not settable here, `Editable:false`), and that declaration must match what the code actually re-reads.
  - An `app_settings` row *existing* is the override, even with value `""` (an operator blanking a field must beat a populated env var). Never add an "is it non-empty" fallback.
  - `Field.Set` assigns; it never mutates a slice or map in place, which would alias the base config.
  - Changing a URL that a stored secret is sent to must require re-entering the secret in the same request (the `immich.apiUrl` / `immich.apiKey` check in `settings/store.go`).
  - Storage-location keys are not, and must never become, settings keys.
- Secret settings values are sealed with `secrets.Box` (`BRANCHDAM_SECRET_KEY`). `device_pairings.pairing_url` and `device_pairings.qr_svg` are sealed or NULL, never plaintext (migration `00034_pairing_url_sealed`), and a rotated agent key is delivered once, then cleared.
- User-controlled values (emails, reset tokens, paths from agents) must not reach a `slog` line unsanitized (CodeQL `go/log-injection`); use the existing `sanitizeForLog` helpers (`httpapi/server.go`, `auth/agent.go`).

## 10. Shutdown ordering

- Scans (`ScanTracker.Wait`), watchers (`WatcherSupervisor.Wait`) and the Immich supervisor are joined before `db.Close()`. A new background worker that writes must be joined the same way, and must refuse to start once shutdown begins.
- `POST /api/v1/restart` re-execs only after the HTTP drain, and only for admin groups.
- Per-file background work goes through `workers.Pool`, not ad-hoc goroutines. A job that tracks completion with a `WaitGroup` must set `OnAbandon`, or shutdown's `Wait` hangs on jobs that were queued but never run.
- Background writers attribute to `audit.SystemActor`, which resolves only after `users.EnsureSystemUser` has run at boot (before workers start). Request chains never attach `KindSystem`.

## 11. Web SPA (`web/`)

- The SSE hub only says "something changed, re-fetch" (`sse.Hub.Broadcast`, one coarse channel). A server mutation the UI shows must broadcast, and every TanStack query key that shows server-mutated data must be in `web/src/hooks/useEventStream.ts`'s invalidate list; otherwise it stays stale until its poll or a reload.
- Theme tokens are declared in both `:root[data-theme=dark]` and `light` with distinct values (`web/src/styles/theme.test.ts`). `text-white` and the 50/100/200 accent text stops are banned unless the same class string has an allowlisted solid background, because the light theme inverts the neutrals (`hardcodedTextColors.test.ts`).
- UI changes also need `make visual-check` (AGENTS.md "UI changes").
