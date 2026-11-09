#!/usr/bin/env bash
# What the demo needs on a clean runner (called by the shared visual workflows, after they installed Node).
# `make demo` builds web/dist and the Go server itself (a runner has gcc for cgo SQLite, and go.mod's toolchain
# line makes Go fetch the version the module needs) and renders its synthetic media with ffmpeg and exiftool.
set -euo pipefail

npm --prefix web ci --no-audit --no-fund

sudo apt-get update -qq
sudo apt-get install -y -qq ffmpeg libimage-exiftool-perl
