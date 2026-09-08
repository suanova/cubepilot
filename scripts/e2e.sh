#!/usr/bin/env bash
# CubePilot end-to-end test: bring up the stack via scripts/setup.sh on a kind
# cluster and verify it. Deploy path always runs; the conversational path runs
# when CUBEPILOT_E2E_CHAT=1 (needs a real provider key).
#
#   CUBEPILOT_LLM_APIKEY='sk-placeholder' scripts/e2e.sh          # deploy only
#   CUBEPILOT_LLM_APIKEY='sk-real' scripts/e2e.sh                 # deploy + chat (auto)
#   CUBEPILOT_LLM_APIKEY='sk-real' CUBEPILOT_LLM_ENDPOINT='https://...' scripts/e2e.sh
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE="${CUBEPILOT_NAMESPACE:-cubepilot}"
KIND_CLUSTER="${CUBEPILOT_KIND_CLUSTER:-cube}"
E2E_USER="${CUBEPILOT_E2E_USER:-admin}"
# The conversational e2e runs by default once a real apiKey is configured
# (same as CI); an explicit CUBEPILOT_E2E_CHAT=0/1 overrides. A placeholder
# key (deploy-only runs) skips chat.
CHAT="${CUBEPILOT_E2E_CHAT:-}"
if [ -z "$CHAT" ]; then
  if [ -n "${CUBEPILOT_LLM_APIKEY:-}" ] && [ "$CUBEPILOT_LLM_APIKEY" != "sk-placeholder" ]; then
    CHAT="1"
  else
    CHAT="0"
  fi
fi

fail() { printf '\n\033[1;31m[e2e] FAIL\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[1;34m[e2e]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[e2e] ok\033[0m %s\n' "$*"; }

# ---------- deploy phase --------------------------------------------------
step "deploy via scripts/setup.sh"
[ -n "${CUBEPILOT_LLM_APIKEY:-}" ] || fail "CUBEPILOT_LLM_APIKEY is required"
"$REPO_DIR/scripts/setup.sh"

step "verify kind cluster + namespace"
kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER" || fail "kind cluster '$KIND_CLUSTER' missing"
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || fail "namespace '$NAMESPACE' missing"
ok "cluster + namespace"

step "verify chart CRDs (ai.cubestack.io)"
for c in agenttemplates agentinstances skills tasktemplates tasks taskruns; do
  kubectl get crd "$c.ai.cubestack.io" >/dev/null 2>&1 || fail "CRD $c.ai.cubestack.io missing"
done
ok "6 CRDs"

step "verify shared secrets"
kubectl -n "$NAMESPACE" get secret agent-kubeconfig >/dev/null 2>&1 || fail "secret agent-kubeconfig missing"
kubectl -n "$NAMESPACE" get secret cubepilot-llm >/dev/null 2>&1 || fail "secret cubepilot-llm missing"
ok "secrets"

step "verify deployments ready (operator/api/web)"
for dep in cubepilot-operator cubepilot-api cubepilot-web; do
  kubectl -n "$NAMESPACE" rollout status deployment/"$dep" --timeout=240s >/dev/null || fail "deployment $dep not ready"
done
ok "operator/api/web ready"

step "verify operator-generated openclaw-config"
# The operator creates the Secret with the gatewayToken first and writes
# openclaw.json on the first reconcile, so wait until the rendered config is
# present and valid (not just until the Secret exists).
for _ in $(seq 1 30); do
  if kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.openclaw\.json}' 2>/dev/null \
    | base64 -d | jq -e '.gateway.mode == "local"' >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.gatewayToken}' | base64 -d | grep -q . \
  || fail "openclaw-config: gatewayToken empty"
kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.openclaw\.json}' | base64 -d \
  | jq -e '.gateway.mode == "local"' >/dev/null 2>&1 || fail "openclaw-config: openclaw.json invalid"
ok "operator-generated config"

PF_PIDS=""
cleanup() { [ -n "$PF_PIDS" ] && kill $PF_PIDS 2>/dev/null || true; }
trap cleanup EXIT

step "verify api /healthz"
kubectl -n "$NAMESPACE" port-forward svc/cubepilot-api 18081:8080 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
for _ in $(seq 1 20); do
  curl -sf --max-time 5 http://127.0.0.1:18081/healthz >/dev/null 2>&1 && break
  sleep 1
done
curl -sf --max-time 5 http://127.0.0.1:18081/healthz >/dev/null || fail "cubepilot-api /healthz failed"
ok "api /healthz"

step "verify portal serves HTML"
kubectl -n "$NAMESPACE" port-forward svc/cubepilot 18080:8080 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
for _ in $(seq 1 20); do
  curl -sf --max-time 5 http://127.0.0.1:18080/ | grep -qi '<html' && break
  sleep 1
done
curl -sf --max-time 5 http://127.0.0.1:18080/ | grep -qi '<html' || fail "portal did not serve HTML"
ok "portal HTML"
ok "deploy path verified"

# ---------- chat phase (optional) ----------------------------------------
if [ "$CHAT" != "1" ]; then
  step "conversational e2e skipped (set CUBEPILOT_E2E_CHAT=1; requires a real provider key)"
  echo "E2E PASS (deploy only)"
  exit 0
fi

step "chat e2e (user=$E2E_USER)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"; cleanup' EXIT
SESSION="e2e-$(date +%s)"
BODY="$(jq -n --arg s "$SESSION" --arg c "你是 CubePilot 平台助手。请用一句话回复:你好。" \
  '{session_id: $s, content: $c}')"
curl -sN --max-time 300 -X POST "http://127.0.0.1:18080/api/messages" \
  -H 'Content-Type: application/json' \
  -H "X-CubePilot-User: $E2E_USER" \
  -d "$BODY" > "$TMP/sse.out" || fail "chat POST failed"
grep -q '^event: message_delta' "$TMP/sse.out" || fail "SSE missing message_delta"
grep -q '^event: message_done' "$TMP/sse.out" || fail "SSE missing message_done"
DONE_ERR="$(awk '/^event: message_done/{f=1} f && /^data:/{print; exit}' "$TMP/sse.out" | sed 's/^data: //' | jq -r '.error // ""')" \
  || fail "could not parse message_done payload"
[ -z "$DONE_ERR" ] || fail "message_done carried an error: $DONE_ERR"
ok "chat streamed a reply (message_delta -> message_done, no error)"

step "verify agent pod running"
kubectl -n "$NAMESPACE" get pods | grep -E "agent-${E2E_USER}" | grep -q "Running" \
  || fail "no Running agent-${E2E_USER} pod"
ok "agent pod running"

# System Prompt under WS-only chat (issue #137): a per-user system prompt is
# stored on the AgentInstance and rendered by the supervisor into the workspace
# AGENTS.md on its next poll (~10s). Assert the marker block appears on disk
# (deterministic) -- not that a model happens to follow it.
step "system prompt renders into the agent workspace AGENTS.md"
SP_MARKER="e2e-system-prompt-$(date +%s)"
curl -sf --max-time 10 -X PUT "http://127.0.0.1:18080/api/agent/config" \
  -H 'Content-Type: application/json' \
  -H "X-CubePilot-User: $E2E_USER" \
  -d "$(jq -n --arg m "$SP_MARKER" '{config:{model:"",systemPrompt:$m}}')" >/dev/null \
  || fail "system prompt PUT failed"
AGENT_POD="$(kubectl -n "$NAMESPACE" get pods -o name | grep -E "pod/agent-${E2E_USER}-" | head -1 | sed 's#pod/##')"
[ -n "$AGENT_POD" ] || fail "could not resolve agent pod for $E2E_USER"
found=""
for _ in $(seq 1 15); do
  if kubectl -n "$NAMESPACE" exec "$AGENT_POD" -c supervisor -- \
      grep -q "cubepilot:system-prompt" /home/node/.openclaw/workspace/AGENTS.md 2>/dev/null; then
    found=1
    break
  fi
  sleep 2
done
[ -n "$found" ] || fail "managed system-prompt block not found in AGENTS.md within ~30s"
kubectl -n "$NAMESPACE" exec "$AGENT_POD" -c supervisor -- \
  grep -q "$SP_MARKER" /home/node/.openclaw/workspace/AGENTS.md \
  || fail "AGENTS.md does not carry the saved system prompt text"
ok "system prompt block present in the agent workspace AGENTS.md"

echo "E2E PASS (deploy + chat)"
