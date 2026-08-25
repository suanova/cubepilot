#!/usr/bin/env bash
# CubePilot setup: ensure a kind cluster, build images (self-contained in
# Docker), create shared Secrets and deploy components via Helm.
#
# Zero host-machine dependencies by design:
#   * no ~/.openclaw/* config file is ever read;
#   * no pre-built local images (openclaw:local) or host-built bin/ artifacts;
#   * all Go binaries compile inside Docker multi-stage builds.
# The only caller-supplied input is the LLM provider credentials
# (CUBEPILOT_MODEL_PROVIDERS); the gateway token is auto-generated unless
# CUBEPILOT_GATEWAY_TOKEN is set. This is CI-friendly: a fresh runner with
# docker/kind/kubectl/helm/jq/openssl can bring the whole stack up.
#
# Requires: docker, kind, kubectl, jq, helm (v3), openssl.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# ---- inputs (env or flag) ------------------------------------------------
KIND_CLUSTER="${CUBEPILOT_KIND_CLUSTER:-cube}"
NAMESPACE="${CUBEPILOT_NAMESPACE:-cubepilot}"
OPENCLAW_IMAGE_TAG="${CUBEPILOT_OPENCLAW_IMAGE_TAG:-2026.6.33}"
MODEL_PROVIDERS="${CUBEPILOT_MODEL_PROVIDERS:-}"
DEFAULT_MODEL="${CUBEPILOT_DEFAULT_MODEL:-}"
GATEWAY_TOKEN="${CUBEPILOT_GATEWAY_TOKEN:-}"
SKIP_CLUSTER_CREATE="${CUBEPILOT_SKIP_CLUSTER_CREATE:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --providers-json)     MODEL_PROVIDERS="${2:?--providers-json requires a JSON value}"; shift 2 ;;
    --default-model)      DEFAULT_MODEL="$2"; shift 2 ;;
    --gateway-token)      GATEWAY_TOKEN="$2"; shift 2 ;;
    --kind-cluster)       KIND_CLUSTER="$2"; shift 2 ;;
    --namespace)          NAMESPACE="$2"; shift 2 ;;
    --openclaw-image-tag) OPENCLAW_IMAGE_TAG="$2"; shift 2 ;;
    --skip-cluster-create) SKIP_CLUSTER_CREATE=1; shift ;;
    -h|--help)
      cat <<'EOF'
CubePilot setup -- bring up the stack on a kind cluster with zero host config.

Usage: scripts/setup.sh [flags]

Required:
  CUBEPILOT_MODEL_PROVIDERS / --providers-json <json>
      The models.providers object (model provider credentials), e.g.
      '{"deepseek":{"api":"sk-...","baseUrl":"https://api.deepseek.com",
        "models":[{"id":"deepseek-v4-flash"}]}}'
      One provider is enough for testing.

Optional:
  CUBEPILOT_DEFAULT_MODEL / --default-model <provider/model>
      Default agent model (default: first provider's first model).
  CUBEPILOT_GATEWAY_TOKEN / --gateway-token <token>
      Gateway auth token (default: auto-generated, openssl rand -hex 32).
  CUBEPILOT_KIND_CLUSTER / --kind-cluster <name>
      Kind cluster name (default: cube).
  CUBEPILOT_NAMESPACE   / --namespace <ns>
      Target namespace (default: cubepilot).
  CUBEPILOT_OPENCLAW_IMAGE_TAG / --openclaw-image-tag <tag>
      Openclaw base image tag (default: 2026.6.33).
  CUBEPILOT_KIND_CONFIG <path>
      Kind config file used when creating the cluster.
  CUBEPILOT_SKIP_CLUSTER_CREATE / --skip-cluster-create
      Fail instead of creating the kind cluster if it is missing.
EOF
      exit 0 ;;
    *) echo "unknown argument: $1 (see --help)" >&2; exit 1 ;;
  esac
done

log() { printf '\033[1;32m[sync]\033[0m %s\n' "$*"; }

# ---- prerequisites -------------------------------------------------------
command -v docker  >/dev/null || { echo "docker required"; exit 1; }
command -v kind    >/dev/null || { echo "kind required"; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl required"; exit 1; }
command -v jq      >/dev/null || { echo "jq required"; exit 1; }
command -v helm    >/dev/null || { echo "helm required"; exit 1; }
command -v openssl >/dev/null || { echo "openssl required"; exit 1; }

[ -n "$MODEL_PROVIDERS" ] || {
  echo "error: CUBEPILOT_MODEL_PROVIDERS is required (JSON of models.providers). See --help." >&2
  exit 1
}
echo "$MODEL_PROVIDERS" | jq -e 'type == "object" and ((to_entries[0].value.models // []) | length) >= 1' >/dev/null \
  || { echo "error: CUBEPILOT_MODEL_PROVIDERS must be an object with >=1 provider and >=1 model" >&2; exit 1; }

# ---- kind cluster bootstrap ----------------------------------------------
if ! kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  if [ -n "$SKIP_CLUSTER_CREATE" ]; then
    echo "error: kind cluster '$KIND_CLUSTER' missing and --skip-cluster-create is set" >&2
    exit 1
  fi
  log "creating kind cluster '$KIND_CLUSTER'"
  kind create cluster --name "$KIND_CLUSTER" ${CUBEPILOT_KIND_CONFIG:+--config "$CUBEPILOT_KIND_CONFIG"}
else
  log "kind cluster '$KIND_CLUSTER' already exists"
fi

# ---- build images (self-contained in Docker) -----------------------------
log "building images"
docker build -t cubepilot-openclaw:local --build-arg OPENCLAW_IMAGE_TAG="$OPENCLAW_IMAGE_TAG" \
  -f "$REPO_DIR/deploy/openclaw-image.Dockerfile" "$REPO_DIR"
docker build -t cubepilot-operator:local -f "$REPO_DIR/deploy/operator-image.Dockerfile" "$REPO_DIR"
docker build -t cubepilot-api:local      -f "$REPO_DIR/deploy/api-image.Dockerfile"      "$REPO_DIR"
docker build -t cubepilot-web:local      -f "$REPO_DIR/web/Dockerfile"                  "$REPO_DIR/web"

log "loading images into kind ($KIND_CLUSTER)"
kind load docker-image cubepilot-openclaw:local cubepilot-operator:local cubepilot-api:local cubepilot-web:local \
  --name "$KIND_CLUSTER"

log "creating namespace + RBAC"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

log "creating shared secrets"
kubectl -n "$NAMESPACE" create secret generic agent-kubeconfig \
  --from-file=config="$REPO_DIR/deploy/agent-kubeconfig.yaml" \
  --dry-run=client -o yaml | kubectl apply -f -

# gateway token: caller-supplied or auto-generated (never read from a host file).
if [ -z "$GATEWAY_TOKEN" ]; then
  GATEWAY_TOKEN="$(openssl rand -hex 32)"
  log "generated a random gateway token (set CUBEPILOT_GATEWAY_TOKEN to pin it)"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "rendering allowlist openclaw.json from caller inputs (deploy/openclaw-config.jq)"
jq -n \
  --argjson providers "$MODEL_PROVIDERS" \
  --arg defaultModel "$DEFAULT_MODEL" \
  --arg token "$GATEWAY_TOKEN" \
  -f "$REPO_DIR/deploy/openclaw-config.jq" > "$TMP/openclaw.json"

kubectl -n "$NAMESPACE" create secret generic openclaw-config \
  --from-file=openclaw.json="$TMP/openclaw.json" \
  --from-literal=gatewayToken="$GATEWAY_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

log "deploying components via Helm (CRDs ship in the chart crds/ dir)"
helm upgrade --install cubepilot "$REPO_DIR/deploy/charts/cubepilot" -n "$NAMESPACE" \
  --set agents.image=cubepilot-openclaw:local \
  --set operator.image=cubepilot-operator:local \
  --set api.image=cubepilot-api:local \
  --set web.image=cubepilot-web:local

log "done. expose the portal with:"
log "  kubectl -n $NAMESPACE port-forward svc/cubepilot 8080:8080"
log "then open http://127.0.0.1:8080"
