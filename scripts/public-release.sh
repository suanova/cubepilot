#!/bin/bash
set -euo pipefail

# Generate the public-release manifest for a published cubepilot tag.
#
# The manifest is uploaded as a GitHub Release asset by
# .github/workflows/public-release.yaml. It records the chart/image references
# and their Harbor digests so consumers can install from public Harbor without
# guessing what a tag resolved to.

REGISTRY="${REGISTRY:-harbor.isuanova.com}"
PROJECT="${PROJECT:-suanova}"
TAG=""
OUTPUT="public-release.yaml"
COMMIT=""
RELEASED_AT=""
TIMEOUT="${PUBLIC_RELEASE_TIMEOUT:-300}"
INTERVAL="${PUBLIC_RELEASE_INTERVAL:-10}"

die() { echo "ERROR: $*" >&2; exit 1; }
say() { echo "==> $*"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --tag) TAG="${2:?--tag needs a value}"; shift 2 ;;
        --output) OUTPUT="${2:?--output needs a path}"; shift 2 ;;
        --commit) COMMIT="${2:?--commit needs a value}"; shift 2 ;;
        --released-at) RELEASED_AT="${2:?--released-at needs a value}"; shift 2 ;;
        --help|-h) sed -n '2,14p' "$0"; exit 0 ;;
        *) die "unknown option: $1" ;;
    esac
done
[ -n "${TAG}" ] || die "--tag is required"
for cmd in skopeo jq; do
    command -v "${cmd}" >/dev/null 2>&1 || die "${cmd} is required"
done

VERSION="${TAG#v}"
IMAGE_TAG="v${VERSION}"
SEMVER='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*))?$'
printf '%s' "${VERSION}" | grep -Eq "${SEMVER}" || die "tag is not semver: ${TAG}"

IMAGES=(cubepilot-openclaw cubepilot-operator cubepilot-api cubepilot-web)
CHART_REPO="${REGISTRY}/${PROJECT}/cubepilot-chart"

image_digest() {
    skopeo inspect --format '{{.Digest}}' "docker://${REGISTRY}/${PROJECT}/$1:${IMAGE_TAG}" 2>/dev/null
}

chart_digest() {
    local raw
    raw="$(skopeo inspect --raw "docker://${CHART_REPO}:${VERSION}" 2>/dev/null)" || return 1
    printf '%s' "${raw}" | jq -r '
        ([.layers[]? | select(.mediaType == "application/vnd.cncf.helm.chart.content.v1.tar+gzip") | .digest][0])
        // ([.layers[].digest][0]) // empty'
}

say "waiting for cubepilot ${TAG} artifacts in ${REGISTRY}/${PROJECT}"
deadline=$((SECONDS + TIMEOUT))
declare -A DIGESTS=()
while :; do
    missing=0
    for image in "${IMAGES[@]}"; do
        if [ -z "${DIGESTS[$image]:-}" ]; then
            DIGESTS[$image]="$(image_digest "${image}")" || DIGESTS[$image]=""
            [ -n "${DIGESTS[$image]}" ] || missing=$((missing + 1))
        fi
    done
    if [ -z "${CHART_DIGEST:-}" ]; then
        CHART_DIGEST="$(chart_digest)" || CHART_DIGEST=""
        [ -n "${CHART_DIGEST}" ] || missing=$((missing + 1))
    fi
    [ "${missing}" -eq 0 ] && break
    [ "${SECONDS}" -ge "${deadline}" ] && die "artifacts not available after ${TIMEOUT}s"
    say "  missing ${missing} artifact(s); retrying in ${INTERVAL}s"
    sleep "${INTERVAL}"
done

[ -n "${RELEASED_AT}" ] || RELEASED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

tmp="${OUTPUT}.tmp"
{
    echo "schema: 1"
    echo "component: cubepilot"
    echo "version: \"${VERSION}\""
    echo "source:"
    echo "  repo: suanova/cubepilot"
    echo "  tag: \"${TAG}\""
    [ -n "${COMMIT}" ] && echo "  commit: \"${COMMIT}\""
    echo "released_at: \"${RELEASED_AT}\""
    echo "chart:"
    echo "  ref: oci://${CHART_REPO}"
    echo "  version: \"${VERSION}\""
    echo "  digest: \"${CHART_DIGEST}\""
    echo "images:"
    for image in "${IMAGES[@]}"; do
        echo "  - name: ${image}"
        echo "    ref: ${REGISTRY}/${PROJECT}/${image}:${IMAGE_TAG}"
        echo "    digest: \"${DIGESTS[$image]}\""
    done
    echo "install:"
    echo "  namespace: cubepilot"
    echo "  command: |"
    echo "    helm install cubepilot oci://${CHART_REPO} --version ${VERSION} -n cubepilot --create-namespace"
} >"${tmp}"
mv "${tmp}" "${OUTPUT}"

say "wrote ${OUTPUT}"
cat "${OUTPUT}"
