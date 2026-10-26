#!/usr/bin/env bash
# Cases for the /cherrypick comment parser. Run directly; aggregates every
# case failure and exits non-zero at the end so it can gate CI.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/cherrypick-targets.sh"
failures=0

check() { # check <name> <want-exit> <want-output> <comment-body>
  local name=$1 want_code=$2 want_out=$3 body=$4 got_code=0 got_out
  got_out=$(printf '%s' "$body" | "$script" 2>&1) || got_code=$?
  if [ "$got_code" != "$want_code" ] || [ "$got_out" != "$want_out" ]; then
    printf 'FAIL %s\n  want exit %s, output: %s\n  got  exit %s, output: %s\n' \
      "$name" "$want_code" "$want_out" "$got_code" "$got_out"
    failures=$((failures + 1))
  else
    printf 'ok   %s\n' "$name"
  fi
}

usage="expected '/cherrypick <target-branch>...' on the first line"

check single            0 'release-2.0'                '/cherrypick release-2.0'
check multiple          0 'release-2.0 release-2.1'    '/cherrypick release-2.0 release-2.1'
check first-line-only   0 'release-2.0'                $'/cherrypick release-2.0\nanything below is ignored'
check crlf              0 'release-2.0'                $'/cherrypick release-2.0\r'
check extra-spaces      0 'release-2.0'                '/cherrypick     release-2.0'
check any-branch-name   0 'hotfix/payments'            '/cherrypick hotfix/payments'
check other-command     1 "$usage"                     '/cherrypickx release-2.0'
check no-argument       1 "$usage"                     '/cherrypick'
check no-command        1 "$usage"                     'please /cherrypick release-2.0'
check main              1 'cannot cherry-pick into main' '/cherrypick main'
check invalid-ref       1 "'bad..name' is not a valid branch name" '/cherrypick bad..name'
check option-like-ref   1 "'-x' is not a valid branch name"        '/cherrypick -x'
check one-bad-of-two    1 "'bad..name' is not a valid branch name" '/cherrypick release-2.0 bad..name'
check glob-target       1 "'release-*' is not a valid branch name" '/cherrypick release-*'
check empty-stdin       1 "$usage"                                ''
check duplicate-target  0 'release-2.0 release-2.0'               '/cherrypick release-2.0 release-2.0'
check tab-separated     0 'release-2.0 release-2.1'               $'/cherrypick\trelease-2.0\trelease-2.1'

# A real glob-expansion check: run from a directory where release-* matches.
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
touch "$tmpdir/release-2.0"
glob_code=0
glob_out=$(cd "$tmpdir" && printf '%s' '/cherrypick release-*' | "$script" 2>&1) || glob_code=$?
if [ "$glob_code" != 1 ] || [ "$glob_out" != "'release-*' is not a valid branch name" ]; then
  printf 'FAIL glob-not-expanded\n  want exit 1, output: %s\n  got  exit %s, output: %s\n' \
    "'release-*' is not a valid branch name" "$glob_code" "$glob_out"
  failures=$((failures + 1))
else
  printf 'ok   glob-not-expanded\n'
fi

if [ "$failures" -ne 0 ]; then
  printf '%s case(s) failed\n' "$failures" >&2
  exit 1
fi
echo 'all cherrypick-targets cases passed'
