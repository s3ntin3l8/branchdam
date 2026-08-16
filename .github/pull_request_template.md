## Summary

<!-- Describe what changed, why, and any context or subproblems. -->

### Changes made:

<!-- Numbered list of changes per file/component. E.g.,
1. **internal/pipeline/commit.go**: Added move-detection rebase path.
-->

### Key design decisions:

<!-- Rationale for non-obvious choices. -->

## Test plan / Verification

- [ ] `make lint` (pre-commit: gofmt, go vet, go mod tidy, sqlc-diff)
- [ ] `make test` — `go test -race ./...`
- [ ] `make build`
- [ ] `golangci-lint run` (v2.x — `.golangci.yml` is `version: "2"`; CI pins v2.12.2)
- [ ] Frontend changes: `cd web && npm run lint && npm run typecheck && npm run build`
- [ ] Migrations/queries (`internal/db/migrations/*.sql`, `internal/db/queries/*.sql`):
      ran `sqlc generate` and committed `internal/db/sqlcgen/` in this PR
- [ ] Route DTO changes (`internal/httpapi/routes.go`): hand-updated `web/src/api/types.ts`
      and `web/src/api/client.ts`
- [ ] Filesystem writes go through `storage.Guard` — no new `os.Create`/`WriteFile`/`MkdirAll`/
      `Remove` on a storage-location path

<!-- Note: `make lint` does not run tests, govulncheck, or golangci-lint -- see CONTRIBUTING.md. -->

Closes #<!-- Issue Number -->

<!--
PR title must use a Conventional Commits prefix (feat:, fix:, chore:, docs:, ...).
This repo squash-merges PRs and Release Please parses the PR title, not the
individual commits -- an unprefixed title silently drops from the changelog.

See CONTRIBUTING.md for the full pre-PR checklist and setup steps.
-->
