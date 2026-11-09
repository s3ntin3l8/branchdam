.DEFAULT_GOAL := help
.PHONY: help install-hooks web-stub web-ensure contract-fixtures dev dev-api dev-web dev-all dev-config \
	demo screenshots screenshots-landing screenshots-audit visual-check visual-baseline test-demo \
	test test-coverage lint fmt vet tidy vulncheck build build-embed golangci-lint \
	web-lint web-typecheck web-build web-test check check-web sqlc-generate sqlc-diff clean

CONFIG ?= config.yaml
GOLANGCI_LINT_VERSION ?= v2.14.0

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

install-hooks: ## Install pre-commit hooks
	pip install pre-commit
	pre-commit install
	pre-commit install --hook-type pre-push

web-stub: ## Stub web/dist so `go build`/`go test` work before the SPA (PR 10) exists locally
	@bash .github/ci-prebuild.sh

# web-ensure is web-stub + auto-rebuild on stale web/dist. Local-only
# convenience for `make dev*` and `make build-embed`: the backend CI lane
# never reaches here (it calls web-stub directly), so we don't drag Node
# into the Go test/benchmark gate. Skips silently if Node isn't on PATH
# -- the stub is still produced, just not refreshed.
#
# Staleness verdict comes from .github/ci-prebuild.sh (which writes
# web/dist/.branchdam-stale when a real dist is older than web/src).
# The gate also catches the common edit case via `find -newer`, which
# compares per-file mtime -- `[ web/src -nt web/dist/index.html ]` only
# sees directory-level changes (added/removed entries) and silently misses
# the most common edit. ci-prebuild.sh's verdict + `find -newer` together
# cover both "any source newer than dist" and the dirty-bit sentinel.
web-ensure: ## Stub-or-rebuild web/dist so the embedded SPA matches web/src (local dev only)
	@bash .github/ci-prebuild.sh
	@if [ -d web/src ] && [ -f web/package.json ] && command -v npm >/dev/null 2>&1; then \
	  if [ ! -f web/dist/index.html ] || [ ! -d web/dist/assets ] \
	     || [ -f web/dist/.branchdam-stale ] \
	     || find web/src -type f -newer web/dist/index.html -print -quit 2>/dev/null | grep -q . \
	     || [ web/package.json -nt web/dist/index.html ] \
	     || [ web/package-lock.json -nt web/dist/index.html ] \
	     || [ web/vite.config.ts -nt web/dist/index.html ]; then \
	    echo "web-ensure: web/dist missing or stale; running npm run build"; \
	    (cd web && npm ci --silent && npm run build --silent); \
	    rm -f web/dist/.branchdam-stale; \
	  fi \
	else \
	  echo "web-ensure: Node/npm not available; leaving whatever web/dist contains (likely a stub)."; \
	fi

dev-config: ## Render config.dev.yaml -> $(CONFIG) if missing, and create data/storage/*
	@if [ -f $(CONFIG) ]; then \
		echo "$(CONFIG) already exists, leaving it alone"; \
	else \
		esc_root=$$(printf '%s\n' "$(CURDIR)" | sed -e 's/[&|\\]/\\&/g'); \
		sed "s|__REPO_ROOT__|$$esc_root|g" config.dev.yaml > $(CONFIG); \
		echo "wrote $(CONFIG) from config.dev.yaml"; \
	fi
	@mkdir -p data/storage/archive data/storage/staging data/storage/exports data/storage/projects data/storage/scratch

dev: web-ensure ## Run the server with live config, building web/dist first if missing
	go run ./cmd/branchdam -config config.yaml

dev-api: web-ensure dev-config ## Run the Go API only (:8080), bootstrapping config.yaml if missing
	go run ./cmd/branchdam -config $(CONFIG) -debug

web/node_modules: web/package.json web/package-lock.json ## (file target) install frontend deps
	cd web && npm ci

dev-web: web/node_modules ## Run the Vite dev server only (:5173, proxies /api -> :8080)
	cd web && npm run dev

dev-all: web-ensure dev-config web/node_modules ## Run API + Vite together in one terminal (Ctrl-C stops both)
	@trap 'kill 0' EXIT INT TERM; \
	go run ./cmd/branchdam -config $(CONFIG) -debug & \
	(cd web && npm run dev) & \
	wait

demo: web/node_modules ## Throwaway demo with synthetic media + lineage (scratch config/DB/storage, clean env, 127.0.0.1:3975); Ctrl-C deletes it
	node scripts/demo/instance.mjs

# Visual pipeline (s3ntin3l8/visual-kit): README and landing screenshots, audit evidence and the per-change
# visual check, all rendered in one pinned image against `make demo`. The image is pinned in visual/Dockerfile
# (Dependabot bumps it); visual/.kit/run.sh is extracted from it. Spec: visual-kit's CONTRACT.md.
VISUAL_IMAGE := visual-kit-local/branchdam
VISUAL_RUN := VISUAL_KIT_IMAGE=$(VISUAL_IMAGE) visual/.kit/run.sh

visual/.kit/run.sh: visual/Dockerfile
	docker build -q -t $(VISUAL_IMAGE) - < visual/Dockerfile
	mkdir -p visual/.kit && docker run --rm $(VISUAL_IMAGE) host-files | tar -x -C visual/.kit

screenshots: visual/.kit/run.sh ## README screenshots into docs/screenshots (committed). FORCE=1 replaces a folder visual-kit did not write
	$(VISUAL_RUN) readme $(if $(FORCE),--force)
	@# The README shows stills only; the loop is for the landing page (screenshots-landing). Loops re-encode
	@# differently on every run, so keeping them here would make the weekly refresh PR itself noise.
	rm -f docs/screenshots/*.webm docs/screenshots/*.mp4 docs/screenshots/*.jpg

screenshots-landing: visual/.kit/run.sh ## Landing-page stills and loops into OUT (default ./landing-shots) for personal-landing
	$(VISUAL_RUN) landing --out "$(or $(OUT),landing-shots)" $(if $(FORCE),--force)

screenshots-audit: visual/.kit/run.sh ## Audit evidence for AUDIT=YYYY-MM-<scope>-<topic> into docs/audit/assets/ (local only)
	@test -n "$(AUDIT)" || { echo "AUDIT=YYYY-MM-<scope>-<topic> is required"; exit 1; }
	$(VISUAL_RUN) audit --slug "$(AUDIT)" $(if $(FORCE),--force)

# Only a whole number from 1 to 99 is forwarded. REPEAT_OK is a pinned mirror of the range visual-kit's launcher
# accepts (host/run.sh, 0.4.0): if the launcher's range moves, move this list with it. An ambient REPEAT=yes, 0 or
# 100 in the environment is ignored with a warning, as the launcher does.
REPEAT_OK := 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32 33 34 35 36 37 38 39 40 41 42 43 44 45 46 47 48 49 50 51 52 53 54 55 56 57 58 59 60 61 62 63 64 65 66 67 68 69 70 71 72 73 74 75 76 77 78 79 80 81 82 83 84 85 86 87 88 89 90 91 92 93 94 95 96 97 98 99
REPEAT_N := $(and $(REPEAT),$(if $(word 2,$(strip $(REPEAT))),,$(filter $(REPEAT_OK),$(strip $(REPEAT)))))
# An invalid REPEAT given on the command line is an error, before the image build: a typo must not look like a determinism check that ran once.
ifneq ($(filter visual-check,$(MAKECMDGOALS)),)
ifneq ($(strip $(REPEAT)),)
ifeq ($(REPEAT_N),)
ifeq ($(origin REPEAT),command line)
$(error REPEAT must be a whole number from 1 to 99, got "$(REPEAT)")
else
$(warning REPEAT="$(REPEAT)" from the environment is ignored: not a whole number from 1 to 99, so no determinism check runs)
endif
endif
endif
endif

visual-check: visual/.kit/run.sh ## Before/after of the routes a UI change touches (ROUTES=assets,/jobs) against BASE (default: merge base); REPEAT=N also recaptures the head N-1 times and fails if it is not deterministic
	$(VISUAL_RUN) check $(if $(ROUTES),--routes "$(ROUTES)") $(if $(BASE),--base "$(BASE)") $(if $(FORCE),--force) $(if $(REPEAT_N),--repeat "$(REPEAT_N)")

visual-baseline: visual/.kit/run.sh ## Regenerate the committed baselines (visual/baselines): the scenes flagged "baseline": true
	$(VISUAL_RUN) baseline $(if $(FORCE),--force)

test-demo: ## Tests for the demo's isolation guard
	node --test scripts/demo/*.test.mjs

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

# GOTOOLCHAIN: minimum Go is the module's ("+auto" can still switch up).
# @latest otherwise builds govulncheck with its own go.mod toolchain, which
# cannot type-check a newer module.
vulncheck: ## Check for known vulnerabilities
	GOTOOLCHAIN=$$(go env GOVERSION)+auto go install golang.org/x/vuln/cmd/govulncheck@latest
	$$(go env GOPATH)/bin/govulncheck ./...

build: web-stub ## Build all packages (embeds whatever web/dist contains -- see `build-embed`)
	go build ./...

build-embed: web-ensure ## Build all packages with a fresh web/dist embedded (local full-build convenience)
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

check: lint test test-demo build golangci-lint ## Backend pre-PR gate (NOTE: lint/gofmt hooks rewrite files in the working tree)
check-web: web-lint web-typecheck web-test web-build ## Frontend pre-PR gate

contract-fixtures: web-stub ## Regenerate docs/contract/fixtures (agent API wire shapes) from the server DTOs
	go test ./internal/httpapi/ -run 'TestContractFixtures$$' -count=1 -update-contract

sqlc-generate: ## Regenerate internal/db/sqlcgen from internal/db/queries
	sqlc generate

sqlc-diff: ## Fail if sqlcgen is out of date (pre-commit hook; no-op without the sqlc binary)
	@command -v sqlc >/dev/null 2>&1 && sqlc diff || echo "sqlc not installed, skipping sqlc-diff"

clean: ## Remove build artifacts and caches
	rm -f coverage.txt junit.xml
	rm -rf web/dist
	go clean -testcache
