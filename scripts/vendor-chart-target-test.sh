#!/usr/bin/env bash
# Unit tests for vendor-chart-target.sh. Run from the repo root: make test.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="${here}/vendor-chart-target.sh"
fail=0

check() {
  local desc=$1 expected=$2
  shift 2
  local actual
  if actual=$("$script" "$@" 2>/dev/null); then :; else actual=FAILED; fi
  if [ "$actual" = "$expected" ]; then
    printf 'ok   %s -> %s\n' "$desc" "$actual"
  else
    printf 'FAIL %s: expected %s, got %s\n' "$desc" "$expected" "$actual" >&2
    fail=1
  fi
}

check "main maps to main"             main          branch main
check "release branch maps to itself" release-2.0   branch release-2.0
check "release tag maps to line"      release-2.0   tag v2.0.0
check "prerelease tag maps to line"   release-2.0   tag v2.0.0-rc1
check "two-digit minor maps"          release-10.20 tag v10.20.30
check "malformed tag fails"           FAILED        tag v2.0
check "unknown ref type fails"        FAILED        tag-or-branch v2.0.0

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "all target-resolution cases passed"
