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
# Built images are registry-addressed (harbor.isuanova.com/cubestack); set
# CUBEPILOT_PUSH=1 to push after building (dev machines may lack creds).
IMAGE_REPO="${CUBEPILOT_IMAGE_REPO:-harbor.isuanova.com/cubestack}"
IMAGE_TAG="${CUBEPILOT_IMAGE_TAG:-local}"
PUSH="${CUBEPILOT_PUSH:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --providers-json)     MODEL_PROVIDERS="${2:?--providers-json requires a JSON value}"; shift 2 ;;
    --default-model)      DEFAULT_MODEL="${2:?--default-model requires a value}"; shift 2 ;;
    --gateway-token)      GATEWAY_TOKEN="${2:?--gateway-token requires a value}"; shift 2 ;;
    --kind-cluster)       KIND_CLUSTER="${2:?--kind-cluster requires a value}"; shift 2 ;;
    --namespace)          NAMESPACE="${2:?--namespace requires a value}"; shift 2 ;;
    --openclaw-image-tag) OPENCLAW_IMAGE_TAG="${2:?--openclaw-image-tag requires a value}"; shift 2 ;;
    --skip-cluster-create) SKIP_CLUSTER_CREATE=1; shift ;;
    --push)              PUSH=1; shift ;;
    -h|--help)
      cat <<'EOF'
CubePilot setup -- bring up the stack on a kind cluster with zero host config.

Usage: scripts/setup.sh [flags]

Required:
  CUBEPILOT_MODEL_PROVIDERS / --providers-json <json>
      The models.providers object (OpenClaw provider config), e.g.
      '{"deepseek":{"api":"openai-completions","apiKey":"sk-...",
        "baseUrl":"https://api.deepseek.com",
        "models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"}]}}'
      api is the OpenClaw API style (openai-completions for DeepSeek);
      apiKey holds the secret. The provider key is arbitrary (it is only the
      first half of the gateway's model ref). The agent's default model is the
      first provider's first model (or CUBEPILOT_DEFAULT_MODEL); to switch an
      agent to another model, add it to the AgentTemplate models and set
      AgentInstance.selectedModel. Each model should carry "name" (the renderer
      fills it from "id" if omitted). One provider is enough for testing.

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
  CUBEPILOT_PUSH / --push
      Push the four built images to CUBEPILOT_IMAGE_REPO after building.
  CUBEPILOT_IMAGE_REPO / CUBEPILOT_IMAGE_TAG
      Image repository and tag for the built cubepilot images
      (default: harbor.isuanova.com/cubestack, local).
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
echo "$MODEL_PROVIDERS" | jq -e 'type == "object" and any(to_entries[]; ((.value.models // []) | length) >= 1)' >/dev/null \
  || { echo "error: CUBEPILOT_MODEL_PROVIDERS must be an object with at least one provider having >=1 model" >&2; exit 1; }

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
log "building images ($IMAGE_REPO, tag $IMAGE_TAG)"
docker build -t "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" --build-arg OPENCLAW_IMAGE_TAG="$OPENCLAW_IMAGE_TAG" \
  -f "$REPO_DIR/deploy/openclaw-image.Dockerfile" "$REPO_DIR"
docker build -t "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" -f "$REPO_DIR/deploy/operator-image.Dockerfile" "$REPO_DIR"
docker build -t "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG"      -f "$REPO_DIR/deploy/api-image.Dockerfile"      "$REPO_DIR"
docker build -t "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"      -f "$REPO_DIR/web/Dockerfile"                  "$REPO_DIR/web"

if [ "$PUSH" = "1" ]; then
  log "pushing images to $IMAGE_REPO"
  docker push "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG"
  docker push "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG"
  docker push "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG"
  docker push "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"
fi

log "loading images into kind ($KIND_CLUSTER)"
kind load docker-image "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" \
  "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG" "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG" --name "$KIND_CLUSTER"

log "creating namespace + RBAC"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

log "creating shared secrets"
kubectl -n "$NAMESPACE" create secret generic agent-kubeconfig \
  --from-file=config="$REPO_DIR/deploy/agent-kubeconfig.yaml" \
  --dry-run=client -o yaml | kubectl apply -f -

# gateway token: caller-supplied, reused from a prior run, or auto-generated
# (never read from a host file). Reuse keeps the operator-created agent gateway
# Pods working across re-runs: they read OPENCLAW_GATEWAY_TOKEN from the
# openclaw-config Secret via secretKeyRef, which does not hot-update.
if [ -z "$GATEWAY_TOKEN" ]; then
  EXISTING="$(kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.gatewayToken}' 2>/dev/null || true)"
  if [ -n "$EXISTING" ]; then
    GATEWAY_TOKEN="$(printf '%s' "$EXISTING" | base64 -d)"
    log "reusing existing gateway token from openclaw-config secret"
  else
    GATEWAY_TOKEN="$(openssl rand -hex 32)"
    log "generated a random gateway token (set CUBEPILOT_GATEWAY_TOKEN to pin it)"
  fi
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
  --set agents.image="$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" \
  --set operator.image="$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" \
  --set api.image="$IMAGE_REPO/cubepilot-api:$IMAGE_TAG" \
  --set web.image="$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"

log "done. expose the portal with:"
log "  kubectl -n $NAMESPACE port-forward svc/cubepilot 8080:8080"
log "then open http://127.0.0.1:8080"
