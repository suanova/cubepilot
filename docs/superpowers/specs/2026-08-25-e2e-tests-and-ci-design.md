# Design: e2e tests + GitHub Actions CI

Date: 2026-08-25
Status: approved (user: "ok, 实施吧")
Goal owner: zhujian

## 1. Context & goal

The zero-host-dependency refactor made `scripts/setup.sh` CI-runnable. The next
step is proving it: end-to-end tests that bring up the whole stack on a fresh
kind cluster and verify it, plus a GitHub Actions workflow that runs them
automatically on every PR and push to main. Chat e2e runs automatically too
(the user chose "全都自动跑"), using a real model-provider key stored as a CI
secret.

## 2. Requirements

- R1. A single e2e script, `scripts/e2e.sh`, brings up the stack via
  `scripts/setup.sh` on a kind cluster and asserts the deploy path. It runs with
  only a placeholder provider key (no real credentials needed).
- R2. The same script runs the conversational e2e when `CUBEPILOT_E2E_CHAT=1`
  and a real provider key is supplied. Chat asserts an SSE stream with
  `message_delta` and a non-error `message_done`, and that the per-user agent
  Pod cold-starts into Running.
- R3. Deploy and chat run in a SINGLE invocation (one `setup.sh`), because the
  agent config is mounted into Pods via subPath (`internal/k8s/resources.go`) and
  does not hot-update — a second `setup.sh` with a different key would not take
  effect on already-created agent Pods.
- R4. Deploy-phase assertions: kind cluster exists; namespace exists; the six
  chart CRDs exist (`agenttemplates`, `agentinstances`, `capabilities`, `tasks`,
  `taskruns`, `tasktemplates` under `ai.cubestack.io`); secrets
  `openclaw-config` (keys `openclaw.json` + non-empty `gatewayToken`) and
  `agent-kubeconfig` exist; `cubepilot-operator`/`cubepilot-api`/`cubepilot-web`
  rollouts Ready; api `/healthz` returns 200; the portal (`svc/cubepilot`)
  serves HTML.
- R5. `.github/workflows/e2e.yaml` triggers on push to main, pull_request, and
  workflow_dispatch.
- R6. CI has a fast `test` job (Go vet + `go test ./...` +
  `scripts/test-openclaw-config.sh`) and an `e2e` job that installs
  kind/kubectl/helm/jq, pre-pulls base images, and runs `scripts/e2e.sh`.
- R7. The e2e job uses the real provider key from
  `secrets.CUBEPILOT_MODEL_PROVIDERS` (with `CUBEPILOT_E2E_CHAT=1`) when the
  secret is present; otherwise it falls back to a hardcoded placeholder provider
  and runs deploy-only. No workflow failure when the secret is absent (forks).

### Non-goals

- No chat assertion of specific tool-call sequences or content beyond "a reply
  streamed and completed without error" (deep behavior is covered by Go unit
  tests + the README's manual verification table).
- No image caching / registry setup (separate optimization).
- No scheduled (cron) trigger (user chose PR+push, always-on).

## 3. `scripts/e2e.sh` contract

Invocation: `CUBEPILOT_MODEL_PROVIDERS=<json> [CUBEPILOT_E2E_CHAT=1] scripts/e2e.sh`

Inputs (env, mirroring setup.sh where shared):
- `CUBEPILOT_MODEL_PROVIDERS` (required) — the `models.providers` object. For
  deploy-only a placeholder value is fine; for chat it must be a real key.
- `CUBEPILOT_E2E_CHAT` (default `0`) — set to `1` to run the chat phase.
- `CUBEPILOT_E2E_USER` (default `zhang.wei`) — which operator identity the chat
  phase talks as (must be in the chart's `agents.users`).
- `CUBEPILOT_NAMESPACE` / `CUBEPILOT_KIND_CLUSTER` — defaults `cubepilot`/`cube`
  (shared with setup.sh).

Exit: `0` on full pass, non-zero on first failed assertion (clear `E2E FAIL`
message). A skipped chat phase (not `CUBEPILOT_E2E_CHAT=1`) still exits `0`.

Deploy-phase assertions are those in R4. Chat phase:
1. `kubectl port-forward svc/cubepilot 8080:8080` (background, killed on EXIT).
2. `POST /api/messages` with `{"session_id": "e2e-<ts>", "content": "<probe>"}`
   and `X-CubePilot-User: $CUBEPILOT_E2E_USER`, `curl -sN --max-time 300`
   (covers cold start + gateway boot + LLM turn).
3. Assert SSE contains `event: message_delta` and `event: message_done`;
   parse the `message_done` data JSON and assert `.error` is empty.
4. Assert `kubectl get pods` shows an `agent-$USER` Pod Running.

## 4. `.github/workflows/e2e.yaml` contract

- Triggers: `push: { branches: [main] }`, `pull_request: {}`,
  `workflow_dispatch: {}`.
- Job `test` (ubuntu-latest): actions/checkout; setup-go 1.26; `go vet ./...`;
  `go test ./...`; `bash scripts/test-openclaw-config.sh`.
- Job `e2e` (ubuntu-latest, `needs: test`):
  1. checkout.
  2. Install kind, kubectl, helm, jq (download pinned binaries; jq via apt).
  3. Pre-pull base images (`golang:1.26-bookworm`, `node:24-bookworm-slim`,
     `ghcr.io/openclaw/openclaw:2026.6.33`) to de-risk BuildKit metadata pulls.
  4. One step that runs the e2e exactly once:
     - `CHAT_PROVIDER: ${{ secrets.CUBEPILOT_MODEL_PROVIDERS }}` (empty when
       unset).
     - if `CHAT_PROVIDER` non-empty → run
       `CUBEPILOT_MODEL_PROVIDERS="$CHAT_PROVIDER" CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh`;
       else → run `CUBEPILOT_MODEL_PROVIDERS='<placeholder>' scripts/e2e.sh`.
- Document the required secret in the workflow comments: `CUBEPILOT_MODEL_PROVIDERS`
  = the `models.providers` object with a real API key.

## 5. Files touched

- Create: `scripts/e2e.sh`
- Create: `.github/workflows/e2e.yaml`
- Modify: `README.md` — document the e2e script + CI (short section).

## 6. Verification

- Local: `CUBEPILOT_MODEL_PROVIDERS='<placeholder>' scripts/e2e.sh` brings up a
  kind cluster and passes all deploy assertions (images cached from the prior
  work). Chat phase requires a real key — verified only on CI (or a dev with a
  key), and skipped locally.
- CI: `test` job green; `e2e` job green deploy-only without the secret; green
  full (deploy+chat) once `CUBEPILOT_MODEL_PROVIDERS` is configured as a secret.
