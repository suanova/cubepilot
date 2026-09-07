# Helm deploy defaults: self-contained chart + default `admin` operator identity (issue #117)

Date: 2026-09-07
Status: Proposed

## Summary

Two deploy-time cleanups that make a fresh manual `helm install` produce a
working stack with no extra steps:

1. **Chart self-contained**: render the static `agent-kubeconfig` Secret from
   the chart instead of creating it out-of-band in `scripts/setup.sh`.
2. **Single `admin` default identity**: align the default operator identity
   across the chart, the backend config defaults, the web Portal fallback,
   and the e2e mirror so a fresh install runs as one `admin` user out of the
   box.

Chart/deploy/docs only; no controller or gateway runtime behavior change
beyond the default identity value.

## Part 1 -- Chart renders the `agent-kubeconfig` Secret

### Background

CubePilot's deployment is Helm-managed: the chart ships all workloads plus the
six `ai.cubestack.io` CRDs. One piece is still created out-of-band by
`scripts/setup.sh`:

```bash
kubectl -n "$NAMESPACE" create secret generic agent-kubeconfig \
  --from-file=config="$REPO_DIR/deploy/agent-kubeconfig.yaml" \
  --dry-run=client -o yaml | kubectl apply -f -
```

The `agent-kubeconfig` Secret is a **static, non-sensitive in-cluster
kubeconfig** (`server: https://kubernetes.default.svc` + the agent Pod's own
mounted ServiceAccount token/CA). It is content-identical on every cluster.

The AgentInstance controller hard-requires it before provisioning any agent
Pod: it does a plain `Get` on `k8s.KubeconfigSecretName` (`agent-kubeconfig`,
defined in `internal/k8s/client.go`) and returns an error on any failure
(`internal/controller/agentinstance_controller.go`), so the reconcile requeues
and the Pod is never created while the Secret is missing. A purely manual
`helm install` therefore has an easy-to-forget prerequisite, and forgetting it
silently leaves every AgentInstance stuck.

### Why the chart can render it

- **Static content.** The kubeconfig references the in-cluster API server and
  the Pod's own projected ServiceAccount, so the bytes do not vary per
  cluster and can be authored once in a template.
- **Fixed name + type.** The operator reads it as a `corev1.Secret` named
  `agent-kubeconfig`. Rendering a `Secret` with that exact name needs no code
  change. It must stay a `Secret` (not a `ConfigMap`) to avoid changing the
  controller's `Get`.

### Decisions

- **Always create (no toggle).** The kubeconfig is static boilerplate that is
  correct on every cluster; a toggle would add surface for a case that does
  not exist today. If an operator ever needs discovery against a *different*
  cluster, the Secret can be re-authored then (the controller recreates the
  Pod on a Secret content/resourceVersion change).
- **Chart dir is the single source of truth.** The content is inlined in the
  new template and the root `deploy/agent-kubeconfig.yaml` is deleted. An
  OCI-packaged chart cannot read files outside its own directory via `.Files`,
  so keeping a root file would require duplicating the content (drift risk).

## Part 2 -- Default operator identity = `admin`

### Background / why empty is not an option

The default demo identities are `zhang.wei,li.ming`. Today those defaults live
in several places:

- chart: `deploy/charts/cubepilot/values.yaml` `agents.users: zhang.wei,li.ming`
- backend: `internal/config/config.go` `CUBEPILOT_USERS` / `CUBEPILOT_DEFAULT_USER`
- web Portal fallback: `web/src/api/client.ts` `getCurrentUser()` -> `'zhang.wei'`
- e2e mirror: `test/e2e/framework/framework.go` (mirrors `internal/config`)
- `scripts/e2e.sh` chat identity: `E2E_USER` default `zhang.wei`

Two facts frame the change:

1. **An empty `users` value is not a valid/running state.** The chart always
   sets the `CUBEPILOT_USERS` env from `agents.users`, and `config.Load()`'s
   `getenv` treats an empty env value as unset and falls back to the built-in
   default pair. Even if users were truly empty, the operator bootstraps zero
   AgentInstance CRs (and mints no per-user identity), so no agent can ever
   become Warm and chat times out. Users are a deploy-time concept.
2. **New users cannot be added from the Portal.** The Agent Config page
   configures an existing instance (model / skills / system prompt); the
   self-service `POST /api/instances` provisions the caller's own instance CR,
   but a working Pod requires the per-user kubeconfig Secret, which the
   builtin bootstrap mints **only** for `CUBEPILOT_USERS` members. A user
   outside that list gets an instance that waits forever for its identity
   (issue #100). Adding/removing users is done by changing `agents.users`
   (or `CUBEPILOT_USERS`) and re-deploying.

So "users default" is really "the default operator identity". Making it a
single `admin` that the Portal operates as out of the box requires aligning
every fallback that names the identity.

### Decisions

- **Single `admin` default, aligned in three runtime places** (chart, web
  fallback, backend config defaults) **plus the e2e mirror and script**. A
  partial change (chart only) would leave the Portal operating as `zhang.wei`
  with no provisioned instance for that identity, i.e. a broken fresh install.
- The localStorage override in the web client (`cubepilot.user`) is preserved;
  only the fallback default changes.
- Historical docs under `docs/cubepilot/` and `docs/superpowers/` are left
  untouched (they describe earlier states).

## Changes by file

### Part 1 (agent-kubeconfig Secret)

- `deploy/charts/cubepilot/templates/agent-kubeconfig.yaml` (new): a
  `kind: Secret` named `agent-kubeconfig` in the release namespace, with
  `stringData.config` set to the existing in-cluster kubeconfig content.
- `scripts/setup.sh`: drop the `agent-kubeconfig` creation block and its log
  line; keep creating the `cubepilot-llm` Secret. Adjust the surrounding log
  text.
- `deploy/agent-kubeconfig.yaml`: deleted (content moved into the chart).
- `internal/k8s/userkubeconfig.go`: update the comment that references
  `deploy/agent-kubeconfig.yaml` to point at the chart template.
- `deploy/charts/cubepilot/templates/NOTES.txt`: update the note that says the
  `agent-kubeconfig` Secret is created by `scripts/setup.sh`.

### Part 2 (`admin` default identity)

- `deploy/charts/cubepilot/values.yaml`: `agents.users: admin` (comment
  updated).
- `internal/config/config.go`: `CUBEPILOT_USERS` default ->
  `"admin"`; `CUBEPILOT_DEFAULT_USER` default -> `"admin"`; update comments.
- `web/src/api/client.ts`: `getCurrentUser()` fallback `'zhang.wei'` ->
  `'admin'`.
- `test/e2e/framework/framework.go`: mirror env defaults ->
  `"admin"` (keep the "mirror internal/config.Load() defaults" comment).
- `scripts/e2e.sh`: `E2E_USER` default `zhang.wei` -> `admin`.
- `deploy/charts/cubepilot/values.yaml`: clean up the dead
  `secrets.openclawConfig` / `secrets.agentKubeconfig` name keys (no template
  consumes them; the real names are fixed in Go) and document how each Secret
  is provisioned: `openclaw-config` is operator-generated,
  `agent-kubeconfig` is chart-rendered, and LLM apiKeys live in user-managed
  Secrets (`cubepilot-llm` seeded by setup, plus `llm-<name>` created by the
  Portal).
- `README.md`: update the default-identity examples (cold start
  `agent-zhang.wei` -> `agent-admin`; reword the user-isolation example to
  show adding a second user, since the default is now a single `admin`).

## Part 3 (addendum) -- Surface "no usable LLM" in AgentInstance status

Scope added during review (same PR). Provisioning behaviour is unchanged
(the agent is still created/run without an LLM); what changes is that the
platform states it clearly, and the Portal points the user at Agent Config.

- Lifecycle and model availability stay decoupled (decision A): a Ready pod is
  `phase: Ready` regardless of models.
- The AgentInstance controller adds a `ModelConfigured` status condition:
  - `True` when the instance's AgentTemplate offers a usable model (non-empty
    endpoint and, for keyed models, an existing credential Secret);
  - `False` (reason `NoModelConfigured`, message pointing at
    `Portal Agent Config -> LLM Config`) otherwise.
- The controller watches AgentTemplates and Secrets (mapping to every
  AgentInstance) so the condition flips as soon as a model or its credential
  appears/disappears.
- Portal (ChatView): while the caller's instance is not Ready or has
  `ModelConfigured=False`, a dismissable nudge shows a "Go to Agent Config"
  link; it disappears automatically once the model is added / the instance
  becomes Ready (5s poll of `GET /api/instances`).
- No CRD schema change (status conditions already exist); controller + unit
  tests cover the condition transitions; the web change is type-checked.

## Migration

- Existing setups created by `scripts/setup.sh` already have the
  `agent-kubeconfig` Secret with identical data; `helm upgrade` applies the
  new manifest and adopts the Secret (adds Helm labels) without triggering
  agent Pod recreation.
- Existing deployments keep whatever `agents.users` they were installed with
  (the value is read from the release, not the new default), so no existing
  instance is torn down by the default change.
- Pre-release, no compatibility branches are needed.

## Testing

- `make lint` (runs `helm lint` + `helm template`) verifies the chart renders
  (Secret present, values valid).
- `helm template <release> deploy/charts/cubepilot -n cubepilot` output
  contains the `agent-kubeconfig` Secret with the expected `config` data and
  `CUBEPILOT_USERS=admin`.
- `go test ./...` (defaults change is exercised by existing unit tests).
- `npm run build` in `web/` (client.ts change type-checks).
- Existing e2e (deploy-only) is unaffected: it installs via
  `scripts/setup.sh` with the new defaults and the e2e mirror defaults now
  match `admin`. Running it locally is optional for this change.
