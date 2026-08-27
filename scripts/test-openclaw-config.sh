#!/usr/bin/env bash
# Gateway config (openclaw.json) renderer test.
#
# The old deploy/openclaw-config.jq and its shell test were replaced by the
# internal/gateway renderer unit tests (issue #6). This file is a thin shim so
# the pull_request_target workflow (which loads the workflow definition from
# main, where this step still references this script) keeps passing until the
# workflow change lands on main. It runs the Go renderer tests.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"
go test ./internal/gateway/...
