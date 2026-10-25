# syntax=docker/dockerfile:1

# --- Stage 1: build the SPA ---
FROM node:26-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
# The SPA build id (Settings → "UI build") is stamped into the JS
# bundle as __BRANCHDAM_BUILD__ and into web/dist/BUILD_ID
# (web/resolve-build-id.ts). BRANCHDAM_BUILD_ID is the explicit override
# release.yml still passes (the release tag) and always wins. Otherwise
# a checkout's `git rev-parse --short HEAD` wins, so a stray host
# COMMIT_SHA cannot outrank the real commit. COMMIT_SHA — injected by
# docker-publish from the checked-out commit (s3ntin3l8/.github#97), not
# github.sha — applies only once .git is gone, which it is here. Last
# resort is dev-docker. Both ARGs stay in this web stage: the value is
# baked into the bundle, and the final debian stage never mentions
# them, which the shared build-arg check allows. Empty defaults so an
# unset override does not shadow COMMIT_SHA.
ARG COMMIT_SHA=""
ARG BRANCHDAM_BUILD_ID=""
ENV COMMIT_SHA=$COMMIT_SHA
ENV BRANCHDAM_BUILD_ID=$BRANCHDAM_BUILD_ID
RUN npm run build

# --- Stage 2: static ffprobe + ffmpeg, pinned by digest (not tag) for
# supply-chain integrity. mwader/static-ffmpeg builds genuinely static (no
# shared libs) binaries; copying just these two avoids apt-get install
# ffmpeg's much larger transitive dependency tree (GTK, SDL2, PulseAudio,
# Sphinx, etc.). branchDAM previously only ever ran ffprobe, never
# ffmpeg/ffplay -- #224 added ffmpeg too, for video poster-frame thumbnail
# extraction (internal/probe.ExtractVideoPoster /
# internal/thumbs.Cache.Generate); ffplay is still never shipped, since
# nothing in this codebase plays media. The ffmpeg binary itself is
# ~134MB uncompressed, ~55MB gzip-compressed on the wire (measured against
# this exact digest) -- a deliberate, documented tradeoff, not an oversight;
# see docs/operations.md's Docker image size note. Multi-arch: the manifest
# list covers both linux/amd64 and linux/arm64, matching docker-publish.yml's
# build matrix.
FROM mwader/static-ffmpeg@sha256:028fb402231d4f5f2223c67fca5d0abc04f5a38cffcbadf9844f06985e2259b8 AS ffprobe

# --- Stage 3: build the Go binary (with embedded dist) ---
# golang:1.27-bookworm, NOT -alpine: CGO_ENABLED=1 (mattn/go-sqlite3, see
# docs/schema.md) needs a glibc builder to match the glibc runtime below --
# an alpine (musl) build here would not run on debian-slim. CI builds each
# platform on a native runner (docker-publish.yml), so CGO's usual
# cross-compile pain doesn't apply.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# overwrite the placeholder dist (.github/ci-prebuild.sh's stub) with the
# freshly built SPA
COPY --from=web /web/dist ./web/dist
RUN VERSION="$(sed -n 's/.*: *"\([0-9][^"]*\)".*/\1/p' .release-please-manifest.json)"; \
    CGO_ENABLED=1 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION:-dev}" \
      -o /out/branchdam ./cmd/branchdam

# --- Stage 4: runtime ---
# debian-slim, not distroless: exiftool (Perl) needs a real userland, and
# distroless ships neither a shell nor apt. This is the one deliberate
# deviation from traefik-viewer's Dockerfile shape -- forced by directive
# 9.4's exiftool/ffprobe requirement, not a style choice.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      libimage-exiftool-perl ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -r -u 65532 -m -d /nonexistent -s /usr/sbin/nologin nonroot
COPY --from=build /out/branchdam /usr/local/bin/branchdam
COPY --from=ffprobe /ffprobe /usr/local/bin/ffprobe
COPY --from=ffprobe /ffmpeg /usr/local/bin/ffmpeg
# /data must exist and be owned by nonroot BEFORE the volume mount happens:
# Docker copies a fresh named volume's initial content (and the mount
# point's ownership) from what's already in the image at that path. Without
# this, a brand new `branchdam-data` volume mounts as root:root, and the
# nonroot process can't create the SQLite file inside it -- "unable to open
# database file" masking what is actually a permissions error. Verified by
# actually running the built image against a fresh volume before writing
# this comment.
RUN mkdir -p /data && chown nonroot:nonroot /data
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/branchdam", "-healthcheck", "-config", "/config/config.yaml"]
ENTRYPOINT ["/usr/local/bin/branchdam"]
CMD ["-config", "/config/config.yaml"]
