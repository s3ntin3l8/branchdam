#!/usr/bin/env bash
# ci-prebuild.sh — runs before `go build`/`go test` in ci-go.yml, and is also
# the single source of truth `make web-stub`/`make dev`/`make test`/`make
# build` call for local dev, so CI and local builds stub web/dist identically.
#
# The Go binary embeds web/dist via //go:embed (see web/embed.go). The real
# frontend bundle is produced by the frontend job / Docker build (PR 10); on
# the backend-only CI lane and on a fresh clone we just need a non-empty stub
# so //go:embed is happy. Idempotent — skips if a real (or stub) build exists.
set -euo pipefail

if [ -f web/dist/index.html ]; then
  exit 0
fi

mkdir -p web/dist
printf '<!doctype html><title>branchDAM</title>' > web/dist/index.html
