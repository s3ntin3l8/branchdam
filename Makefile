.DEFAULT_GOAL := help
.PHONY: help install-hooks web-stub dev test test-coverage lint fmt vet tidy vulncheck build sqlc-generate sqlc-diff schema-check clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

install-hooks: ## Install pre-commit hooks
	pip install pre-commit
	pre-commit install
	pre-commit install --hook-type pre-push

web-stub: ## Stub web/dist so `go build`/`go test` work before the SPA (PR 10) exists locally
	@bash .github/ci-prebuild.sh

dev: web-stub ## Run the server with live config, building web/dist first if missing
	go run ./cmd/branchdam -config config.yaml

test: web-stub ## Run tests with race detector
	go test -race -coverprofile=coverage.txt -covermode=atomic -coverpkg=./... ./...

test-coverage: test ## Alias for test (coverage.txt is always produced)

lint: ## Run pre-commit on all files
	pre-commit run --all-files

fmt: ## Format Go code
	gofmt -w .
	goimports -w . 2>/dev/null || true

vet: web-stub ## Run go vet
	go vet ./...

tidy: ## Tidy Go modules
	go mod tidy

vulncheck: ## Check for known vulnerabilities
	go install golang.org/x/vuln/cmd/govulncheck@latest
	$$(go env GOPATH)/bin/govulncheck ./...

build: web-stub ## Build all packages
	go build ./...

sqlc-generate: ## Regenerate internal/db/sqlcgen from internal/db/queries
	sqlc generate

sqlc-diff: ## Fail if sqlcgen is out of date (pre-commit hook; no-op without the sqlc binary)
	@command -v sqlc >/dev/null 2>&1 && sqlc diff || echo "sqlc not installed, skipping sqlc-diff"

schema-check: ## Diff internal/db/schema.sql (sqlc's input) against a freshly-migrated DB dump — fallback if sqlc can't parse goose files directly (see docs risk log)
	@bash scripts/schema-check.sh

clean: ## Remove build artifacts and caches
	rm -f coverage.txt junit.xml
	rm -rf web/dist
	go clean -testcache
