#!/usr/bin/env bash
# Mirror CubePilot base images from public registries into
# harbor.isuanova.com/library. Base images keep their exact public tag and
# only change registry, so Dockerfiles / CI references resolve unchanged.
#
# Requires: docker login harbor.isuanova.com (maintainer or above on library).
# Reference: docs/superpowers/specs/2026-08-26-harbor-image-migration-design.md
set -euo pipefail

REG="harbor.isuanova.com/library"

mirror() { # $1=source ref  $2=target ref
  echo "[mirror] $1 -> $2"
  docker buildx imagetools create -t "$2" "$1"
}

mirror docker.io/library/golang:1.26-bookworm  "$REG/golang:1.26-bookworm"
mirror docker.io/library/node:24-bookworm-slim "$REG/node:24-bookworm-slim"
mirror docker.io/library/node:22-alpine        "$REG/node:22-alpine"
mirror docker.io/library/nginx:1.27-alpine     "$REG/nginx:1.27-alpine"
mirror ghcr.io/openclaw/openclaw:2026.6.33     "$REG/openclaw:2026.6.33"

echo "mirror done: $REG"
