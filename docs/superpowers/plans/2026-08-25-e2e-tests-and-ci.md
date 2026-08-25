# E2E Tests + CI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `scripts/e2e.sh` (deploy + gated conversational e2e on a kind cluster) and `.github/workflows/e2e.yaml` (run on every PR + push to main), so CubePilot is continuously proven end-to-end on CI.

**Architecture:** `scripts/e2e.sh` shells out to the existing `scripts/setup.sh`, then asserts the deploy path (cluster, namespace, 6 CRDs, secrets, deployment rollouts, api /healthz, portal HTML) and — only when `CUBEPILOT_E2E_CHAT=1` — the conversational path (SSE chat via `/api/messages`, cold-started agent Pod). The CI workflow has a fast `test` job and an `e2e` job that installs kind/kubectl/helm/jq, pre-pulls base images, and runs the e2e once, using the real provider key from `secrets.CUBEPILOT_MODEL_PROVIDERS` when present (chat enabled) and a hardcoded placeholder otherwise (deploy only).

**Tech Stack:** bash, kubectl/kind/helm, curl + jq, GitHub Actions (ubuntu-latest), Go 1.26.

## Global Constraints

- CRD group is **`assistant.suanova.io`** (the code's real group; `ai.cubestack.io` in the frontend API doc is aspirational). The 6 CRDs: `agenttemplates`, `agentinstances`, `capabilities`, `tasks`, `taskruns`, `tasktemplates`.
- Chat endpoint: `POST /api/messages` with JSON `{"session_id": "...", "content": "..."}` and header `X-CubePilot-User: <user>`; SSE lines are `event: <type>` then `data: <json>`. Success = `message_delta` present and `message_done` with empty `.error`.
- The agent config mounts via subPath and does not hot-update, so deploy + chat must run in ONE `setup.sh` invocation with the final provider key (no placeholder→real-key switch mid-test).
- `scripts/e2e.sh` must be executable (`chmod +x`) and exit `0` on pass / non-zero on first failed assertion; skipping chat (env unset) still exits `0`.
- Port-forwards use local ports `18080` (svc/cubepilot → 8080) and `18081` (svc/cubepilot-api → 8080) to avoid collisions; killed on EXIT.
- `.github/workflows/e2e.yaml` triggers: push to main, pull_request, workflow_dispatch. Job `e2e` depends on job `test`.
- All commits use `git commit -s` with a trailing `Assisted-by: Claude Code` line.

---

### Task 1: `scripts/e2e.sh`

**Files:**
- Create: `scripts/e2e.sh`

**Interfaces:**
- Consumes: `scripts/setup.sh` (env: `CUBEPILOT_MODEL_PROVIDERS`, `CUBEPILOT_NAMESPACE`, `CUBEPILOT_KIND_CLUSTER`); the running cluster's Services `cubepilot-api`, `cubepilot`.
- Produces: exit code only; a kind cluster named `cube` is left running (ephemeral on CI; local users can inspect or delete).

- [ ] **Step 1: Write the script**

Create `scripts/e2e.sh`:

```bash
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

echo "E2E PASS (deploy + chat)"
```

- [ ] **Step 2: Make executable + syntax check**

```bash
chmod +x scripts/e2e.sh
bash -n scripts/e2e.sh
```
Expected: `bash -n` silent.

- [ ] **Step 3: Commit**

```bash
git add scripts/e2e.sh
git commit -s -m "feat(e2e): add scripts/e2e.sh (deploy + conversational kind e2e)

Brings up the stack via scripts/setup.sh and asserts the deploy path
(cluster, 6 CRDs under assistant.suanova.io, secrets, operator/api/web
rollouts, api healthz, portal HTML). When CUBEPILOT_E2E_CHAT=1 it also drives
POST /api/messages over SSE and asserts a streamed reply plus a cold-started
Running agent Pod. CRDs are asserted against the chart's assistant.suanova.io
group.

Assisted-by: Claude Code"
```

---

### Task 2: `.github/workflows/e2e.yaml`

**Files:**
- Create: `.github/workflows/e2e.yaml`

**Interfaces:**
- Consumes: `scripts/e2e.sh` (Task 1), `scripts/setup.sh`, `scripts/test-openclaw-config.sh`, Go module.
- Produces: CI runs — `test` (vet + unit + jq test) and `e2e` (deploy, or deploy+chat when `secrets.CUBEPILOT_MODEL_PROVIDERS` is set).

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/e2e.yaml`:

```yaml
name: e2e

on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  test:
    name: Unit tests + static checks
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - run: go vet ./...
      - run: go test ./...
      - run: bash scripts/test-openclaw-config.sh

  e2e:
    name: End-to-end (kind)
    runs-on: ubuntu-latest
    timeout-minutes: 30
    needs: test
    steps:
      - uses: actions/checkout@v4

      - name: Install kind
        run: |
          sudo curl -fsSLo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/v0.32.0/kind-linux-amd64"
          sudo chmod +x /usr/local/bin/kind

      - name: Install kubectl
        run: |
          curl -fsSLO "https://dl.k8s.io/release/v1.36.1/bin/linux/amd64/kubectl"
          sudo install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl

      - name: Install helm
        run: curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

      - name: Install jq
        run: sudo apt-get update && sudo apt-get install -y jq

      - name: Pre-pull base images (de-risk BuildKit metadata pulls)
        run: |
          docker pull golang:1.26-bookworm
          docker pull node:24-bookworm-slim
          docker pull ghcr.io/openclaw/openclaw:2026.6.33
          docker pull node:22-alpine
          docker pull nginx:1.27-alpine

      # The full conversational e2e needs a real model-provider key. Configure
      # it as the GitHub secret CUBEPILOT_MODEL_PROVIDERS (the models.providers
      # object, e.g. {"deepseek":{"api":"openai-completions","apiKey":"sk-...",
      # "baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash"}]}}).
      # Without the secret the job still runs the deploy path with a placeholder.
      - name: Run e2e (deploy, or deploy + chat when the provider secret is set)
        env:
          CHAT_PROVIDER: ${{ secrets.CUBEPILOT_MODEL_PROVIDERS }}
        run: |
          set -euo pipefail
          if [ -n "$CHAT_PROVIDER" ]; then
            echo "::group::e2e deploy + chat (real provider)"
            CUBEPILOT_MODEL_PROVIDERS="$CHAT_PROVIDER" CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh
            echo "::endgroup::"
          else
            echo "::group::e2e deploy only (placeholder provider)"
            CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-placeholder","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"}]}}' scripts/e2e.sh
            echo "::endgroup::"
          fi
```

- [ ] **Step 2: Validate the YAML**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/e2e.yaml'))"` (or `ruby -e 'require "yaml"; YAML.load_file(...)'` if pyyaml is absent — report which).
Expected: parses without error.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/e2e.yaml
git commit -s -m "ci: add GitHub Actions e2e workflow

Runs go vet + unit tests + the jq test on every PR/push, and a kind-based
end-to-end job that brings up the stack via scripts/e2e.sh. The conversational
e2e runs when the CUBEPILOT_MODEL_PROVIDERS secret is configured; otherwise the
deploy path still runs with a placeholder provider.

Assisted-by: Claude Code"
```

---

### Task 3: README + spec-doc touch-ups

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-08-25-e2e-tests-and-ci-design.md` (fix the CRD group reference)

**Interfaces:**
- Consumes: the `scripts/e2e.sh` interface from Task 1 and the workflow from Task 2.

- [ ] **Step 1: Add an e2e/CI section to the README**

Insert after the `## Run` section (before `## What to verify`):

```markdown
## End-to-end tests & CI

`scripts/e2e.sh` brings up the whole stack on a kind cluster via
`scripts/setup.sh` and verifies it: cluster + namespace, the six
`assistant.suanova.io` CRDs, shared Secrets, `operator`/`api`/`web` rollouts,
the api `/healthz`, and the Portal HTML.

```bash
# Deploy path only (placeholder provider is fine):
CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-placeholder","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash"}]}}' \
  scripts/e2e.sh

# Full conversational e2e (needs a real provider key; drives POST /api/messages
# over SSE and cold-starts a per-user agent Pod):
CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-real","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash"}]}}' \
  CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh
```

CI (`.github/workflows/e2e.yaml`) runs these on every PR and push to `main`: a
fast `test` job (`go vet` + `go test` + `scripts/test-openclaw-config.sh`) and
an `e2e` job on kind. The conversational e2e runs when the GitHub secret
`CUBEPILOT_MODEL_PROVIDERS` (the `models.providers` object) is configured;
without it the deploy path still runs with a placeholder key.
```

- [ ] **Step 2: Fix the CRD group in the design spec**

In `docs/superpowers/specs/2026-08-25-e2e-tests-and-ci-design.md`, replace the two occurrences of `ai.cubestack.io` with `assistant.suanova.io` (R4 and §3, and the "6 CRDs" wording). Verify:

```bash
grep -rn "ai.cubestack.io" docs/superpowers/specs/2026-08-25-e2e-tests-and-ci-design.md || echo "fixed: no ai.cubestack.io"
```

- [ ] **Step 3: Commit**

```bash
git add README.md docs/superpowers/specs/2026-08-25-e2e-tests-and-ci-design.md
git commit -s -m "docs: document e2e + CI, fix CRD group in e2e design spec

Adds an End-to-end tests & CI section to the README and corrects the design
spec's CRD group from ai.cubestack.io to the real assistant.suanova.io.

Assisted-by: Claude Code"
```

---

### Task 4: Local verification

**Files:**
- Test-only task (no source changes unless a check fails).

**Interfaces:**
- Consumes: Tasks 1-3.

- [ ] **Step 1: Static checks**

```bash
bash -n scripts/e2e.sh
go vet ./...
go test ./...
bash scripts/test-openclaw-config.sh
```
Expected: all pass.

- [ ] **Step 2: Full deploy-path e2e on a local kind cluster**

Run (uses the already-cached images; ~5-10 min):
```bash
CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-placeholder","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"}]}}' \
  scripts/e2e.sh
```
Expected: `E2E PASS (deploy only)`; all assertions green. (The chat phase is
not run locally without a real key — it is exercised by CI when the secret is
configured.)

- [ ] **Step 3: Clean up the test cluster (leave the environment clean)**

```bash
kind delete cluster --name cube
```

- [ ] **Step 4: Commit any fixes**

If any check failed, fix inline and commit with `git commit -s`. If everything
passed, make no commit.

---

## Self-Review

**Spec coverage:**
- R1 deploy path → Task 1 (deploy phase) + Task 4 Step 2.
- R2 chat path → Task 1 (chat phase).
- R3 single invocation → Task 1 (one setup.sh call; chat uses the same cluster).
- R4 assertions → Task 1 Step 1 (cluster/ns, 6 CRDs, secrets, rollouts, healthz, HTML).
- R5 triggers → Task 2.
- R6 test + e2e jobs → Task 2.
- R7 secret gating → Task 2 Step 1 (CHAT_PROVIDER branch).
- §5 files touched → Tasks 1-3.
- §6 verification → Task 4.

**Placeholder scan:** all steps contain complete code; the "placeholder
provider" is a concrete literal. The workflow's kind/kubectl versions are
pinned (v0.32.0 / v1.36.1) and match the local env where they were verified.

**Type consistency:** CRD group `assistant.suanova.io` used identically in Task 1
and Task 3 Step 2; chat endpoint `/api/messages`, headers, and SSE event names
match `internal/server` (verified: `writeSSE` emits `event: <type>` +
`data: <json>`; event types `message_delta`/`message_done`).
