.DEFAULT_GOAL := help
.PHONY: help install-hooks web-stub dev dev-api dev-web dev-all dev-config test test-coverage \
	lint fmt vet tidy vulncheck build golangci-lint web-lint web-typecheck web-build web-test \
	check check-web sqlc-generate sqlc-diff clean

CONFIG ?= config.yaml
GOLANGCI_LINT_VERSION ?= v2.12.2

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

install-hooks: ## Install pre-commit hooks
	pip install pre-commit
	pre-commit install
	pre-commit install --hook-type pre-push

web-stub: ## Stub web/dist so `go build`/`go test` work before the SPA (PR 10) exists locally
	@bash .github/ci-prebuild.sh

dev-config: ## Render config.dev.yaml -> $(CONFIG) if missing, and create data/storage/*
	@if [ -f $(CONFIG) ]; then \
		echo "$(CONFIG) already exists, leaving it alone"; \
	else \
		sed 's|__REPO_ROOT__|$(CURDIR)|g' config.dev.yaml > $(CONFIG); \
		echo "wrote $(CONFIG) from config.dev.yaml"; \
	fi
	@mkdir -p data/storage/archive data/storage/exports data/storage/projects data/storage/scratch

dev: web-stub ## Run the server with live config, building web/dist first if missing
	go run ./cmd/branchdam -config config.yaml

dev-api: web-stub dev-config ## Run the Go API only (:8080), bootstrapping config.yaml if missing
	go run ./cmd/branchdam -config $(CONFIG) -debug

web/node_modules: web/package.json web/package-lock.json ## (file target) install frontend deps
	cd web && npm ci

dev-web: web/node_modules ## Run the Vite dev server only (:5173, proxies /api -> :8080)
	cd web && npm run dev

dev-all: web-stub dev-config web/node_modules ## Run API + Vite together in one terminal (Ctrl-C stops both)
	@trap 'kill 0' EXIT INT TERM; \
	go run ./cmd/branchdam -config $(CONFIG) -debug & \
	(cd web && npm run dev) & \
	wait

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

golangci-lint: web-stub ## Run the pinned golangci-lint (required CI check; make lint does NOT cover this)
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

web-lint: ## Run eslint on web/
	cd web && npm run lint

web-typecheck: ## Run tsc --noEmit on web/
	cd web && npm run typecheck

web-build: web/node_modules ## Typecheck + production build web/ -> web/dist/
	cd web && npm run build

web-test: web/node_modules ## Run vitest on web/
	cd web && npm run test

check: lint test build golangci-lint ## Backend pre-PR gate (NOTE: lint/gofmt hooks rewrite files in the working tree)
check-web: web-lint web-typecheck web-build ## Frontend pre-PR gate

sqlc-generate: ## Regenerate internal/db/sqlcgen from internal/db/queries
	sqlc generate

sqlc-diff: ## Fail if sqlcgen is out of date (pre-commit hook; no-op without the sqlc binary)
	@command -v sqlc >/dev/null 2>&1 && sqlc diff || echo "sqlc not installed, skipping sqlc-diff"

clean: ## Remove build artifacts and caches
	rm -f coverage.txt junit.xml
	rm -rf web/dist
	go clean -testcache
