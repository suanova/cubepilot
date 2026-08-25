#!/usr/bin/env bash
# CubePilot end-to-end test: bring up the stack via scripts/setup.sh on a kind
# cluster and verify it. Deploy path always runs; the conversational path runs
# when CUBEPILOT_E2E_CHAT=1 (needs a real provider key).
#
#   CUBEPILOT_MODEL_PROVIDERS='{...}' scripts/e2e.sh             # deploy only
#   CUBEPILOT_MODEL_PROVIDERS='{...}' CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAMESPACE="${CUBEPILOT_NAMESPACE:-cubepilot}"
KIND_CLUSTER="${CUBEPILOT_KIND_CLUSTER:-cube}"
E2E_USER="${CUBEPILOT_E2E_USER:-zhang.wei}"
CHAT="${CUBEPILOT_E2E_CHAT:-0}"

fail() { printf '\n\033[1;31m[e2e] FAIL\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[1;34m[e2e]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[e2e] ok\033[0m %s\n' "$*"; }

# ---------- deploy phase --------------------------------------------------
step "deploy via scripts/setup.sh"
[ -n "${CUBEPILOT_MODEL_PROVIDERS:-}" ] || fail "CUBEPILOT_MODEL_PROVIDERS is required"
"$REPO_DIR/scripts/setup.sh"

step "verify kind cluster + namespace"
kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER" || fail "kind cluster '$KIND_CLUSTER' missing"
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || fail "namespace '$NAMESPACE' missing"
ok "cluster + namespace"

step "verify chart CRDs (assistant.suanova.io)"
for c in agenttemplates agentinstances capabilities tasks taskruns tasktemplates; do
  kubectl get crd "$c.assistant.suanova.io" >/dev/null 2>&1 || fail "CRD $c.assistant.suanova.io missing"
done
ok "6 CRDs"

step "verify shared secrets"
kubectl -n "$NAMESPACE" get secret openclaw-config >/dev/null 2>&1 || fail "secret openclaw-config missing"
kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.gatewayToken}' | base64 -d | grep -q . \
  || fail "openclaw-config: gatewayToken empty"
kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.openclaw\.json}' | base64 -d \
  | jq -e '.gateway.mode == "local"' >/dev/null 2>&1 || fail "openclaw-config: openclaw.json invalid"
kubectl -n "$NAMESPACE" get secret agent-kubeconfig >/dev/null 2>&1 || fail "secret agent-kubeconfig missing"
ok "secrets"

step "verify deployments ready (operator/api/web)"
for dep in cubepilot-operator cubepilot-api cubepilot-web; do
  kubectl -n "$NAMESPACE" rollout status deployment/"$dep" --timeout=240s >/dev/null || fail "deployment $dep not ready"
done
ok "operator/api/web ready"

PF_PIDS=""
cleanup() { [ -n "$PF_PIDS" ] && kill $PF_PIDS 2>/dev/null || true; }
trap cleanup EXIT

step "verify api /healthz"
kubectl -n "$NAMESPACE" port-forward svc/cubepilot-api 18081:8080 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
sleep 2
curl -sf --max-time 10 http://127.0.0.1:18081/healthz >/dev/null || fail "cubepilot-api /healthz failed"
ok "api /healthz"

step "verify portal serves HTML"
kubectl -n "$NAMESPACE" port-forward svc/cubepilot 18080:8080 >/dev/null 2>&1 & PF_PIDS="$PF_PIDS $!"
sleep 2
curl -sf --max-time 10 http://127.0.0.1:18080/ | grep -qi '<html' || fail "portal did not serve HTML"
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
DONE_ERR="$(awk '/^event: message_done/{getline; print}' "$TMP/sse.out" | sed 's/^data: //' | jq -r '.error // ""')"
[ -z "$DONE_ERR" ] || fail "message_done carried an error: $DONE_ERR"
ok "chat streamed a reply (message_delta -> message_done, no error)"

step "verify agent pod running"
kubectl -n "$NAMESPACE" get pods | grep -E "agent-${E2E_USER}" | grep -q "Running" \
  || fail "no Running agent-${E2E_USER} pod"
ok "agent pod running"

echo "E2E PASS (deploy + chat)"
