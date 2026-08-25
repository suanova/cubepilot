#!/usr/bin/env bash
# CubePilot setup: build images, load into kind, create shared Secrets and
# deploy components via Helm. Requires: docker, kind (cluster "cube"),
# kubectl, jq, helm.
#
# The LLM/gateway configuration is extracted from the host's existing
# ~/.openclaw/openclaw.json at runtime (never committed to the repo).
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-cube}"
NAMESPACE="${NAMESPACE:-cubepilot}"
HOST_CONFIG="${OPENCLAW_HOST_CONFIG:-$HOME/.openclaw/openclaw.json}"

log() { printf '\033[1;32m[sync]\033[0m %s\n' "$*"; }

command -v docker >/dev/null || { echo "docker required"; exit 1; }
command -v kind >/dev/null || { echo "kind required"; exit 1; }
command -v kubectl >/dev/null || { echo "kubectl required"; exit 1; }
command -v jq >/dev/null || { echo "jq required"; exit 1; }
command -v helm >/dev/null || { echo "helm required"; exit 1; }
[ -f "$HOST_CONFIG" ] || { echo "missing $HOST_CONFIG"; exit 1; }

log "building Go binaries (host) + images"
(cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/cubepilot-operator ./cmd/cubepilot-operator)
(cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/cubepilot-api ./cmd/cubepilot-api)
docker build -t cubepilot-openclaw:local    -f "$REPO_DIR/deploy/openclaw-image.Dockerfile"    "$REPO_DIR"
docker build -t cubepilot-operator:local -f "$REPO_DIR/deploy/operator-image.Dockerfile" "$REPO_DIR"
docker build -t cubepilot-api:local      -f "$REPO_DIR/deploy/api-image.Dockerfile"      "$REPO_DIR"
# Portal SPA -- multi-stage node build -> nginx (independent component, §9).
docker build -t cubepilot-web:local      -f "$REPO_DIR/web/Dockerfile"                  "$REPO_DIR/web"

log "loading images into kind ($KIND_CLUSTER)"
kind load docker-image cubepilot-openclaw:local cubepilot-operator:local cubepilot-api:local cubepilot-web:local --name "$KIND_CLUSTER"

log "creating namespace + RBAC"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

log "installing platform CRDs (design §3: Agent / AgentInstance / Capability / TaskTemplate / Task / TaskRun)"
kubectl apply -f "$REPO_DIR/config/crd/bases/"

log "creating shared secrets"
# 1. agent-kubeconfig: in-cluster kubeconfig using the Pod's own SA token.
kubectl -n "$NAMESPACE" create secret generic agent-kubeconfig \
  --from-file=config="$REPO_DIR/deploy/agent-kubeconfig.yaml" \
  --dry-run=client -o yaml | kubectl apply -f -

# 2. openclaw-config: in-Pod gateway config, **allowlist extraction** (only model
#    credentials + fields the agent needs at runtime; host-only fields
#    (workspace/agents.list/mcp/channels/plugins) cannot leak by construction;
#    the host ~/.openclaw/openclaw.json is only a dev-time credential source,
#    production credential governance lives in design §3.3 Model.credentialRef
#    (phase two).
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# Default backend model for agent gateways. Empty = inherit the host config's
# .agents.defaults.model.primary untouched (recommended; the Model catalog,
# not the host config, is the long-term source -- §3.3). Set CUBEPILOT_DEFAULT_MODEL
# to force a specific primary (e.g. deepseek-v4-flash).
DEFAULT_MODEL="${CUBEPILOT_DEFAULT_MODEL:-}"
jq --arg dm "$DEFAULT_MODEL" '
  {
    models: { providers: .models.providers },
    agents: {
      defaults: {
        workspace: "/home/node/.openclaw/workspace",  # in-Pod workspace (PVC, seeded by seed-workspace)
        model: .agents.defaults.model,
        models: .agents.defaults.models,
        sandbox: .agents.defaults.sandbox,
        memorySearch: .agents.defaults.memorySearch
      }
    },
    gateway: {
      mode: .gateway.mode,                      # local (required for gateway startup; the security mechanism blocks missing mode)
      port: .gateway.port,                      # 18789 (agent svc target port)
      bind: .gateway.bind,
      controlUi: .gateway.controlUi,
      http: { endpoints: { chatCompletions: { enabled: true } } }
    },
    tools: {
      exec: { security: "full", ask: "off" },
      sessions: { visibility: "all" }
    }
  }
  | if ($dm != "") then .agents.defaults.model.primary = $dm else . end
' "$HOST_CONFIG" > "$TMP/openclaw.json"

GATEWAY_TOKEN="$(jq -r '.gateway.auth.token // empty' "$HOST_CONFIG")"
[ -n "$GATEWAY_TOKEN" ] || { echo "gateway.auth.token not found in $HOST_CONFIG"; exit 1; }

kubectl -n "$NAMESPACE" create secret generic openclaw-config \
  --from-file=openclaw.json="$TMP/openclaw.json" \
  --from-literal=gatewayToken="$GATEWAY_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

log "deploying components via Helm"
helm upgrade --install cubepilot "$REPO_DIR/deploy/charts/cubepilot" -n "$NAMESPACE" \
  --set agents.image=cubepilot-openclaw:local \
  --set operator.image=cubepilot-operator:local \
  --set api.image=cubepilot-api:local \
  --set web.image=cubepilot-web:local

log "done. expose the portal with:"
log "  kubectl -n $NAMESPACE port-forward svc/cubepilot 8080:8080"
log "then open http://127.0.0.1:8080"
