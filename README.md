# CubePilot

CubePilot is the intelligent assistant of the CubeStack platform. It implements
the two core capabilities described in the module design document (extension
points E1/E2, FR-M2/M3):

1. **Per-user agent instance lifecycle (K8s Pod)** -- the Instance Manager
   controller provisions / self-heals / optionally reclaims per-user OpenClaw
   Pods through the Kubernetes API. Sessions and memory survive instance
   rebuilds (each instance has its own PVC).
2. **Conversational loop** -- a real OpenClaw runtime (DeepSeek V4 Flash) acts
   under the guidance of the capability catalog (Skills), calls `exec` to run
   `kubectl` against the same cluster, and streams results back to the Portal
   over SSE.

## Architecture

```
browser (host) -- kubectl port-forward --- inside the kind cluster:
  cubepilot Deployment (assistant service + instance-manager controllers)
     ├--- per-user OpenClaw Pod  svc/agent-<user> (ClusterIP:18789)
     │         exec --- kubectl (in-cluster SA token) --- same kind cluster
     └--- K8s API (controller-runtime / client-go): Pod/PVC/Service lifecycle
```

- Per-user isolation = **Pod + dedicated PVC** (NFR-002); sessions persist on
  each PVC (FR-M2-004).
- Skill catalog = OpenClaw **Skills** (`internal/controller/skills/*/SKILL.md`,
  embedded and rendered by the supervisor) plus `workspace/SOUL.md` / `AGENTS.md`,
  baked into the agent image.
- Chat flows through OpenClaw's `/v1/chat/completions` (OpenAI-compatible,
  `stream:true`, `model: openclaw/default`); the gateway runs the full agent
  loop. Session lists/history go through `/tools/invoke` (`sessions_list`) and
  `GET /sessions/{key}/history`.
- Platform objects are Kubernetes CRDs (cluster-scoped, group
  `ai.cubestack.io`): `AgentTemplate`, `AgentInstance`, `Skill`,
  `TaskTemplate`, `Task`, `TaskRun` (`config/crd/bases`), reconciled by
  controller-runtime controllers.

## Directory layout

```
cmd/cubepilot-operator   platform controllers entry point
cmd/cubepilot-api         Portal + REST/SSE API entry point
internal/apiv1alpha1    CRD types (kubebuilder annotations)
internal/controller     AgentInstance + builtin-bootstrap controllers
internal/scheduler      CRD-driven task scheduler (Task -> TaskRun)
internal/instances      Instance Manager facade (CR-warm waits)
internal/config         env configuration
internal/k8s            client-go + Pod/PVC/Service builders
internal/openclaw       OpenClaw HTTP client + event mapping (with tests)
internal/server         REST/SSE handlers (no static serving)
internal/store          platform metadata store (JSON on PVC)
web/                    Portal SPA -- Vue 3 + TypeScript + Vite (independent
                        component; nginx serves it and proxies /api)
internal/controller/skills/   embedded skill catalog SKILL.md × 4
workspace/              SOUL.md / AGENTS.md
config/crd/bases        generated CRD manifests
deploy/                 images Dockerfiles + charts/cubepilot Helm chart + kubeconfig template
scripts/setup.sh        one-shot deployment (build -> kind -> helm install)
```

## Prerequisites

- Docker, [kind](https://kind.sigs.k8s.io/) (cluster name `cube`), `kubectl`,
  `jq`, `helm` (v3), and `openssl`.
- Model provider credentials supplied at setup time via `CUBEPILOT_MODEL_PROVIDERS`
  (the `models.providers` object). The gateway token is auto-generated.
  `scripts/setup.sh` reads no host config file (`~/.openclaw/...`) and needs no
  pre-built images or host Go toolchain, so it also runs on CI.

Images are registry-addressed:
- Base images resolve from `harbor.isuanova.com/library/...` (mirrored from the
  public registries by `scripts/mirror-base-images.sh`).
- Built images are
  `harbor.isuanova.com/suanova/cubepilot-{openclaw,operator,api,web}:<tag>`:
  `make images` builds them, `make push` pushes them, and
  `CUBEPILOT_PUSH=1 scripts/setup.sh` pushes after building.

### Releases (CI)

The `release` workflow (`.github/workflows/release.yaml`) builds and publishes
the four images plus the Helm chart (as an OCI artifact) into
`harbor.isuanova.com`:

| Trigger | Images | Helm chart (OCI) |
|---|---|---|
| push to `main` | `.../cubepilot-{openclaw,operator,api,web}:latest` | `oci://harbor.isuanova.com/suanova/cubepilot:<chartVersion>-latest` (rolling dev artifact) |
| tag `vX.Y.Z` | `...:X.Y.Z` | `oci://harbor.isuanova.com/suanova/cubepilot:X.Y.Z` |
| `workflow_dispatch` (input `tag`) | `...:<tag>` | `oci://harbor.isuanova.com/suanova/cubepilot:<tag>` |

The chart's default image tags are `:latest` (tracking main builds); pin a
specific release with `--set operator.image=...,api.image=...,web.image=...,agents.image=...`
or your own values file. Install a published chart with:

```bash
helm install cubepilot oci://harbor.isuanova.com/suanova/cubepilot --version 0.1.0
```

The workflow needs the GitHub variable `CI_BOT_NAME` (Harbor username) and
secret `CI_BOT_PASSWORD` (Harbor password/token), ideally a Harbor bot account
with push rights on the `suanova` project.
The Harbor `suanova` project must allow tag overwrites so the rolling
`latest` / `-latest` artifacts can be re-pushed on every main push.

## Run

```bash
# 1. Build images, create the kind cluster if needed, create Secrets, deploy
CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-...","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"}]}}' \
  scripts/setup.sh

# 2. Expose the Portal
kubectl -n cubepilot port-forward svc/cubepilot 8080:8080

# 3. Open http://127.0.0.1:8080
```

The provider key is arbitrary — it only prefixes the gateway's model ref. The
agent's default model is the first provider's first model (or
`CUBEPILOT_DEFAULT_MODEL`); to use a specific model, add it to the
`AgentTemplate` `models` and set `AgentInstance.selectedModel`.

Full input reference: `scripts/setup.sh --help` (or the env vars in
`docs/superpowers/specs/2026-08-25-setup-zero-local-deps-design.md` §3).

Deployment is Helm-managed (`deploy/charts/cubepilot`): the operator, api,
web and per-component RBAC are chart templates; platform CRDs ship in the
chart's `crds/` dir (installed at `helm install` -- upgrade them by reapplying
the manifests in `deploy/charts/cubepilot/crds/`). Secrets (`openclaw-config`, `agent-kubeconfig`) are created out-of-band by
`scripts/setup.sh` because they hold LLM credentials supplied at setup time.

### Adding or changing providers after install (no setup.sh needed)

The gateway config (LLM providers, model allowlist, gateway token) lives in
the `openclaw-config` Secret's `openclaw.json`. `scripts/setup.sh` only writes
that Secret at first install; it is not involved afterwards. Production
installations (plain `helm install`) create the Secret out-of-band, and later
changes edit it directly — the chart does not own it.

To add a provider to a running install:

```bash
# 1. Export the current config, edit it (add your provider under
#    .models.providers), then write it back -- keep gatewayToken untouched.
kubectl -n cubepilot get secret openclaw-config -o jsonpath='{.data.openclaw\.json}' | base64 -d > openclaw.json
#    ... edit openclaw.json ...
kubectl -n cubepilot create secret generic openclaw-config \
  --from-file=openclaw.json=openclaw.json \
  --from-literal=gatewayToken="$(kubectl -n cubepilot get secret openclaw-config -o jsonpath='{.data.gatewayToken}' | base64 -d)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

2. The agent supervisors watch the mounted `openclaw.json` and gracefully
   restart their gateway when it changes, so the new provider applies without
   touching Pods (sessions/PVC survive).

3. To make a model selectable, add it to the `AgentTemplate` `models` list —
   its `modelId` prefix must equal the provider key (the gateway allowlist is
   keyed `<provider>/<model-id>`) — and set `AgentInstance.selectedModel` to
   the model's `name`. Model selection is configuration (API/kubectl), not a
   chat-time picker in phase one.

The first message cold-starts the `agent-zhang.wei` Pod (the Portal shows the
assistant as thinking while it waits for the gateway to become ready), then
streams tool calls and the answer back.

## End-to-end tests & CI

`scripts/e2e.sh` brings up the whole stack on a kind cluster via
`scripts/setup.sh` and verifies it: cluster + namespace, the six
`ai.cubestack.io` CRDs, shared Secrets, `operator`/`api`/`web` rollouts,
the api `/healthz`, and the Portal HTML.

```bash
# Deploy path only (placeholder provider is fine):
CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-placeholder","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"}]}}' \
  scripts/e2e.sh

# Full conversational e2e (needs a real provider key; drives POST /api/messages
# over SSE and cold-starts a per-user agent Pod):
CUBEPILOT_MODEL_PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-real","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"}]}}' \
  CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh
```

Any provider key works (the e2e uses `deepseek`); the agent's default model is
the first provider's first model, so the gateway allowlist always covers it.

CI (`.github/workflows/e2e.yaml`) runs these on every PR and push to `main`: a
fast `test` job (`go vet` + `go test` + `scripts/test-openclaw-config.sh`) and
an `e2e` job on kind. The conversational e2e runs when the GitHub secret
`CUBEPILOT_MODEL_PROVIDERS` (the `models.providers` object) is configured;
without it the deploy path still runs with a placeholder key. Publishing the
images/chart to the registry is handled separately by the `release` workflow —
see [Releases (CI)](#releases-ci).

## What to verify

| Check | Action | Expected |
|---|---|---|
| Conversational loop | Ask "which Pods are abnormal?" | SSE emits `message_start -> agent_thinking -> tool_call(exec kubectl) -> message_delta -> message_done`, ending with a natural-language summary of real kind Pod state |
| Cold start | First message | `kubectl -n cubepilot get pods` shows `agent-zhang.wei` |
| Resident self-heal / memory | Delete the Pod manually, send a message | The controller rebuilds the Pod; session and memory persist (PVC) |
| User isolation | Request with `X-CubePilot-User: li.ming` | Separate Pod/PVC per user |
| Inspection | Portal -> scheduled tasks -> run now | Severity-graded node/Pod report (`/api/inspect`) |

## Current simplifications (phase-one boundaries)

- The assistant service and the instance-manager controllers run in one
  process; the production shape separates them (design doc §9).
- `agent-*` Pods share one ServiceAccount with a broad ClusterRole (production
  target: per-user minimal RBAC, FR-M3-001).
- The capability catalog is baked into the image (production target: ConfigMap
  mounting for "takes effect immediately", FR-M2-005).
- Not yet implemented: HITL confirmation (`confirm_*`), audit DB (M5), RAG,
  and multi-tenant auth -- scheduled for phases two/three.
- `tool_result` events do not appear separately in the stream (OpenClaw's agent
  loop executes tools server-side); tool outcomes are reflected in the final
  answer text, and the full tool blocks are available in session history.

## Local development (without the cluster)

```bash
# Run on the host with ~/.kube/config against kind (the IM still creates Pods)
CUBEPILOT_LISTEN=:8080 CUBEPILOT_GATEWAY_TOKEN=<token> go run ./cmd/cubepilot-api
# Note: the host cannot reach ClusterIPs directly; port-forward each agent Pod
# individually. In-cluster deployment is recommended.
```

## Test

```bash
go test ./...
```
