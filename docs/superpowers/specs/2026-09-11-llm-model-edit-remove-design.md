# LLM model editing, removal, endpoint normalization, and public models (issue #170) -- design

Date: 2026-09-11 · Status: approved for implementation · Scope: issue #170

## Context

A model added through the Portal cannot be changed or removed. `handleAddLLM`
is POST-only and the route is a single `mux.HandleFunc("/api/llms", ...)`
([server.go:171](../../../internal/server/server.go)); a duplicate name returns 409
([handlers_llms.go:60-65](../../../internal/server/handlers_llms.go)). So a typo'd
endpoint or a key that has to be rotated is unrecoverable through the product --
fixing it required `kubectl patch agenttemplate` against the CRD by hand.

Two failures motivate this:

1. **A full request URL pasted as the endpoint.** The endpoint is written
   verbatim into the provider's `baseUrl` ([render.go:35](../../../internal/gateway/render.go)),
   and OpenClaw's `openai-completions` provider hands that to the official
   OpenAI SDK, whose `baseURL` is the API *root* -- the SDK appends
   `/chat/completions`. `http://host/v1/chat/completions` therefore becomes
   `.../v1/chat/completions/chat/completions` and 404s.
2. **A public model (no apiKey) never works unless it is on a local address.**
   Investigated in this design; see "Verified facts" below.

## Verified facts (OpenClaw 2026.8.2, source-grounded)

`deploy/openclaw-image.Dockerfile` pins `OPENCLAW_IMAGE_TAG=2026.8.2`, and the
`openclaw` checkout read here is `2026.8.2`, so these are the semantics of the
deployed runtime, not of some other version.

- A provider with no resolvable credential fails **every** turn with
  `No API key resolved for provider "<x>" (auth mode: ..., checked: ...)`
  (`src/agents/model-auth-runtime-shared.ts`). It is not a lazy per-request
  failure; the turn never reaches the endpoint.
- OpenClaw synthesizes a non-secret no-auth placeholder (`custom-local`) for a
  custom provider with **no** explicit `apiKey` -- but only when the base URL is
  local. Both producers of that placeholder
  (`hasSyntheticLocalProviderAuthConfig`, `resolveUsableCustomProviderApiKey` in
  `src/agents/model-auth-provider-config.ts`) end at the same gate:
  `isLocalProviderBaseUrl()`, which accepts `localhost`, `0.0.0.0`, `*.local`,
  `host.docker.internal` / `docker.orb.internal` / `host.orb.internal`, the
  `ipaddr.js` `loopback` range, and the IPv4 `private` (RFC 1918) range. A
  public internet address, a cluster service DNS name, and a CGNAT address are
  **not** local.
- There is no configuration lever that removes the `Authorization` header for a
  non-local provider. `authHeader: false` is the default and means "let the SDK
  inject it"; `authHeader: true` **adds** a canonical one
  (`applyAuthHeaderOverride`). The only code that clears it
  (`applyLocalNoAuthHeaderOverride`) requires the `custom-local` placeholder,
  i.e. a local base URL.
- A **literal** (non-marker) `apiKey` on a custom provider config is a usable
  credential: `resolveProviderEntryApiKeyProfileReference` returns
  `{kind:"literal", apiKey: <value>, source:"models.json"}`. The OpenAI SDK then
  constructs and the request goes out as `Authorization: Bearer <value>`.

Conclusion: a keyless model at a non-local endpoint can never work as
configured, and the only way to make one work is to give OpenClaw a value to
resolve. `render.go` omitting `apiKey` entirely for a model without a
`credentialRef` is what produced the dead-but-saveable model.

## Decisions

| Question | Decision |
|---|---|
| Delete a model an instance selects | **Refuse** (409), naming the instances |
| Rename | **Not supported**; the name is immutable in the edit form |
| Endpoint that is a full request URL | **Strip** the trailing `/chat/completions` |
| Deleting the current primary | **Clear** `defaultModel`, renderer falls back to the first model |
| Deleting the last model | **Allowed** |
| Keyless model on a remote endpoint | **Require an explicit `public: true` declaration** on save |
| Renderer for a credential-less model | **Always** emit a placeholder `apiKey`, local or not |

## API

All three handlers act on the builtin template (`v1alpha1.DefaultAgentName`),
matching `handleAddLLM`. `{name}` is the sanitized model name.

| Route | Semantics |
|---|---|
| `POST /api/llms` | same route; gains `public`; endpoint normalized |
| `PUT /api/llms/{name}` | edit endpoint, apiKey, and public flag |
| `DELETE /api/llms/{name}` | remove the model and its credential Secret |

Registered as `/api/llms` plus `/api/llms/{name}` with `r.PathValue("name")`
and an in-handler method check, mirroring `/api/skills/{name}/publish`.

### The credential rule

A request may only leave a model **without** a credential if it says so
explicitly with `"public": true`. Otherwise a credential is required.

- `POST`: `apiKey` empty and `public` not true -> **400**. This is the guard
  that stops the silently-dead model from being created.
- `PUT`: `apiKey` empty and `public` not true -> the **existing** credential is
  kept. That is the "fix my endpoint typo" path: the client cannot echo a key it
  never received. If the model has no credential to keep -> **400**, the same
  guard as POST.
- `public: true` together with a non-empty `apiKey` -> **400** (contradictory).

So: a keyed model never needs a declaration, and a public model always does.

### PUT

Body `{endpoint, apiKey, public}`. No `name` field -- a rename is delete + add.

- 404 when the model is not in the builtin template.
- `endpoint` required; validated and normalized (see below).
- `apiKey` non-empty -> upsert the Secret and point `credentialRef` at
  `llm-<name>`. This also covers promoting a public model to keyed.
- `apiKey` empty + `public: true` -> clear `credentialRef` and **delete** the
  Secret if one exists; this demotes a keyed model to public.
- Ordering: template first, then the Secret -- the same rationale as
  `handleAddLLM` (the template is authoritative for the model's existence; a
  failed Secret write leaves a harmless orphan rather than a model the operator
  skips).
- 200 `{"model": <stored entry>}`, with the normalized endpoint, so the UI shows
  the correction it actually saved.

### DELETE

- 404 when the model is not in the builtin template.
- **409** when any `AgentInstance` has `spec.templateRef == "cubepilot"` and
  `spec.selectedModel == name`. `SelectedModelFor` is fail-closed, so removing
  the model would make that user's turns fail; the response names the instances
  (`{"error": ..., "instances": [{"name", "owner"}]}`). `templateRef` defaults to
  the builtin at creation and is never empty, so the check is exact.
- Otherwise: remove the entry -> clear `spec.defaultModel` if it named this
  model -> delete the credential Secret (NotFound is success). The renderer
  already falls back to the first remaining provider, so a cleared
  `defaultModel` has a defined outcome.
- 200 `{"removed": name}`. A failed Secret delete returns 200 with a `warning`
  string: the model is gone, so reporting an error would be a lie, but the
  orphan is worth surfacing.

### Endpoint normalization

`normalizeEndpoint(raw string) (string, bool)`, shared by add and edit:

1. `TrimSpace`.
2. Drop a trailing `/`.
3. Drop a trailing `/chat/completions` (case-insensitive).
4. `url.Parse`; require scheme and host.

Only that one suffix is stripped, and a missing `/v1` is never added -- a root
endpoint is correct for some providers (the repo's own default
`https://api.deepseek.com` is one). Done at the API write boundary only, not in
`gateway.Render`: rewriting at render time would leave the CR disagreeing with
what the user sees.

## Renderer

`gateway.Render` emits a placeholder `apiKey` for a provider with no credential:

```go
// A model with no credential (a "public" model) still needs a non-empty
// apiKey: OpenClaw fails every turn with "No API key resolved" for a provider
// it cannot resolve a credential for, and the OpenAI SDK will not construct a
// client without one. OpenClaw synthesizes its own no-auth placeholder, but
// only for local base URLs; emitting ours unconditionally keeps one rule
// ("every rendered provider has a resolvable apiKey") instead of mirroring
// OpenClaw's locality check here. The value is never a real secret, and an
// endpoint that needs no authentication ignores the header it produces.
pv["apiKey"] = PublicModelAPIKey // "cubepilot-no-auth"
```

The placeholder must not collide with any OpenClaw marker
(`custom-local`, `ollama-local`, `secretref-managed`, ...) -- those are
recognized and routed down paths that would resolve nothing again.

Residual behavior change to accept: a local no-auth server now receives
`Authorization: Bearer cubepilot-no-auth` where previously it received no
`Authorization` header at all. Ollama, vLLM and llama.cpp ignore an unexpected
`Authorization` header.

## Web

`web/src/views/AgentView.tsx`:

- Model rows gain `Edit` and `Remove`. `Remove` uses a `confirm()` like
  `TasksView.tsx:152`; a 409 renders the server's instance list in the toast.
- `Edit` reuses the card's form, pre-filled with the endpoint, with the name
  rendered read-only. The title switches between Add and Edit.
- A **Public endpoint -- no API key required** checkbox; when checked the
  apiKey input is disabled.
- Endpoint field hint on both paths: *OpenAI-compatible base URL (API root) --
  do not include `/chat/completions`*.
- `web/src/api/index.ts` gains `updateLLM(name, {endpoint, apiKey?, public?})`
  and `deleteLLM(name)`.

## Tests

`internal/server/handlers_llms_test.go` (existing fake-client style):

- add: normalizes `.../v1/chat/completions`; keyless without the flag -> 400
  with nothing written; `public: true` -> no Secret.
- update: endpoint-only keeps the key; a new key rotates the Secret; public ->
  keyed and keyed -> public; a remote keyless update without the flag -> 400;
  unknown model -> 404; normalizes.
- delete: removes the model and its Secret; a public model (no Secret) deletes
  cleanly; unknown -> 404; a selected model -> 409 naming the instance;
  clearing `defaultModel`; the last model.

`internal/gateway/render_test.go`: a credential-less provider carries the
placeholder; a keyed provider is unchanged.

No front-end test framework exists, so the web change is verified by
`npm run build` (`tsc -b`).

## Docs

`docs/cubepilot/api.md`: extend the 添加 LLM section with the `public` rule and
normalization, and add 编辑 LLM / 删除 LLM sections.

## Out of scope

- Models in non-builtin AgentTemplates. `addLLM` already writes only to the
  builtin, and phase one ships exactly one template; the UI reads the first
  template from the list, which is the builtin in practice.
- Migrating endpoints already stored with a trailing `/chat/completions`.
  Normalization applies on write.
