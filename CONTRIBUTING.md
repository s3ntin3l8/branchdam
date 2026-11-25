# Contributing

## Setup

```bash
make install-hooks    # pre-commit + pre-push git hooks (see below -- do this before your first commit)
cd web && npm ci
```

The Go binary embeds `web/dist` via `//go:embed`. `make build`/`test`/`vet`/`golangci-lint` create a placeholder
when it's missing (`web-stub`), `make dev*` rebuild it when `web/src` is newer (`web-ensure`), and `make build-embed`
builds a binary with the real SPA embedded.

Agent and process rules (branching, worktrees, PR titles, review threads) live in [`AGENTS.md`](AGENTS.md)'s
Workflow Conventions; architecture and invariants are there too.

## Before opening a PR

```bash
make check       # lint + test + test-demo + build + golangci-lint
make check-web   # frontend: cd web && lint + typecheck + test + build
```

- `make install-hooks` wires up git hooks: `pre-commit` runs gofmt, `go vet`, `go mod tidy` and `sqlc-diff`;
  `pre-push` adds `go test -race` and `govulncheck`. `make lint` (= `pre-commit run --all-files`) rewrites files in
  your working tree and does not run tests, `govulncheck` or golangci-lint. `govulncheck` is only in the pre-push hook
  and `make vulncheck`.
- golangci-lint is pinned by the Makefile's `GOLANGCI_LINT_VERSION` (v2 module path, matching `.golangci.yml`'s
  `version: "2"`) and by `golangci/golangci-lint-action` in CI. Bump both together.
- `sqlc-diff` is a no-op without the `sqlc` binary. After changing a migration or query, run `sqlc generate` and
  commit `internal/db/sqlcgen/` in the same PR: CI has no codegen step. See [`docs/schema.md`](docs/schema.md) for
  the sqlc traps (ASCII-only query files, aliased recursive-CTE anchors).
- A browser DTO change in `internal/httpapi/*.go` needs a hand update of `web/src/api/types.ts` and
  `web/src/api/client.ts`; there's no generated client.

## Demo instance and screenshots

`make demo` starts a throwaway branchDAM with synthetic media and a populated version graph on `127.0.0.1:3975`
(its own config, database and storage roots under `/tmp/branchdam-demo`, a clean environment, Ctrl-C deletes it).
It needs Go, `ffmpeg` and `exiftool`. With `DEMO_NOW=2026-10-15T10:00:00Z` set, the server's clock
(`internal/clock`) is frozen an hour before it (config key `clock.fixed`, accepted only with `BRANCHDAM_DEMO=1`, which
the demo sets), so every timestamp and node UUID reads the same on any day. A frozen clock does not advance: the
scan jobs on the jobs page all show one instant and a 0 s duration. Server code reads time through `internal/clock`
(`clock.Now` for a stored or shown time, `clock.Real` for deadlines and measurements); lint rejects `time.Now`.

The screenshots and the per-change visual check run in a pinned
[visual-kit](https://github.com/s3ntin3l8/visual-kit) image (Docker) against that demo; the pages are the scenes in
`visual/scenes.json`.

| Command | What it does |
| --- | --- |
| `make screenshots` | the README images into `docs/screenshots/` (dark and light, desktop and mobile; commit them) |
| `make screenshots-landing OUT=<dir>` | the landing-page stills and the asset-graph loop for personal-landing's `npm run assets:sync branchdam` |
| `make screenshots-audit AUDIT=YYYY-MM-<scope>-<topic>` | desktop, tablet and phone stills of every page into `docs/audit/assets/` (local only) |
| `make visual-check [ROUTES=a,b] [BASE=ref]` | renders the merge base and your working tree and reports what changed (`visual/.out/check/report/`); `REPEAT=2` also proves the capture repeats |
| `make visual-baseline` | regenerates `visual/baselines/`, the committed check shots of the scenes flagged `"baseline": true` |

- **On a pull request** the visual check runs for UI changes (advisory) and posts its report as a comment, with a
  separate `visual / health` check for new console errors and serious axe findings.
- **`/update-baselines`**: comment it on a pull request (write access, branch in this repository) and CI regenerates
  `visual/baselines/` and pushes one commit of the images that really changed to that branch.
- **Weekly refresh** (`visual-refresh.yml`, Mondays, or run it from the Actions tab): regenerates the README images
  and the baselines and, only when one changed by more than the pixel tolerance, opens or updates one pull request
  each (`bot/refresh-screenshots`, `bot/refresh-baselines`). Review the images, then merge or close it.
- **`/refresh-screenshots`**: comment it on a pull request (write access) to run that same refresh on demand, so a
  stale refresh pull request can be regenerated without waiting for Monday. The refresh pull requests carry this
  reminder in their description.
- **`baseHarness`** renders the base with the head's `scripts/demo` and the merge base's server and web app, so a PR
  that fixes the demo isn't compared against the bug it fixes. A PR that changes `scripts/demo` together with code it
  depends on can fail to start its base demo; then every scene reports "new" (the reason is only a stderr line).
- The `storage-health` page shows the host's real disk usage, so it is in `check` and `audit` (masked) but not in the
  README or landing set. The landing loop re-encodes differently on every run, so `make screenshots` doesn't keep it
  in `docs/screenshots`.
- The asset paths in the captures read `/tmp/branchdam-demo/...`; if your shell sets `TMPDIR` to something else, run
  `TMPDIR=/tmp make screenshots` so your images match what CI renders.

## PR titles and branching

Branch off the latest `origin/main` (`git fetch origin && git checkout -b <branch> origin/main`); no direct commits to
`main`, one issue = one PR. PR titles need a [Conventional Commits](https://www.conventionalcommits.org/) prefix:
this repo squash-merges and release-please reads the **PR title** to cut versions and changelog entries, so an
unprefixed title silently drops out of the changelog.

## Branch protection

`main` requires these status checks: `Go (build · vet · test) / lint-and-test`,
`Web (typecheck · build) / lint-and-test`, `CodeQL`, `review / dependency-review`, and
`golangci-lint`. `strict` is `false` (branches need not be up to date with `main` to merge) and
`enforce_admins` is `false`. There are no required reviews (solo repo).

One expected wrinkle: release-please's own release PR never gets a CI check reported --
`release-please-action` opens/updates that PR using the default `GITHUB_TOKEN`, and GitHub's
Actions recursion guard means refs/PRs created by `GITHUB_TOKEN` don't trigger
`on: push`/`on: pull_request` workflows. With required status checks in place, that PR's merge
button reads as blocked/"Expected" forever, not just slow -- that's expected, not a bug. Merge
it via the "merge without waiting for requirements" path, which `enforce_admins: false` makes
available to the repo owner.

`required_conversation_resolution` is also on: every review thread (Hermes's or a human's) must
be replied to and resolved before a PR is mergeable, even with `enforce_admins: false`. Thread resolution is
GraphQL-only (`resolveReviewThread`), not a `gh pr` verb; [`AGENTS.md`](AGENTS.md)'s Workflow Conventions have the
exact commands.

## Automated review

Every non-draft PR gets an automated review from the `s3ntin3l8-hermes[bot]` GitHub App,
posted once on `opened` (or once on `ready_for_review` if the PR started as a draft). Ask for
another look at any point -- including after addressing feedback -- by commenting
`@s3ntin3l8-hermes Review` on the PR (or `@s3ntin3l8-hermes Triage` on an issue). Agents request reviews
through the Mullion review relay instead (see [`AGENTS.md`](AGENTS.md)).

Because `required_conversation_resolution` is on, any inline comment Hermes (or a human
reviewer) attaches to a review thread blocks merge until that thread is replied to and resolved.
This does *not* gate on the review's overall verdict (`APPROVED`/`CHANGES_REQUESTED`) -- a
`CHANGES_REQUESTED` review whose findings live only in the summary body, with no inline
comments, does not block. That's the deliberate trade-off: it keeps a Hermes outage from ever
wedging a merge, since a human can resolve threads without the bot.

Auto-review only runs for PRs from this repo, not forks -- `hermes.yml`'s `auto-review` job is
gated on `head.repo.full_name == github.repository`, since a `pull_request` event otherwise runs
a fork's own copy of the workflow file on the self-hosted runner (this repo is public). A
maintainer can still get a review on a fork PR by commenting `@s3ntin3l8-hermes Review` --
`issue_comment` always runs the default branch's copy of the workflow, in the base repo's
context, gated by commenter trust rather than fork origin.

## Templates

All issues and pull request descriptions must adhere to the standard templates:

- Issue blueprint: [.github/ISSUE_TEMPLATE/issue-blueprint.md](.github/ISSUE_TEMPLATE/issue-blueprint.md)
- PR template: [.github/pull_request_template.md](.github/pull_request_template.md)

The templates enforce checking all guidelines (lint, test, build, golangci-lint, codegen
contracts) before submission. Fill them in rather than skipping them.
