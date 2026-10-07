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
make check-web   # cd web && lint + typecheck + test + build

# Dev Servers
make dev-api    # Go API only (:8080)
make dev-web    # Vite dev server (:5173, proxies /api -> :8080)
make dev-all    # Both together

# Codegen & Data Layer
sqlc generate    # Run after editing migrations or queries; commit internal/db/sqlcgen/
```

> **`sqlc generate` Risk Note:** `internal/db/queries/*.sql` files must be ASCII-only. sqlc v1.31.1's SQLite engine slices statement spans using rune offsets as if they were byte offsets, so any multi-byte UTF-8 character (even in a `--` comment) corrupts every later statement in the file -- dropped placeholder digits, truncated `RETURNING` lists, stray garbage lines. `internal/db/queries_ascii_test.go`'s `TestQueryFilesAreASCII` enforces this under `make check`; see docs/schema.md's "sqlc risk: non-ASCII in query comments" section and [sqlc#4372](https://github.com/sqlc-dev/sqlc/issues/4372)/[#4523](https://github.com/sqlc-dev/sqlc/issues/4523). Fixed upstream by [sqlc#4535](https://github.com/sqlc-dev/sqlc/pull/4535) but not yet in a release past v1.31.1 -- drop this rule once one ships.
> **SQL Syntax Traps:** Recursive CTE anchor `SELECT`s must explicitly alias every column (`SELECT sqlc.arg(x) AS id`) or sqlc's SQLite parser fails with `*ast.ResTarget has nil name`.

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
| `internal/pipeline` | Ingestion, collision handling, move detection (`MISSING`/rebase), `TrashAsset`/`Commit` tx |
| `internal/graph` | Edge resolvers (Tier 1 sidecars, Tier 2 stems/XMP, Tier 3 heuristics), cycle checks |
| `internal/auth` | `Principal` + `BrowserChain`/`AgentChain` (sole reader of `X-Authentik-*`) |
| `internal/sync` | `remote_sync_state` machine, Immich library scan trigger worker |
| `internal/prune` | TTL cache pruning engine with strict TOCTOU disk checks |
| `internal/httpapi` | Huma v2 routes, middleware chain, SSE handler |
| `internal/agent` | Server side of the agent contract: `event_queue` draining, handshake support, path rebasing |
| `internal/audit` | `actor_audit` log writes/reads for admin actions (scan, settings, prune, restart, pairing lifecycle) |
| `internal/djisrt` | Parses a DJI `.srt` flight-telemetry sidecar down to one representative GPS point |
| `internal/email` | Outbound email `Notifier` (`smtp` or `log` provider) for password-reset links |
| `internal/immich` | Minimal Immich HTTP client for the external-library scan trigger; no DB access |
| `internal/metadata` | Pure planning of which EXIF/XMP identity tags a child inherits from its parent |
| `internal/naming` | Filename-stem normalization and the ingest naming template, shared by pipeline and graph |
| `internal/pairing` | Per-device API key mint/rotate/revoke for agent auth; QR credential sealing |
| `internal/projectfile` | Tier-1 project-file parsers (`.dam.json`, `.drp`, `.fcpxml`, `.edl`, `.prproj`) |
| `internal/qr` | SVG QR rendering for companion pairing |
| `internal/secrets` | `BRANCHDAM_SECRET_KEY`-based encryption of secret settings values (`secrets.Box`) |
| `internal/settings` | `app_settings` UI overrides on top of `config.yaml`; field registry and live/restart apply modes |
| `internal/sse` | Server-Sent Events hub ("something changed, re-fetch") |
| `internal/thumbs` | JPEG thumbnail cache and background generation worker (outside `storage.Guard`, on the `/data` volume) |
| `internal/users` | Multi-user attribution: lazy `users` provisioning keyed on `(auth_provider, external_uid)` and the system user |
| `web/` | React 19 + Vite SPA (`@xyflow/react` graph, TanStack Query, Tailwind) |

## Key Invariants

1. **No Triggers, No CASCADE**: Every FK is `RESTRICT`. `PRAGMA foreign_keys = ON` is set on every connection in `ConnectHook`. Missing files set `lifecycle_state = 'MISSING'`; rows are never deleted.
2. **Single-Connection Writer Pool**: `db.DB` writer has `SetMaxOpenConns(1)` to eliminate race conditions during cycle checks and edge insertions.
3. **Filesystem Write Guarding**: All storage writes route through `storage.Guard`. Tier 3 is writable by default for server-governed ingest; `readOnly: true` in config and the `:ro` mount are opt-in for archive-only deployments.
4. **Header Isolation**: `internal/auth.BrowserChain` is the ONLY code permitted to read `X-Authentik-*` headers (`TestNoDirectAuthentikHeaderReads`). Agent routes unconditionally strip them.
5. **Agent Paths Untrusted**: `storage_location_id` on agent DTOs is ignored and re-derived from `storage.Guard.Resolve(filePath)`.
6. **Audit Priority**: Human `CONFIRMED`/`REJECTED` edge review states permanently outrank automated resolvers.
7. **Pruning TOCTOU Protections**: Tier-1 pruning requires a verified, live Tier-3 master with a 64-character `full_hash`, reached through `CONFIRMED` or `AUTO_ACCEPTED` lineage edges only (a `NEEDS_REVIEW` edge is an unreviewed guess and never authorises a purge). `prune.Execute` re-verifies disk presence (`os.Lstat`) and mtime/size for both candidates and Tier-3 ancestors immediately before calling `Guard.Remove`.
8. **Immich Supervisor**: Immich sync worker joins on graceful shutdown and refuses to start new workers once shutdown begins.
9. **Process Re-exec on Restart**: `POST /api/v1/restart` re-execs the binary (`syscall.Exec`) after graceful HTTP drain, gated to admin groups.

## Review thread resolution

Hermes reviews automatically on open (`.github/workflows/hermes.yml`'s
`auto-review` job) — don't also `@s3ntin3l8-hermes Review` right after
opening the PR, or you'll trigger a redundant second review. A re-review
can be requested the same way after pushing fixes, but keep it to a
couple of rounds — don't loop on it indefinitely.

Every review thread (Hermes or human) must be replied to and resolved before
a PR is mergeable. This is a GraphQL-only concept, not a `gh pr` verb:

```sh
# 1. Reply to inline comment (REST)
gh api repos/s3ntin3l8/branchdam/pulls/<PR>/comments/<comment_id>/replies -f body="Fixed in <sha>"
# 2. Resolve thread (GraphQL)
gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "<thread_id>"}) { thread { isResolved } } }'
```
