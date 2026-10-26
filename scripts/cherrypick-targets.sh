#!/usr/bin/env bash
# Parse the target branches from a /cherrypick comment. Only the first line is
# read; branches are whitespace-separated. Prints them space-separated so the
# caller can pass the result straight to backport-action's target_branches.
set -euo pipefail

# Glob metacharacters are invalid in refs, so never expand a target like release-*.
set -f

first_line=$(head -n1)
first_line=${first_line%$'\r'}   # a browser may submit CRLF

read -r command rest <<<"$first_line" || true
if [ "$command" != /cherrypick ] || [ -z "${rest//[[:space:]]/}" ]; then
  echo "expected '/cherrypick <target-branch>...' on the first line" >&2
  exit 1
fi

targets=()
for branch in $rest; do
  if [ "$branch" = main ]; then
    echo 'cannot cherry-pick into main' >&2
    exit 1
  fi
  if ! git check-ref-format --branch "$branch" >/dev/null 2>&1; then
    echo "'$branch' is not a valid branch name" >&2
    exit 1
  fi
  targets+=("$branch")
done

printf '%s' "${targets[*]}"
