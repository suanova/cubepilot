# Design: Declarative model providers (issue #6) + portal LLM management

Date: 2026-08-27
Status: approved (user: "ok, continue"; schema reviewed with user)
Goal owner: zhujian

## 1. Context & goal

Issue suanova/cubepilot#6: the `openclaw-config` Secret (the gateway's
`openclaw.json`) is today a one-shot, install-time artifact rendered by
`scripts/setup.sh` from the `CUBEPILOT_MODEL_PROVIDERS` env var. This makes
provider configuration imperative and non-declarative:

- Adding a provider post-install means hand-editing a Secret.
- The provider JSON and the platform's model catalog (`AgentTemplate.models`)
  are two independent sources of truth that must agree by convention: the
  gateway allowlist is keyed `<provider-key>/<model-id>` from the JSON, while
  the API's `x-openclaw-model` override comes from the template's model
  identity. When the key doesn't match, chat fails with
  `Model '<ref>' is not allowed for agent 'main'`.

Goal (per design [`docs/cubepilot/cubepilot-design.md`](../../cubepilot/cubepilot-design.md) §3.3):
a CubePilot component generates the gateway config from declarative state.
Adding an LLM = adding a model entry (+ a credential Secret when it is not a
public model); nothing else is hand-maintained. In the real environment the
platform admin adds an LLM from the Portal (no setup-time env vars). The
project has not shipped yet, so there is **no migration/upgrade path** in
scope.

## 2. Current state

- `TemplateModelSpec` (`internal/api/v1alpha1/agenttemplate_types.go`) carries
  `endpoint` + `credentialRef`, but no code reads them (dead fields). It also
  has a `provider` enum (Platform | External) and a `modelId` field, neither of
  which has a rendering effect; `credentialRef` is a bare string and `modelId`
  is off the k8s initialism convention.
- The builtin template (`internal/controller/builtin.go`) hard-codes
  `ModelID: "cuberouter/deepseek-v4-flash-0731"` (a full gateway ref) which
  must match the deployer's `models.providers` allowlist key by convention; the
  README/e2e examples use provider key `deepseek`, so selecting the builtin
  default model breaks. It also ships a placeholder External model
  (`qwen2.5-72b` at `api.example.com`, credential `cred-llm-org`).
- `scripts/setup.sh` requires `CUBEPILOT_MODEL_PROVIDERS`, renders
  `deploy/openclaw-config.jq` into the `openclaw-config` Secret (gatewayToken +
  openclaw.json). The chart injects `CUBEPILOT_GATEWAY_TOKEN` from that Secret
  into operator/api pods; the agent supervisor reads the Secret directly.
- The resolver (`internal/resolver/resolver.go`) only sends an
  `x-openclaw-model` override for an explicitly selected model.

## 3. Scope

1. Simplify `TemplateModelSpec`: drop `provider` and `modelId`; make `endpoint`
   required for every model; make `credentialRef` optional (a public model has
   none) and a structured `LocalObjectReference`. The model `name` is the model
   id sent to the endpoint. Update the design doc §3.3 accordingly.
2. Builtin template carries a working model (endpoint + credentialRef); drop
   the placeholder External entry.
3. `internal/gateway` renderer: template models + credential Secrets ->
   `openclaw.json` (replaces `deploy/openclaw-config.jq`).
4. Gateway token owned by CubePilot: generated once, persisted in the Secret.
5. `OpenClawConfigReconciler`: render + reconcile the `openclaw-config` Secret.
6. Portal: add an LLM (API + web) -- writes a model entry (+ credential Secret
   when keyed); the operator picks it up.
7. Retire `CUBEPILOT_MODEL_PROVIDERS` from setup.sh / e2e / CI.

Non-goals: migration/upgrade (pre-release); per-user providers (phase two,
user-created templates); non-OpenAI-compatible API styles (`api` is always
`openai-completions` in phase one); multi-template provider-key collision
policy beyond last-write-wins; a display-name/backend-name split (phase two
catalog aliasing, e.g. a `displayName`).

## 4. Part A -- Model schema + builtin template

### Schema (`internal/api/v1alpha1/agenttemplate_types.go`)

```go
type TemplateModelSpec struct {
    Name          string                      `json:"name"`                 // catalog name = selection key = provider key = backend model id
    Endpoint      string                      `json:"endpoint"`             // required; OpenAI-compatible base URL
    CredentialRef corev1.LocalObjectReference `json:"credentialRef,omitempty"` // optional; public models omit it
}
```

- `provider` (Platform | External) and `modelId` are **removed**: platform-
  managed and external models are both concrete OpenAI-compatible endpoints,
  and `name` is the single model identity -- what the user selects
  (`selectedModel`), the gateway provider key, and the model id sent to the
  endpoint. No display-name/backend-name split in phase one (add `displayName`
  in phase two if catalog aliasing is needed).
- `endpoint` is required for every model (no `+optional`, no `omitempty`);
  enforced by `Validate()` and a CEL `XValidation`.
- `credentialRef` is optional; a present one must carry `name`. Public models
  omit it entirely.

Validation rules:

```
self.all(m, has(m.endpoint))                                    // every model needs an endpoint
self.all(m, !has(m.credentialRef) || has(m.credentialRef.name)) // if present, it is a real ref
```

### Builtin template (`internal/controller/builtin.go`)

```go
{ Name:          "deepseek-v4-flash",
  Endpoint:      "https://api.deepseek.com",                    // default platform endpoint; admin may edit the CR
  CredentialRef: corev1.LocalObjectReference{Name: "cubepilot-llm"},  // credential Secret from setup.sh
}
```

The placeholder `qwen2.5-72b` External entry is **removed**: with declarative
generation a dangling credential leaves a selectable-but-broken model. The
portal "add LLM" flow is what creates additional models.

`builtin_test.go` (TestBuiltinAgentShape) updates: expect 1 model with endpoint
+ credentialRef.

## 5. Part B -- internal/gateway renderer (new package)

Pure function; unit-tested. Replaces `deploy/openclaw-config.jq`.

```go
type Provider struct {
    Key     string  // provider key = model Name
    BaseURL string  // endpoint
    APIKey  string  // optional; from the credentialRef Secret when present
    Model   string  // model name = backend id (emitted as models[].id and models[].name)
}
func Render(token, primary string, providers []Provider) ([]byte, error)
```

Mapping (single template in phase one):

- One provider per template model with a non-empty `endpoint`.
- provider key = model `Name`; the provider's single model entry is
  `{id: Name, name: Name}`; `api: "openai-completions"`, `baseUrl = endpoint`.
  `apiKey` is set only when the model has a `credentialRef` whose Secret
  resolves; a public model renders a keyless provider (OpenClaw supports
  endpoints without auth, equivalent to the current placeholder key).
- Allowlist ref = `<name>/<name>`.
- `primary` is computed by the caller (the reconciler) and passed in: the ref
  of the template's `defaultModel` when that model renders a provider,
  otherwise the first provider's first model. `Render` does not derive it.
- Static scaffolding (workspace, sandbox, gateway port/auth, tools, sessions)
  copied from the current jq defaults.

Why provider key = model name is sufficient: the renderer produces the
provider entry **and** the allowlist **and** the primary from the same template
model, so the config is always internally consistent -- the key is just a
stable label. No new `providerKey` field (YAGNI); revisit if multi-template
name collisions arise (phase two).

**Override ref (kills the coupling)**: the `x-openclaw-model` override the
resolver sends for an explicit selection must be a full ref, because the
allowlist is keyed `<provider-key>/<model-id>`. The resolver composes
`<name>/<name>` -- the same string the renderer puts in the allowlist -- so the
two can never drift. (This replaces the old convention of stuffing a full ref
into `modelId`.) Existing resolver tests that used full-ref `ModelID` fixtures
(e.g. `cuberouter/...`) are updated to assert the composed `<name>/<name>`
override.

Credential Secret schema: `apiKey` literal key. `credentialRef` is a structured
`LocalObjectReference`; its `name` resolves in the operator/API namespace
(`cfg.Namespace`).

## 6. Part C -- Gateway token owned by CubePilot

`EnsureGatewayToken(ctx, cl, ns) (string, error)` in `internal/gateway`:

1. Read the `openclaw-config` Secret's `gatewayToken`; non-empty -> return it
   (never regenerate an existing token).
2. Else generate `crypto/rand` 32-byte hex and write it into the Secret
   (create if missing; on `AlreadyExists` re-read the winner's token).

Called at startup by operator `main()` and api `main()` before building the
runner/server, so `cfg.GatewayToken` is set in-process. The reconciler keeps
the `gatewayToken` key untouched when writing `openclaw.json`. The chart's
`CUBEPILOT_GATEWAY_TOKEN` env injection is removed; setup.sh no longer manages
the token. The agent supervisor is unchanged (reads the Secret at Pod create).

## 7. Part D -- OpenClawConfigReconciler

New reconciler in `internal/controller`, wired into operator `main()`.

Reconcile:

1. List AgentTemplates.
2. Collect models with a non-empty `endpoint`. For each with a `credentialRef`,
   read the credential Secret -> `apiKey`; missing Secret: skip that model +
   log + requeue (never block other providers). Models without `credentialRef`
   render keyless.
3. Render `openclaw.json` (Part B).
4. Reconcile the `openclaw-config` Secret: create if missing (with
   `EnsureGatewayToken`), always preserve `gatewayToken`, set/update
   `openclaw.json`.

Watch: `For(&v1alpha1.AgentTemplate{})` + `Watches(&corev1.Secret{})` (credential
changes re-render), plus a periodic requeue as a safety net.

RBAC (kubebuilder markers): `ai.cubestack.io agenttemplates get;list;watch`;
`"" secrets get;list;watch;create;update;patch`.

## 8. Part E -- API: add an LLM

New `POST /api/llms` in `internal/server` (body `{name, endpoint, apiKey?}`):

1. Validate: `name` required, sanitized to DNS-1123 (`k8s.Sanitize`), and the
   sanitized name must be unique among the builtin template's models; `endpoint`
   a valid URL. Return the effective (sanitized) name.
2. Only when `apiKey` is present: create credential Secret `<ns>/llm-<slug>`
   with `apiKey` (a public model skips this and the model carries no
   `credentialRef`).
3. Append a model to the builtin `agent-for-cloud` AgentTemplate:
   `{Name, Endpoint, CredentialRef: LocalObjectReference{Name: "llm-<slug>"}
   (only when keyed)}`; Update the CR.
4. Return the created model. The operator reconciles it into the gateway on
   the next watch tick.

API RBAC additions (chart): `"" secrets create`; `ai.cubestack.io
agenttemplates get;update`. Existing endpoints already expose the resulting
model via the AgentTemplate list.

## 9. Part F -- Web: LLM management

A minimal "LLM 配置" panel (new web view or section) that:

- Lists the builtin template's current models.
- "添加 LLM" form: name / endpoint / apiKey (optional, for public models) ->
  `POST /api/llms` -> refresh.

After adding, the new model appears in the Agent model picker and chat can
select it. Keep the UI minimal (frontend still early).

## 10. Part G -- setup.sh / e2e / CI / chart

`scripts/setup.sh`:

- Remove `CUBEPILOT_MODEL_PROVIDERS` / `--providers-json` input, required
  check, validation, jq render block, and the openclaw-config Secret creation +
  gateway-token reuse/generate logic.
- Add a required `CUBEPILOT_LLM_APIKEY` / `--llm-apikey`: creates the
  `cubepilot-llm` Secret (`apiKey`) that the builtin model references. CI
  feeds it from the GitHub secret so an install gets one working LLM out of
  the box.
- Keep `agent-kubeconfig` creation.

Remove `deploy/openclaw-config.jq` and `scripts/test-openclaw-config.sh`
(superseded by the Part B unit tests); update `Makefile` and the CI `test` job
accordingly. `e2e.sh` / `.github/workflows/e2e.yaml` switch the GitHub secret to
`CUBEPILOT_LLM_APIKEY`; the e2e openclaw-config check waits for the operator to
generate the Secret (it no longer exists at helm-install time).

Helm chart: remove `CUBEPILOT_GATEWAY_TOKEN` from `_helpers.tpl`; add secrets
RBAC for operator and api (Parts D/E).

## 11. Part H -- Docs

- README: rewrite the provider section to the declarative flow (setup
  `--llm-apikey` + Portal "add LLM"); remove the hand-edit-Secret walkthrough.
- `scripts/setup.sh --help`: replace the providers-json block with
  `--llm-apikey`.
- `docs/cubepilot/api.md`: add `/api/llms`; update the AgentTemplate sample
  (single endpoint model).
- `docs/cubepilot/cubepilot-design.md` §3.3: rewrite the model section --
  no provider enum / no modelId, `name` is the model id, `endpoint` required,
  `credentialRef` optional (public models).
- `docs/cubepilot/implementation-status.md`: note `CUBEPILOT_MODEL_PROVIDERS`
  retired and the schema/builtin changes.

## 12. Testing

- `internal/gateway`: renderer unit tests (model->provider mapping, primary,
  allowlist, keyless providers, empty providers); `EnsureGatewayToken`
  (generate once / reuse / preserve / AlreadyExists).
- `internal/controller`: reconciler fake-client test (create Secret, preserve
  token, re-render on template change, skip missing credential, keyless model).
- `internal/api/v1alpha1`: `Validate()` / CEL rule tests (endpoint required,
  credentialRef optional + name when present).
- `internal/resolver`: update to the new schema; assert the composed
  `<name>/<name>` override for an explicit selection.
- `internal/server`: `POST /api/llms` happy path (keyed + public) + validation
  + idempotency.
- `go build ./... && go test ./...`; kind e2e (`scripts/e2e.sh`) with
  `--llm-apikey` from the GitHub secret.

## 13. Decisions resolved

- Provider-key derivation: model `Name` (no new field).
- `provider` and `modelId` dropped; `name` is the model id; `endpoint` required
  for every model; `credentialRef` optional (`corev1.LocalObjectReference`).
- Builtin default credentials: builtin model references `cubepilot-llm`
  (created by setup.sh from `--llm-apikey`).
- Gateway token: generated once by CubePilot, persisted in the Secret.
- Migration: none (pre-release).
