# AGENTS.md — branchDAM

This file is the single source of truth for this repo's workflow rules and
load-bearing invariants — the ones every agent needs before touching
anything, regardless of which CLI you are. `CLAUDE.md` is a one-line
`@AGENTS.md` import, so every CLI (Claude Code, Codex, opencode, agy) reads
this file, natively or via that import. See [`CONTRIBUTING.md`](CONTRIBUTING.md)
for the contributor workflow and [`docs/`](docs) for deeper architecture detail.

Self-hosted Digital Asset Management server. Models media assets as a version
node graph with confidence-weighted lineage edges. Go backend (Huma v2, SQLite
WAL, sqlc, goose) + React 19 SPA (Vite, @xyflow/react, TanStack Query, Tailwind).

## Commands

```sh
# Pre-PR Gates
make check       # lint + test + build + golangci-lint
make check-web   # cd web && lint + typecheck + build

# Dev Servers
make dev-api    # Go API only (:8080)
make dev-web    # Vite dev server (:5173, proxies /api -> :8080)
make dev-all    # Both together

# Codegen & Data Layer
sqlc generate    # Run after editing migrations or queries; commit internal/db/sqlcgen/
```

> **`sqlc generate` Risk Note:** Always run `git diff --stat internal/db/sqlcgen/` and inspect files you didn't deliberately change. v1.31.1 has known corruption bugs dropping characters in unrelated files.
> **SQL Syntax Traps:** Use bare positional params (`?1`, `?2`) instead of `sqlc.arg(name)` in mixed files. Recursive CTE anchor `SELECT`s must explicitly alias every column (`SELECT sqlc.arg(x) AS id`).

## Architecture & Package Responsibilities

| Package | Responsibility |
|---|---|
| `internal/config` | Load + expand `config.yaml`; `${VAR}` resolved at load time |
| `internal/db` | SQLite pools (single writer, multi reader), `ConnectHook` pragmas, goose migrations |
| `internal/storage` | `Guard`: strictly resolves tiers from `storage_locations`, refuses writes to read-only tiers before syscall |
| `internal/hashing` | `FastHash` (xxHash64), `FullHash` (BLAKE3-256), `PerceptualHash` (DCT). No I/O |
| `internal/probe` | `exiftool`/`ffprobe`/`ffmpeg` subprocess wrappers; graceful `ErrToolUnavailable` fallback |
| `internal/indexer` | `Walk` (directory scan) and `Watch` (fsnotify), both `Lstat`-only |
| `internal/workers` | Bounded worker pool, per-path dedup, non-blocking `Submit` |
| `internal/pipeline` | Ingestion, collision handling, move detection (`MISSING`/rebase), `Commit` tx |
| `internal/graph` | Edge resolvers (Tier 1 sidecars, Tier 2 stems/XMP, Tier 3 heuristics), cycle checks |
| `internal/auth` | `Principal` + `BrowserChain`/`AgentChain` (sole reader of `X-Authentik-*`) |
| `internal/sync` | `remote_sync_state` machine, Immich library scan trigger worker |
| `internal/prune` | TTL cache pruning engine with strict TOCTOU disk checks |
| `internal/httpapi` | Huma v2 routes, middleware chain, SSE handler |
| `web/` | React 19 + Vite SPA (`@xyflow/react` graph, TanStack Query, Tailwind) |

## Key Invariants

1. **No Triggers, No CASCADE**: Every FK is `RESTRICT`. `PRAGMA foreign_keys = ON` is set on every connection in `ConnectHook`. Missing files set `lifecycle_state = 'MISSING'`; rows are never deleted.
2. **Single-Connection Writer Pool**: `db.DB` writer has `SetMaxOpenConns(1)` to eliminate race conditions during cycle checks and edge insertions.
3. **Filesystem Write Guarding**: All storage writes route through `storage.Guard`. Tier 3 is read-only unless `readOnly: false` is configured; the `:ro` mount remains a defense-in-depth default.
4. **Header Isolation**: `internal/auth.BrowserChain` is the ONLY code permitted to read `X-Authentik-*` headers (`TestNoDirectAuthentikHeaderReads`). Agent routes unconditionally strip them.
5. **Agent Paths Untrusted**: `storage_location_id` on agent DTOs is ignored and re-derived from `storage.Guard.Resolve(filePath)`.
6. **Audit Priority**: Human `CONFIRMED`/`REJECTED` edge review states permanently outrank automated resolvers.
7. **Pruning TOCTOU Protections**: Tier-1 pruning requires a verified, live Tier-3 master with a 64-character `full_hash`. `prune.Execute` re-verifies disk presence (`os.Lstat`) and mtime/size for both candidates and Tier-3 ancestors immediately before calling `Guard.Remove`.
8. **Immich Supervisor**: Immich sync worker joins on graceful shutdown and refuses to start new workers once shutdown begins.
9. **Process Re-exec on Restart**: `POST /api/v1/restart` re-execs the binary (`syscall.Exec`) after graceful HTTP drain, gated to admin groups.

## Review thread resolution

Every review thread (Hermes or human) must be replied to and resolved before
a PR is mergeable. This is a GraphQL-only concept, not a `gh pr` verb:

```sh
# 1. Reply to inline comment (REST)
gh api repos/s3ntin3l8/branchdam/pulls/<PR>/comments/<comment_id>/replies -f body="Fixed in <sha>"
# 2. Resolve thread (GraphQL)
gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "<thread_id>"}) { thread { isResolved } } }'
```
