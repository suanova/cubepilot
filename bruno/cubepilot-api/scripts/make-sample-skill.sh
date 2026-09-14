#!/usr/bin/env bash
# Repack fixtures/sample-skill into the gzip tar the publish endpoint expects.
# The archive must have SKILL.md at its ROOT -- not inside a wrapping directory.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$here/fixtures/sample-skill.tar.gz"
tar czf "$out" -C "$here/fixtures/sample-skill" .
echo "wrote $out"
tar tzf "$out"
