# LLM providers with multiple models per endpoint (issue #189) -- design

Date: 2026-09-15 · Status: approved for implementation · Scope: issue #189

## Context

The only model concept in the CRs is the inline list
`AgentTemplate.spec.models[]` ([agenttemplate_types.go:39-55](../../../internal/api/v1alpha1/agenttemplate_types.go)).
Each entry is `{name, endpoint, credentialRef?}`, and `name` carries four
identities at once: the catalog name, the `selectedModel` key, the OpenClaw
gateway provider key, and the backend model id sent to the endpoint. There is no
provider object.

"One endpoint and one key serving N models" is therefore expressible only by
writing N entries that repeat the endpoint string, point at the same credential,
and create N Secrets -- `llmCredentialName` is `"llm-" + modelName`
([handlers_llms.go:271-275](../../../internal/server/handlers_llms.go)).

Self-hosted and aggregator gateways (vLLM, SGLang, LiteLLM, ollama, OpenRouter)
serve many models behind a single base URL and a single key, which makes this
shape actively wrong for them. Two failures follow:

1. **Duplication.** The endpoint is copied per model and one API key is copied
   into N Secrets. Nothing in the CR records that those models share a provider.
2. **An unusable Secret name.** `name` doubles as a k8s resource-name suffix, so
   a model id containing `/` -- OpenRouter's `anthropic/claude-sonnet-4.5` --
   produces the Secret name `llm-anthropic/claude-sonnet-4.5`. That is not a
   valid Secret name and the API server rejects it. The `EnvNameForModel`
   credential key is unaffected (it sanitizes and appends a short hash,
   [client.go:162-190](../../../internal/k8s/client.go)); only the resource-name
   path is broken.

`name` being overloaded is also what makes a model un-renamable today: a rename
is a delete plus an add, because the same string is the Secret name and the
gateway provider key ([handlers_llms.go:21-24](../../../internal/server/handlers_llms.go)).

The coupling is purely ours. See the next section.

## Verified facts (OpenClaw 2026.8.2, source-grounded)

`deploy/openclaw-image.Dockerfile` pins `OPENCLAW_IMAGE_TAG=2026.8.2`, and the
`openclaw` checkout read here is `v2026.8.2` (commit `0965053`), so these are the
semantics of the deployed runtime.

- **A provider already declares a list of models.** `models` is
  `z.array(ModelDefinitionSchema)` and `id` is `z.string().min(1)` with no
  charset or slash restriction (`src/config/zod-schema.core.ts`). A custom
  provider must declare both `baseUrl` and `models`.
- **Model refs split on the first `/` only.** `parseModelRef` takes
  `trimmed.indexOf("/")` and keeps the whole remainder as the model id
  (`src/agents/model-selection-normalize.ts`). Multi-slash ids are first-class
  and documented: `openrouter/moonshotai/kimi-k2` (`docs/cli/models.md`).
- **The canonical key is `modelKey(provider, model)`** (`src/shared/model-key.ts`).
  It returns the model id alone when that id already starts with `<provider>/`,
  and `"<provider>/<model>"` otherwise. Any component that builds a ref must
  apply this rule or it will not match the key OpenClaw computes.
- **The allowlist is `agents.defaults.modelPolicy.allow`**, a
  `z.array(z.string())` (`src/config/zod-schema.agent-runtime.ts`). The keys of
  `agents.defaults.models` act as an allowlist **only** through the legacy path,
  which stops applying once `meta.migrations.modelPolicyAllowlist` is stamped
  (`src/config/model-policy-allowlist-migration.ts`). Resolution normalizes both
  the allowlist entries and the requested ref through the same
  `normalizeStaticProviderModelId`, so the two agree.
- **Built-in providers rewrite model ids.** For an exact provider key of
  `google` / `google-gemini-cli` / `google-vertex`, `openrouter`, `anthropic`,
  `vercel-ai-gateway`, `huggingface`, `nvidia`, `xai`, `together` or `openai`,
  `normalizeBuiltInProviderModelId` transforms the model id
  (`packages/model-catalog-core/src/provider-model-id-normalization.ts`). Any
  other provider name returns the id unchanged. Most are alias or prefix
  rewrites; `nvidia` and `huggingface` are unconditional. `openai` is a no-op.

Consequence: the CR layer is the only place that forces a one-to-one
relationship.

## Design

### 1. CRD: `spec.providers[]`

`TemplateModelSpec` is replaced by:

```go
// TemplateProviderSpec is one OpenAI-compatible LLM provider of an
// AgentTemplate: an endpoint plus the credential it is reached with, and the
// backend model ids available through it.
type TemplateProviderSpec struct {
	// Name is the provider key: the OpenClaw models.providers key, the prefix
	// of every model ref (<name>/<modelId>), and the suffix of the credential
	// Secret name llm-<name>. DNS-1123 label.
	//
	// A name that exactly matches an OpenClaw built-in provider key inherits
	// that provider's model-id normalization, which can rewrite the id sent to
	// the endpoint. Use a distinct name (e.g. nvidia-proxy) unless that is
	// intended.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Endpoint is the OpenAI-compatible base URL.
	Endpoint string `json:"endpoint"`

	// CredentialRef optionally references a platform-managed Secret (name)
	// holding the apiKey; providers that need no credentials omit it (nil).
	// References only -- never the key itself.
	// +optional
	CredentialRef *corev1.LocalObjectReference `json:"credentialRef,omitempty"`

	// Models lists the backend model ids reachable through this provider. Each
	// id is sent to the endpoint verbatim and may itself contain "/" (e.g.
	// OpenRouter's "anthropic/claude-sonnet-4.5").
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Models []string `json:"models"`
}
```

`AgentTemplateSpec.Models` becomes `Providers []TemplateProviderSpec` with
`+listType=map` and `+listMapKey=name`; `DefaultModel` keeps its field name but
its value is now a full ref, `<provider>/<modelId>`.

Validation is split so that each rule lands where it is cheapest to enforce:

| Rule | Mechanism |
|---|---|
| provider name charset / length | `Pattern` + `MaxLength` (OpenAPI schema) |
| provider name unique | `+listMapKey=name` |
| model id unique within a provider | `+listType=set` |
| `models` non-empty | `MinItems=1` |
| `credentialRef`, when present, carries a Secret name | CEL `XValidation` (moved from the model list) |
| model id has no whitespace, no leading/trailing `/`, no `//`, is not `*` | CEL `XValidation` |
| `defaultModel`, when non-empty, names an existing `<provider>/<modelId>` | CEL `XValidation` (guarded by `has()` on both fields, which are `omitempty`) |
| endpoint is a valid URL | CEL, plus the existing `normalizeEndpoint` in the handler |

Of the two CEL rules on `Models` at [agenttemplate_types.go:172-173](../../../internal/api/v1alpha1/agenttemplate_types.go),
one retires and one moves. "Every model requires an endpoint" retires: an
endpoint now exists once per provider, so it cannot be omitted per model.
"credentialRef must carry a name" moves onto `Providers`, where the field now
lives. `TemplateProviderSpec.Validate()` mirrors the CEL rules in Go for the
controller and handlers, as `TemplateModelSpec.Validate()` does today.

The provider name is restricted to a DNS-1123 label because it is simultaneously
the ref prefix and the Secret-name suffix. The ref grammar splits on the first
`/` with no escaping, so a `/` in a provider name would make refs unparseable;
that restriction also makes `llm-<name>` a legal Secret name, keeps the name
compatible with OpenClaw's lowercase provider normalization, and excludes `*`,
whitespace and control characters. Model ids get no such restriction -- they are
data sent to the endpoint.

### 2. Render and resolver

`gateway.Provider` ([render.go:13-24](../../../internal/gateway/render.go)) gains
a list:

```go
type Provider struct {
	Key     string   // provider name = models.providers key
	BaseURL string   // endpoint
	APIKey  string   // credential key name (k8s.EnvNameForProvider); "" = public
	Models  []string // backend model ids, sent to the endpoint verbatim
}
```

`Render` expands `models` per provider instead of emitting a single-element
array, and generates one allowlist entry per id.

**The one piece of OpenClaw logic we mirror.** `modelKey`'s self-prefix rule must
exist in Go, as `gateway.ModelKey(provider, modelID string) string`, because both
the renderer and the resolver build and compare allowlist keys with it. Without
it, provider `vllm` with id `vllm/qwen3-32b` would be rendered as
`vllm/vllm/qwen3-32b` and never match. The function carries a comment naming
`src/shared/model-key.ts` as the thing it tracks. (Rejecting ids that start with
`<provider>/` would avoid the mirror, but OpenRouter ships a model literally
named `openrouter/auto`, so that rejection would be wrong.)

**The allowlist is made explicit.** `Render` additionally emits
`agents.defaults.modelPolicy.allow` with every ref. The existing
`agents.defaults.models` map is kept, since it is what carries the per-model
`alias`. That alias is only written when the bare model id is unique across the
template: with providers, the same id can appear under two of them, and an alias
collision resolves to an arbitrary winner. Omitting the alias in that case keeps
the ambiguity out of the config while `modelPolicy.allow` still lists both refs.
Nothing in cubepilot resolves a model by alias -- `selectedModel`,
`defaultModel` and the turn-time model are all full refs -- so the alias is a
convenience for OpenClaw's own surfaces only.

Everything downstream follows the same grain:

| Concern | Today | After |
|---|---|---|
| `k8s.EnvNameForModel` | takes a model name | renamed `EnvNameForProvider`, takes a provider name (algorithm unchanged) |
| credential Secret | `llm-<model>` | `llm-<provider>`, one per provider |
| operator loop ([openclawconfig_controller.go:45-70](../../../internal/controller/openclawconfig_controller.go)) | per model, checks the model's Secret | per provider, checks the provider's Secret; a missing Secret skips the whole provider |
| `ResolvedCredential` ([resolver.go:86-91](../../../internal/resolver/resolver.go)) | per model | per provider |
| `resolveModel` ([resolver.go:275-287](../../../internal/resolver/resolver.go)) | returns `<name>/<name>` | returns `<provider>/<modelId>`; fails closed when the provider is absent or the id is not in its `models` |
| `defaultModel` fallback | `DefaultModel`, else the first model | `DefaultModel`, else the first id of the first provider |
| `ModelConfigured` | a model is available | at least one provider's credential is ready |

The supervisor is untouched: `syncCredentials`
([supervisor.go:273-328](../../../internal/supervisor/supervisor.go)) reads the
`apiKey` key out of a Secret and writes `keys.json`; the credential's granularity
is invisible to it.

`AgentInstance.CredentialSpec.ModelRef` and `.Endpoint`
([agentinstance_types.go:32-40](../../../internal/api/v1alpha1/agentinstance_types.go))
are declared and documented as the model-to-credential binding but are read
nowhere -- `CredentialFor` matches `Target` only
([agentinstance_types.go:220-229](../../../internal/api/v1alpha1/agentinstance_types.go)).
They are removed in this change rather than left to imply a mechanism that does
not exist.

### 3. API and credential lifecycle

- `POST /api/llms` becomes "add a provider": name, endpoint, apiKey/public, and
  the `models` list.
- `PUT /api/llms/{name}` accepts a full replacement of `models`, alongside the
  existing rule that an empty `apiKey` keeps the stored credential.
- `DELETE /api/llms/{name}` refuses with 409 while any instance still selects any
  id of that provider; the `instances` array in the body keeps its shape
  ([handlers_llms.go:339-407](../../../internal/server/handlers_llms.go)).
- Removing one model id leaves the credential alone; removing the provider
  removes `llm-<provider>`. `removeModelCredential`
  ([handlers_llms.go:277-299](../../../internal/server/handlers_llms.go)) is
  written against one-Secret-per-model and must be rewritten, or deleting a
  single model takes the shared key with it.

### 4. Web

The LLM card in [AgentView.tsx:407-458](../../../web/src/views/AgentView.tsx)
becomes a provider card with a nested model list:

```
┌─ vllm ───────────────────────────────── [keyed] ─┐
│  http://vllm.ai.svc.cluster.local:8000/v1        │
│  ─────────────────────────────────────────────── │
│   qwen3-32b                                 [×]  │
│   deepseek-v4-flash                         [×]  │
│   + Add model                                     │
│                        [Edit]  [Remove provider]  │
└──────────────────────────────────────────────────┘
```

- Adding a model is a one-field action on an existing card. It is the most
  frequent operation and must not require walking the whole provider form.
- Removing a model and removing a provider are separate controls with different
  consequences (Secret kept vs. Secret deleted). They must be visually distinct;
  today there is a single "remove" affordance.
- The `selectedModel` select groups options by provider and still submits a full
  ref; empty still means Runtime Default.

## Risks and deliberately unhandled

- **Built-in provider names.** A provider named `nvidia`, `anthropic`, `xai` or
  similar inherits that provider's model-id normalization, and the rewrite can
  make the endpoint reject a model it never heard of (`nvidia` prefixes any bare
  id). Not blocked at validation time: the failure surfaces through the existing
  turn-error path, and a name blacklist would move in the wrong direction as
  OpenClaw adds built-ins. Documented in the field comment instead. If this turns
  out to be hard to diagnose in practice, a non-blocking warning is the next
  step.
- **Alias ambiguity.** Handled by omitting the alias when a bare id is not
  unique; see section 2.
- **Dynamic model discovery** (`GET /v1/models` against the provider, so an
  admin multi-selects instead of typing ids) is orthogonal and not part of this
  change.

## Verification

`gateway.Render` and `gateway.ModelKey` are pure and table-drive cleanly: ref
construction, the self-prefix rule, `APIKey == ""` rendering `cubepilot-no-auth`
([render.go:26-39](../../../internal/gateway/render.go)), the alias uniqueness
rule, and the `modelPolicy.allow` contents. The resolver's fail-closed branches
get one case each: provider absent, id not in the provider's list, credential
Secret missing.
