# AGENTS.md — branchDAM

This file is the single source of truth for this repo's workflow rules and
load-bearing invariants — the ones every agent needs before touching
anything, regardless of which CLI you are. `CLAUDE.md` is a one-line
`@AGENTS.md` import, so every CLI (Claude Code, Codex, opencode, agy) reads
this file, natively or via that import. See [`CONTRIBUTING.md`](CONTRIBUTING.md)
for setup, the demo/screenshot tooling and CI details, and [`docs/`](docs) for deeper architecture detail.

Self-hosted Digital Asset Management server. Models media assets as a version
node graph with confidence-weighted lineage edges. Go backend (Huma v2, SQLite
WAL, sqlc, goose) + React 19 SPA (Vite, @xyflow/react, TanStack Query, Tailwind).

<!-- mullion:briefing:start -->
This repo's non-obvious correctness invariants are in `.claude/skills/branchdam/SKILL.md`. They cover single-writer transactions, `storage.Guard`, `internal/clock` determinism, edge review precedence, prune hash provenance, agent input trust, sqlc traps and hand-synced wire contracts. Read it before changing anything under `internal/`, a migration or query, or a route DTO. For review, delegate to the `branchdam-reviewer` subagent (`.claude/agents/branchdam-reviewer.md`). It reads that skill first and checks your uncommitted diff or branch against it, section by section. Use it for the self-review pass before declaring a change done.

## Workflow Conventions

- Never commit directly to the default branch. Always branch and open a PR.
- Branch off the latest remote default branch, never off your local one: `git fetch origin && git checkout -b <branch> origin/<default>`. A local default branch is routinely stale, which is what makes a PR show up as "out-of-date with the base branch" the moment it's opened.
- Work in a dedicated worktree per branch, under `.wt/<name>` (the branch name with `/` replaced by `-`, so the directory stays flat), off the latest remote default branch: `git fetch origin && git worktree add .wt/<name> -b <branch> origin/<default>`. A fresh worktree does not inherit installed dependencies — reinstall them there before testing or building.
- Commit and PR titles need a Conventional Commits prefix (`feat:`, `fix:`, `chore:`, ...).
- Squash-merge PRs — the PR title becomes the commit message on the default branch.
- Require green CI before merging.
- Run a self-review pass on your own diff before declaring done.

  Request an automated/bot review through the review relay (`mullion review`, or the `request_review` tool) — once after opening the pull request, and again per head commit after pushing fixes for a changes-requested review, then wait for it. A review comment that mentions the review bot is for humans, not agents; if no relay is set up, tell the human instead. One review per pull request at a time: a review that is already running is not re-requested — the request is refused with its run id, and that refusal is the answer, not an error to retry: wait for it, or stop it with stop_review. A review takes 10-30 minutes; check review_status once instead of asking again. After pushing fixes for a changes-requested review, ask again only when no review is in flight. Once the review bot has approved, apply its remaining suggestions and push without requesting another review, even though the head moved: small targeted fixes do not change the verdict, and re-reviewing them only breeds more nits. Never re-trigger a review with an @mention of the review bot: that path is for humans, and it queues another round behind the running one.
- Fixing the code is not enough to address review feedback — reply to each inline comment via the GitHub API, then resolve the thread via the GraphQL `resolveReviewThread` mutation, rather than pushing a silent fix. Reply with `gh api repos/{owner}/{repo}/pulls/<pr>/comments/<comment_id>/replies -f body="…"`; resolve with `gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "<thread_id>"}) { thread { isResolved } } }'`, taking the thread id from a `reviewThreads` query on the pull request, not from the REST comment id.
- File a tracked issue for anything a plan defers, blocks, or descopes, and link it from the PR (using the repository's issue template if it has one) — a footnote in a plan doc is not a durable record. Write each one as a spec an agent can implement on its own (scope, acceptance criteria), not as a note. Title it the way this repo titles commits and PRs, reusing its existing scope names rather than coining new ones; attach it as a sub-issue to the matching open epic when one exists; and set GitHub's `blocked_by` dependency where one issue cannot start before another closes.
- After a merge, delete the local and remote branch and remove any associated worktree.
- Before pushing, run the full lint/typecheck/test/format gate.
<!-- mullion:briefing:end -->

## Repo specifics

- **Fresh worktree setup:** `make install-hooks`, then `cd web && npm ci`. The Go binary embeds `web/dist`; `make build`/`test`/`vet`/`golangci-lint` stub it when missing, `make build-embed` builds the real SPA in.
- **Pre-push gate:** `make check` for backend changes, `make check-web` for anything under `web/`. `make lint` rewrites files (pre-commit fixers), so re-check `git status` afterwards. `govulncheck` runs only from the pre-push hook or `make vulncheck`.
- **CI has no codegen step.** A migration or query change without a committed `internal/db/sqlcgen/` breaks `go build ./...` on `main`. Query files must stay ASCII-only and recursive-CTE anchors must alias every column (skill §2, `docs/schema.md`).
- **Hand-synced contracts:** browser DTOs (any `json:` struct in `internal/httpapi/*.go` outside `/agent`) → `web/src/api/types.ts` + `client.ts`; agent API DTOs → `make contract-fixtures`; new agent error codes → `docs/agent-api.md`'s table. branchdam-agent and branchdam-mobile copy `docs/contract/fixtures` in with `make contract-sync` and test against it, so an agent DTO, error-code or hashing change also needs a follow-up issue (or PR) in each of those repos.
- **PR titles feed release-please** (squash-merge title → changelog). Existing scopes include `upload`, `uiux` and `ci`. PRs use `.github/pull_request_template.md`, issues `.github/ISSUE_TEMPLATE/issue-blueprint.md`.
- **Review bot:** Hermes (`s3ntin3l8-hermes[bot]`) auto-reviews every non-draft same-repo PR on open, except `mullion/task-*` branches (Task Master runs its own review there, `hermes.yml`); request reviews as the Workflow Conventions describe. `main` has `required_conversation_resolution`: every inline thread blocks merge until replied to and resolved.
- **release-please PRs** never get CI checks reported; the owner merges them past the required checks (CONTRIBUTING.md "Branch protection").

## Commands

```sh
make check       # lint + test + test-demo + build + golangci-lint (not govulncheck)
make check-web   # cd web && lint + typecheck + test + build
make dev-api     # Go API only (:8080)
make dev-web     # Vite dev server (:5173, proxies /api -> :8080)
make dev-all     # both
sqlc generate           # after editing migrations or queries; commit internal/db/sqlcgen/
make contract-fixtures  # after changing an agent API DTO; commit docs/contract/fixtures
make demo                        # throwaway instance on :3975; DEMO_NOW=<ISO date-time> freezes its clock
make visual-check ROUTES=assets  # before/after of the pages a UI change touches (REPEAT=2: determinism)
make visual-baseline             # regenerate visual/baselines (or comment /update-baselines on the PR)
make screenshots                 # docs/screenshots (README); see CONTRIBUTING.md for the landing/audit sets
```

## UI changes

**Look at every UI change before calling it done.** Run `make visual-check ROUTES=<pages you touched>` (Docker;
routes are scene names or paths from `visual/scenes.json`). It renders the merge base and your working tree against
`make demo` in the pinned visual-kit image and writes `visual/.out/check/report/` (`index.html`, `summary.md`).
Read the changed and new shots and the deltas (axe, console errors, overflow, unnamed controls) and classify each
diff as intended, regression or flake. Fix flake in the demo or `visual/hooks.mjs`, or give that one scene a
`maxDiffPixels` with the reason; never raise the threshold globally. The PR description gets a **Visual check**
section (routes, what changed and why, axe delta). A new page or state needs a scene in `visual/scenes.json`.

- **Determinism:** after changing the demo or a scene, `make visual-check REPEAT=2` must report no determinism
  failure. It relies on `DEMO_NOW` + `internal/clock` (skill §4) and one hash worker ordering the scan.
- **Baselines:** scenes flagged `"baseline": true` have committed shots in `visual/baselines/`; regenerate them in
  the PR that moves those pages.
- **README/landing screenshots:** regenerate them when a screen they show changes (`TMPDIR=/tmp make screenshots`).
  CONTRIBUTING.md covers the weekly refresh, `baseHarness` and the `storage-health` exclusion.

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
| `internal/prune` | TTL cache pruning engine with strict TOCTOU disk checks; `.trash/` retention purge aged by the recorded `node_trash_times.trashed_at` |
| `internal/httpapi` | Huma v2 routes, middleware chain, SSE handler |
| `internal/agent` | Server side of the agent contract: `event_queue` draining, handshake support, path rebasing |
| `internal/apierr` | The agent API's single error envelope: problem+json plus a stable machine-readable `code` (table in `docs/agent-api.md`) |
| `internal/audit` | `actor_audit` log writes/reads for admin actions (scan, settings, prune, restart, pairing lifecycle) |
| `internal/djisrt` | Parses a DJI `.srt` flight-telemetry sidecar down to one representative GPS point |
| `internal/email` | Outbound email `Notifier` (`smtp` or `log` provider) for password-reset links |
| `internal/immich` | Minimal Immich HTTP client for the external-library scan trigger; no DB access |
| `internal/metadata` | Pure planning of which EXIF/XMP identity tags a child inherits from its parent |
| `internal/naming` | Filename-stem normalization and the ingest naming template, shared by pipeline and graph |
| `internal/pairing` | Per-device API key mint/rotate/revoke for agent auth; QR credential sealing; rotated-key delivery through the handshake and adoption on first use |
| `internal/projectfile` | Tier-1 project-file parsers (`.dam.json`, `.drp`, `.fcpxml`, `.edl`, `.prproj`) |
| `internal/qr` | SVG QR rendering for companion pairing |
| `internal/rehash` | Background worker that re-reads agent-asserted Tier-3 `full_hash` values through `Guard.OpenRead` and marks matches server-computed (`full_hash_provenance`) |
| `internal/secrets` | `BRANCHDAM_SECRET_KEY`-based encryption of secret settings values (`secrets.Box`) |
| `internal/settings` | `app_settings` UI overrides on top of `config.yaml`; field registry and live/restart apply modes |
| `internal/sse` | Server-Sent Events hub ("something changed, re-fetch") |
| `internal/thumbs` | JPEG thumbnail cache and background generation worker (outside `storage.Guard`, on the `/data` volume) |
| `internal/users` | Multi-user attribution: lazy `users` provisioning keyed on `(auth_provider, external_uid)` and the system user; PAT principals resolve to the token owner by id and never provision a row |
| `web/` | React 19 + Vite SPA (`@xyflow/react` graph, TanStack Query, Tailwind) |

## Key Invariants

1. **No Triggers, No CASCADE**: Every FK is `RESTRICT`. `PRAGMA foreign_keys = ON` is set on every connection in `ConnectHook`. Missing files set `lifecycle_state = 'MISSING'`; rows are never deleted.
2. **Single-Connection Writer Pool**: `db.DB` writer has `SetMaxOpenConns(1)` to eliminate race conditions during cycle checks and edge insertions.
3. **Filesystem Write Guarding**: All storage writes route through `storage.Guard`. Tier 3 is writable by default for server-governed ingest; `readOnly: true` in config and the `:ro` mount are opt-in for archive-only deployments.
4. **Header Isolation**: `internal/auth.BrowserChain` is the ONLY code permitted to read `X-Authentik-*` headers (`TestNoDirectAuthentikHeaderReads`). Agent routes unconditionally strip them.
5. **Agent Paths Untrusted**: `storage_location_id` on agent DTOs is ignored and re-derived from `storage.Guard.Resolve(filePath)`.
6. **Audit Priority**: Human `CONFIRMED`/`REJECTED` edge review states permanently outrank automated resolvers.
7. **Pruning TOCTOU Protections**: Tier-1 pruning (and agent `node-status` `verified`) needs a live Tier-3 master whose `full_hash` is server-verified (`full_hash_provenance` = `SERVER`; a device-asserted hash authorises nothing), reached only through `CONFIRMED`/`AUTO_ACCEPTED` `PROXY_OF`/`DERIVED_FROM`/`DUPLICATE_OF` edges over `ACTIVE`/`HIDDEN` nodes. Project files and `.xmp` sidecars are never candidates. `prune.Execute` re-checks disk state and re-reads the verified ancestor set on the writer right before `Guard.Remove`, and abandons the purge if it changed (skill §6).
8. **Immich Supervisor**: Immich sync worker joins on graceful shutdown and refuses to start new workers once shutdown begins.
9. **Process Re-exec on Restart**: `POST /api/v1/restart` re-execs the binary (`syscall.Exec`) after graceful HTTP drain, gated to admin groups.
