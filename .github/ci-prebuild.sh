#!/usr/bin/env bash
# ci-prebuild.sh — runs before `go build`/`go test` in ci-go.yml, and is also
# the single source of truth `make web-stub`/`make dev`/`make test`/`make
# build` call for local dev, so CI and local builds stub web/dist identically.
#
# The Go binary embeds web/dist via //go:embed (see web/embed.go). The real
# frontend bundle is produced by the frontend job / Docker build (PR 10); on
# the backend-only CI lane and on a fresh clone we just need a non-empty stub
# so //go:embed is happy. Idempotent — skips writing if a real (or stub) build
# exists.
#
# Stale-dist detection (local-`make dev` only, never the backend CI lane):
# if web/dist already exists but is older than any web/src file, print a
# warning pointing at the newest offending source and export
# BRANCHDAM_DIST_STALE=1 so the Makefile web-ensure target can rebuild
# without re-implementing the verdict. CI intentionally keeps the stub for
# speed; the warning is a hint, not a failure.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
INDEX="$REPO_ROOT/web/dist/index.html"
SRC_DIR="$REPO_ROOT/web/src"
WEB_ROOT="$REPO_ROOT/web"

# Stub marker: anything matching this prefix is the placeholder we wrote,
# not a real Vite build. Detecting it lets us tell "no SPA built yet"
# apart from "stale SPA sitting around from last time".
STUB_MARKER='<!doctype html><title>branchDAM</title>'

# mtime_of FILE prints integer epoch seconds for FILE on stdout, or 0 if
# FILE is missing or stat is unavailable. POSIX sh only has `stat` with
# wildly different flags per platform -- GNU: -c %Y, BSD/macOS: -f %m.
# Probe in order and accept whatever returns a non-empty integer.
mtime_of() {
  local f="$1"
  local t
  if t=$(stat -c %Y "$f" 2>/dev/null) && [ -n "$t" ]; then
    echo "$t"
  elif t=$(stat -f %m "$f" 2>/dev/null) && [ -n "$t" ]; then
    echo "$t"
  else
    echo 0
  fi
}

is_stub() {
  if [ ! -f "$INDEX" ]; then return 1; fi
  # The real Vite index.html is ~600B+; stub is ~42B.
  local size
  size=$(wc -c <"$INDEX" | tr -d ' ')
  if [ "$size" -lt 200 ]; then return 0; fi
  if head -c 100 "$INDEX" | grep -qF "$STUB_MARKER"; then return 0; fi
  return 1
}

warn_stale() {
  local newest_src="$1" dist_mtime="$2" src_mtime="$3"
  echo "warning: web/dist appears stale relative to web/src" >&2
  echo "         newest source: $newest_src" >&2
  echo "         newest source mtime: $src_mtime" >&2
  echo "         web/dist/index.html mtime: $dist_mtime" >&2
  echo "         run \`make web-build\` (or just \`make dev-api\`, which auto-builds) to refresh the embedded SPA." >&2
}

# Always make sure //go:embed has something to resolve. Missing dist -> stub.
if [ ! -f "$INDEX" ]; then
  mkdir -p "$(dirname "$INDEX")"
  printf '%s' "$STUB_MARKER" > "$INDEX"
  exit 0
fi

# CI backend lane: don't even stat sources, don't warn. Stub-or-not, we
# hand the path to `go build` and move on. Detected by the presence of
# the env var the backend reusable workflow sets.
if [ -n "${BRANCHDAM_CI_BACKEND:-}" ]; then
  exit 0
fi

# Local: detect staleness and warn. mtime comparison in seconds since epoch.
if [ -d "$SRC_DIR" ]; then
  # `-print0` keeps filenames with spaces/newlines intact under find -exec.
  newest_src_path=""
  newest_src_epoch=0
  while IFS= read -r -d '' f; do
    epoch=$(mtime_of "$f")
    if [ "$epoch" -gt "$newest_src_epoch" ]; then
      newest_src_epoch="$epoch"
      newest_src_path="$f"
    fi
  done < <(find "$SRC_DIR" -type f \( -name '*.ts' -o -name '*.tsx' -o -name '*.css' -o -name '*.html' \) -print0)
  # Also consider vite config / package manifest changes -- these affect output
  # even when src/ is unchanged (e.g. plugin or dep bump).
  for root in "$WEB_ROOT"; do
    for f in "$root/vite.config.ts" "$root/package.json" "$root/package-lock.json" "$root/index.html"; do
      [ -f "$f" ] || continue
      epoch=$(mtime_of "$f")
      if [ "$epoch" -gt "$newest_src_epoch" ]; then
        newest_src_epoch="$epoch"
        newest_src_path="$f"
      fi
    done
  done

  dist_epoch=$(mtime_of "$INDEX")
  if [ "$newest_src_epoch" -gt "$dist_epoch" ] && [ "$newest_src_epoch" -ne 0 ]; then
    src_iso=$(date -u -d "@$newest_src_epoch" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "$newest_src_epoch")
    dist_iso=$(date -u -d "@$dist_epoch" +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "$dist_epoch")
    # Only show the staleness warning if the dist looks like a real SPA --
    # a stale stub vs. fresh sources is fine, the user just hasn't built yet.
    if ! is_stub; then
      warn_stale "$newest_src_path" "$dist_iso" "$src_iso"
      # Drop a sentinel next to the dist so the Makefile web-ensure
      # gate can rebuild without re-implementing "is dist stale".
      # (env-var doesn't propagate across make's per-line subshells.)
      # The sentinel sits inside web/dist/ which is gitignored.
      mkdir -p "$(dirname "$INDEX")"
      : > "$REPO_ROOT/web/dist/.branchdam-stale"
    fi
  fi
fi
