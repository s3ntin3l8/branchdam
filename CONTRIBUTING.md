# Contributing

## Setup

```bash
make install-hooks    # pre-commit + pre-push git hooks (see below -- do this before your first commit)
cd web && npm ci
```

The Go binary embeds `web/dist` via `//go:embed`. If `web/dist` doesn't exist, backend builds
and tests fail. `make build`/`make test`/`make dev` all depend on the `web-stub` target, which
runs `.github/ci-prebuild.sh` to create a placeholder if none exists yet. Run `npm run build` in
`web/` first if you want the real SPA embedded instead of the stub.

See [`CLAUDE.md`](CLAUDE.md) for the full architecture and package tour.

## Before opening a PR

`make install-hooks` wires up local git hooks: the `pre-commit` stage runs gofmt, `go vet`,
`go mod tidy`, and `sqlc-diff` on every commit; the `pre-push` stage additionally runs
`go test -race` and `govulncheck`. Run the same checks manually before opening a PR:

```bash
make check
```

which is exactly `make lint && make test && make build && make golangci-lint` --
`golangci-lint` is `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run`
(see the Makefile's `GOLANGCI_LINT_VERSION`). The `/v2` module path and the version pin both
matter -- `.golangci.yml` declares `version: "2"`, and a floating `@latest` install could
resolve v3+ and fail against a v2 config. CI pins the same version via
`golangci/golangci-lint-action@v9`; bump both together.

Be precise about what each of these does and doesn't cover:

- `make lint` is `pre-commit run --all-files` -- it only runs the `pre-commit`-staged hooks
  (gofmt, go vet, go mod tidy, sqlc-diff), **not** `go test` or `govulncheck` (those are
  `pre-push`-staged), and **not** `golangci-lint`. `make check` covers all of these; `make lint`
  alone does not. Note `make lint`'s pre-commit hooks (`trailing-whitespace`,
  `end-of-file-fixer`, `mixed-line-ending`, `gofmt`) rewrite files in your working tree.
- `sqlc-diff` is a no-op if the `sqlc` binary isn't installed on your machine. If you changed
  `internal/db/migrations/*.sql` or `internal/db/queries/*.sql`, run `sqlc generate` yourself
  and commit `internal/db/sqlcgen/` in the same PR -- CI has no codegen step, so a stale
  `sqlcgen/` breaks `go build ./...` on `main`, not just your branch.

If you changed a route DTO in `internal/httpapi/routes.go`, hand-update
`web/src/api/types.ts` and `web/src/api/client.ts` to match -- there's no generated client yet.

If you're writing a recursive CTE against `internal/db/queries/*.sql`, see the sqlc risk note in
[`docs/schema.md`](docs/schema.md): every column in the anchor `SELECT` must be explicitly named
or aliased (`SELECT sqlc.arg(x) AS id`, not `SELECT sqlc.arg(x)`), or sqlc's SQLite parser fails
with `*ast.ResTarget has nil name`.

Frontend changes:

```bash
make check-web
```

which is `cd web && npm run lint && npm run typecheck && npm run build`.

## PR title

Must use a [Conventional Commits](https://www.conventionalcommits.org/) prefix (`feat:`,
`fix:`, `chore:`, `docs:`, ...). This repo squash-merges PRs and release-please parses the
**PR title**, not the individual commits, to cut versions and changelog entries -- an
unprefixed title silently drops from the changelog.

## Branching

Branch off the latest `origin/main`, not a possibly-stale local `main`:

```bash
git fetch origin && git checkout -b <branch> origin/main
```

No direct commits to `main`. One issue = one PR, per [`docs/roadmap.md`](docs/roadmap.md).

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
be replied to and resolved before a PR is mergeable, even with `enforce_admins: false`. See
[`CLAUDE.md`](CLAUDE.md)'s "Addressing review feedback" guideline for the exact commands (thread
resolution is a GraphQL-only concept, not a `gh pr` verb).

## Automated review

Every non-draft PR gets an automated review from the `s3ntin3l8-hermes[bot]` GitHub App,
posted once on `opened` (or once on `ready_for_review` if the PR started as a draft). Ask for
another look at any point -- including after addressing feedback -- by commenting
`@s3ntin3l8-hermes Review` on the PR (or `@s3ntin3l8-hermes Triage` on an issue). See
[`CLAUDE.md`](CLAUDE.md)'s "Addressing review feedback" guideline for replying to and resolving
Hermes's inline comments.

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
