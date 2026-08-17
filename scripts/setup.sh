#!/usr/bin/env bash
# CubePilot PoC setup: build images, load into kind, create shared Secrets and
# RBAC. Requires: docker, kind (cluster "cube"), kubectl, jq.
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
[ -f "$HOST_CONFIG" ] || { echo "missing $HOST_CONFIG"; exit 1; }

log "building Go binary (host) + images"
(cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/cubepilot ./cmd/cubepilot)
docker build -t cubepilot-agent:local   -f "$REPO_DIR/deploy/agent-image.Dockerfile"   "$REPO_DIR"
docker build -t cubepilot-service:local -f "$REPO_DIR/deploy/service-image.Dockerfile" "$REPO_DIR"

log "loading images into kind ($KIND_CLUSTER)"
kind load docker-image cubepilot-agent:local cubepilot-service:local --name "$KIND_CLUSTER"

log "creating namespace + RBAC"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$REPO_DIR/deploy/rbac.yaml"

log "creating shared secrets"
# 1. agent-kubeconfig: in-cluster kubeconfig using the Pod's own SA token.
kubectl -n "$NAMESPACE" create secret generic agent-kubeconfig \
  --from-file=config="$REPO_DIR/deploy/agent-kubeconfig.yaml" \
  --dry-run=client -o yaml | kubectl apply -f -

# 2. openclaw-config: host LLM config, adjusted for the agent Pod runtime.
#    - enable the OpenAI-compatible chat endpoint (agent loop over HTTP)
#    - point the workspace at the baked-in capability catalog
#    - exec "full/off" = write-direct (phase one, no HITL)
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
jq '
  del(.mcp.servers, .channels, .plugins)
  | .gateway.http = (.gateway.http // {})
  | .gateway.http.endpoints = (.gateway.http.endpoints // {})
  | .gateway.http.endpoints.chatCompletions = (.gateway.http.endpoints.chatCompletions // {})
  | .gateway.http.endpoints.chatCompletions.enabled = true
  | .agents.defaults.workspace = "/opt/cubepilot/workspace"
  | .agents.defaults.model = (.agents.defaults.model // {})
  | .agents.defaults.model.primary = "cuberouter/glm-5.1"
  | .models.providers.cuberouter.models = (.models.providers.cuberouter.models + [{"id":"glm-5.1","name":"GLM 5.1"}] | unique_by(.id))
  | .tools.exec = (.tools.exec // {})
  | .tools.exec.security = "full"
  | .tools.exec.ask = "off"
  | .tools.sessions = (.tools.sessions // {})
  | .tools.sessions.visibility = "all"
' "$HOST_CONFIG" > "$TMP/openclaw.json"

GATEWAY_TOKEN="$(jq -r '.gateway.auth.token // empty' "$HOST_CONFIG")"
[ -n "$GATEWAY_TOKEN" ] || { echo "gateway.auth.token not found in $HOST_CONFIG"; exit 1; }

kubectl -n "$NAMESPACE" create secret generic openclaw-config \
  --from-file=openclaw.json="$TMP/openclaw.json" \
  --from-literal=gatewayToken="$GATEWAY_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

log "deploying assistant service + Instance Manager"
kubectl apply -f "$REPO_DIR/deploy/service.yaml"

log "done. expose the portal with:"
log "  kubectl -n $NAMESPACE port-forward svc/cubepilot 8080:8080"
log "then open http://127.0.0.1:8080"
