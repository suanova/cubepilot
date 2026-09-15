# LLM Providers with Multiple Models per Endpoint -- Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `AgentTemplate.spec.models[]` -- a flat list whose every entry carries its own endpoint and credential -- with `spec.providers[]`, where each provider owns one endpoint, one credential and the list of backend model ids it serves.

**Architecture:** The runtime already models this one-to-many (OpenClaw's `models.providers.<key>.models` is an array, and a model ref splits on the first `/`), so the CR layer is the only place forcing one-to-one. The change therefore moves in four layers, each keeping the tree green: the pure `gateway` renderer first (it can render N models from one provider while the CRD still supplies one), then the CRD types and every reader of them, then the write API, then the web card.

**Tech Stack:** Go 1.26 (controller-runtime, kubebuilder markers + controller-gen v0.19.0), Helm chart CRDs, React 19 + TypeScript 7 web (Vite), stdlib `testing` for Go unit tests, Ginkgo for the cluster-backed e2e suite.

**Spec:** [docs/superpowers/specs/2026-09-15-llm-provider-models-design.md](../specs/2026-09-15-llm-provider-models-design.md) (issue #189)

## Global Constraints

- Go 1.26. Every `go` command below runs from the repo root.
- **English + ASCII punctuation** in all code, comments, docs and UI strings (AGENTS.md). Use `--` not an em-dash; `§` is permitted for design cross-references.
- **User-facing strings must never contain issue/PR numbers or requirement ids** (AGENTS.md). `#189` belongs in source comments and commit messages only.
- Which OpenClaw facts the change relies on, and their source, are recorded in the spec's "Verified facts" section. Do not restate them from memory.
- `make test` = `go vet ./...` + `go test $(go list ./... | grep -v /test/)`. It does not build the web app or run e2e.
- `make web` = `cd web && npm run build` (runs `tsc -b`, so it type-checks as well as bundles).
- Every commit needs DCO sign-off (`git commit -s`) and, when AI-assisted, the `Assisted-by: Claude Code` trailer.
- golangci-lint v2.13.2 runs in CI with `errcheck, govet, ineffassign, misspell, staticcheck, unused` plus `gofmt`/`goimports` formatting. Do not leave an unchecked error.
- **There is no `make manifests`.** controller-gen is invoked by hand (exact commands in Task 2, Step 1).
- `config/crd/bases/*.yaml` and `deploy/charts/cubepilot/crds/*.yaml` are byte-identical copies today and must stay identical.

## Deliberate scope cuts

The spec's "Risks and deliberately unhandled" section is binding on this plan:

- A provider named after an OpenClaw built-in (`nvidia`, `anthropic`, ...) is **not** rejected. It is documented in the field comment, and the failure surfaces through the existing turn-error path.
- Dynamic model discovery (`GET /v1/models`) is out of scope.
- No back-compat shims: the project is pre-release, so old `spec.models` objects and old `selectedModel` strings are simply not supported.

## Task ordering note

Tasks 1 and 2 are the same logical change split for reviewability, and neither leaves the web app usable: after Task 2 the frontend still reads `spec.models`, which no longer exists, so the LLM card is empty until Task 4. The Go tree and `make test` are green after every task. Do not try to shorten this by landing Task 2's type change without Task 1 -- the renderer's `Provider` struct is what the controller passes to it.

---

### Task 1: Render many models from one provider

**Files:**
- Create: `internal/gateway/modelkey.go`
- Create: `internal/gateway/modelkey_test.go`
- Modify: `internal/gateway/render.go` (the `Provider` struct at :13-24 and `Render` at :44-67)
- Modify: `internal/gateway/render_test.go`
- Modify: `internal/controller/openclawconfig_controller.go:41-70`
- Modify: `internal/controller/openclawconfig_controller_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `gateway.ModelKey(provider, modelID string) string`; `gateway.Provider{Key, BaseURL, APIKey string; Models []string}`; `gateway.Render(token, primary string, providers []Provider) ([]byte, error)` unchanged in signature but now emitting every id of every provider, an `agents.defaults.modelPolicy.allow` array, and a per-model `alias` only when that id is unique across the template.

- [ ] **Step 1: Write the failing test for the key rule**

Create `internal/gateway/modelkey_test.go`:

```go
package gateway

import "testing"

// TestModelKey pins the ref shape OpenClaw computes, because the renderer's
// allowlist keys are compared against it by exact string match. The self-prefix
// case is the one plain concatenation gets wrong: an id that already names its
// provider must not be prefixed a second time.
func TestModelKey(t *testing.T) {
	cases := []struct {
		provider string
		modelID  string
		want     string
	}{
		{"vllm", "qwen3-32b", "vllm/qwen3-32b"},
		{"vllm", "vllm/qwen3-32b", "vllm/qwen3-32b"},
		{"openrouter", "anthropic/claude-sonnet-4.5", "openrouter/anthropic/claude-sonnet-4.5"},
		{"openrouter", "openrouter/auto", "openrouter/auto"},
		{"vllm", "", "vllm"},
		{"", "qwen3-32b", "qwen3-32b"},
	}
	for _, tc := range cases {
		if got := ModelKey(tc.provider, tc.modelID); got != tc.want {
			t.Errorf("ModelKey(%q, %q) = %q, want %q", tc.provider, tc.modelID, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/gateway/ -run TestModelKey`
Expected: FAIL -- `undefined: ModelKey`.

- [ ] **Step 3: Implement ModelKey**

Create `internal/gateway/modelkey.go`:

```go
package gateway

import "strings"

// ModelKey builds the canonical provider/model key for a model reference. It
// mirrors modelKey in OpenClaw's src/shared/model-key.ts: an id that already
// starts with "<provider>/" is its own key, and anything else is prefixed.
//
// The renderer writes this string as the key of the agents.defaults.models
// entry and into agents.defaults.modelPolicy.allow, and OpenClaw computes the
// same key from the requested ref and compares them by exact string equality,
// so the rule cannot be approximated with plain concatenation: provider "vllm"
// with id "vllm/qwen3-32b" must be "vllm/qwen3-32b", not "vllm/vllm/qwen3-32b".
func ModelKey(provider, modelID string) string {
	providerID := strings.TrimSpace(provider)
	model := strings.TrimSpace(modelID)
	if providerID == "" {
		return model
	}
	if model == "" {
		return providerID
	}
	if strings.HasPrefix(strings.ToLower(model), strings.ToLower(providerID)+"/") {
		return model
	}
	return providerID + "/" + model
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/gateway/ -run TestModelKey`
Expected: PASS.

- [ ] **Step 5: Write the failing render tests**

In `internal/gateway/render_test.go`, add these three tests. The first replaces the single-model assumption; the second covers the aliasing rule; the third covers the ref that carries its own provider prefix.

```go
// TestRenderProviderWithMultipleModels covers one provider -- one endpoint and
// one credential -- serving several model ids, which is what a self-hosted
// gateway or an aggregator looks like. The provider body's models array is what
// OpenClaw registers, and modelPolicy.allow is what authorizes selection of
// each id.
func TestRenderProviderWithMultipleModels(t *testing.T) {
	b, err := Render("tok", "vllm/qwen3-32b", []Provider{
		{Key: "vllm", BaseURL: "http://vllm.ai.svc:8000/v1", APIKey: "CUBEPILOT_LLM_VLLM", Models: []string{"qwen3-32b", "deepseek-v4-flash"}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Models struct {
			Providers map[string]struct {
				Models []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
		Agents struct {
			Defaults struct {
				Models      map[string]any `json:"models"`
				ModelPolicy struct {
					Allow []string `json:"allow"`
				} `json:"modelPolicy"`
			} `json:"defaults"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pv := cfg.Models.Providers["vllm"]
	if len(pv.Models) != 2 || pv.Models[0].ID != "qwen3-32b" || pv.Models[1].ID != "deepseek-v4-flash" {
		t.Errorf("provider models = %+v, want both ids", pv.Models)
	}
	want := []string{"vllm/deepseek-v4-flash", "vllm/qwen3-32b"} // sorted
	got := cfg.Agents.Defaults.ModelPolicy.Allow
	if len(got) != len(want) {
		t.Fatalf("modelPolicy.allow = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("modelPolicy.allow[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, ref := range want {
		if _, ok := cfg.Agents.Defaults.Models[ref]; !ok {
			t.Errorf("agents.defaults.models missing %q", ref)
		}
	}
}

// TestRenderDuplicateModelIDOmitsAlias: the same bare id can be served by two
// providers, and the alias index resolves an alias to exactly one ref with no
// defined winner. Omitting the alias keeps the ambiguity out of the config;
// the refs stay selectable through modelPolicy.allow either way.
func TestRenderDuplicateModelIDOmitsAlias(t *testing.T) {
	b, err := Render("tok", "a/qwen3-32b", []Provider{
		{Key: "a", BaseURL: "https://a", Models: []string{"qwen3-32b"}},
		{Key: "b", BaseURL: "https://b", Models: []string{"qwen3-32b", "solo"}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Agents struct {
			Defaults struct {
				Models map[string]map[string]any `json:"models"`
			} `json:"defaults"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, ref := range []string{"a/qwen3-32b", "b/qwen3-32b"} {
		entry, ok := cfg.Agents.Defaults.Models[ref]
		if !ok {
			t.Fatalf("agents.defaults.models missing %q", ref)
		}
		if _, dup := entry["alias"]; dup {
			t.Errorf("%q should carry no alias while the id is ambiguous: %+v", ref, entry)
		}
	}
	if got := cfg.Agents.Defaults.Models["b/solo"]["alias"]; got != "solo" {
		t.Errorf("unambiguous id alias = %v, want solo", got)
	}
}

// TestRenderIDCarryingItsOwnProviderPrefix pins the self-prefix rule end to
// end: the allowlist key must be the id itself, and the primary ref must match
// it, or the selection would be rejected as not-allowed.
func TestRenderIDCarryingItsOwnProviderPrefix(t *testing.T) {
	b, err := Render("tok", "openrouter/auto", []Provider{
		{Key: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "CUBEPILOT_LLM_OPENROUTER", Models: []string{"openrouter/auto"}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Agents struct {
			Defaults struct {
				Models      map[string]any `json:"models"`
				ModelPolicy struct {
					Allow []string `json:"allow"`
				} `json:"modelPolicy"`
			} `json:"defaults"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := cfg.Agents.Defaults.Models["openrouter/auto"]; !ok {
		t.Errorf("allowlist should hold the id as-is: %+v", cfg.Agents.Defaults.Models)
	}
	if len(cfg.Agents.Defaults.ModelPolicy.Allow) != 1 || cfg.Agents.Defaults.ModelPolicy.Allow[0] != "openrouter/auto" {
		t.Errorf("modelPolicy.allow = %v, want [openrouter/auto]", cfg.Agents.Defaults.ModelPolicy.Allow)
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/gateway/ -run 'TestRenderProviderWithMultipleModels|TestRenderDuplicateModelIDOmitsAlias|TestRenderIDCarryingItsOwnProviderPrefix'`
Expected: FAIL to compile -- `unknown field Models in struct literal` (the old `Provider` has `Model string`).

- [ ] **Step 7: Implement the Provider struct and Render**

In `internal/gateway/render.go`, replace the `Model` field of `Provider`:

```go
// Provider is one OpenClaw models.providers entry derived from a template
// provider: an endpoint, the credential it is reached with, and the backend
// model ids available through it.
type Provider struct {
	Key     string // provider name = models.providers key
	BaseURL string // endpoint
	// APIKey is the credential key name (k8s.EnvNameForProvider) rendered as a
	// file SecretRef ({source:"file", provider:cubepilot-keys, id:"/<name>"})
	// into the emptyDir JSON file the supervisor writes. The literal key never
	// lands in the config file, the PVC, or the network response. Empty for
	// providers that need no credential: those render PublicModelAPIKey
	// instead.
	APIKey string
	// Models are the backend model ids served through this endpoint, sent
	// verbatim. An id may contain "/" (OpenRouter's
	// "anthropic/claude-sonnet-4.5").
	Models []string
}
```

and replace the provider loop of `Render` (currently lines 45-67) with:

```go
	// A bare model id is usable as an alias only while it is unique across the
	// template. Two providers serving the same id would both claim it and the
	// alias index resolves an alias to one ref with no defined winner, so an
	// ambiguous id gets its entry without an alias. The ref stays selectable
	// through modelPolicy.allow either way.
	idCount := map[string]int{}
	for _, p := range providers {
		for _, id := range p.Models {
			idCount[id]++
		}
	}
	allowOut := []string{}
	for _, p := range providers {
		modelEntries := make([]any, 0, len(p.Models))
		for _, id := range p.Models {
			modelEntries = append(modelEntries, map[string]any{"id": id, "name": id})
			key := ModelKey(p.Key, id)
			entry := map[string]any{}
			if idCount[id] == 1 {
				entry["alias"] = id
			}
			modelsOut[key] = entry
			allowOut = append(allowOut, key)
		}
		pv := map[string]any{
			"api":     "openai-completions",
			"baseUrl": p.BaseURL,
			"models":  modelEntries,
		}
		if p.APIKey != "" {
			// File SecretRef into the supervisor-written keys.json; OpenClaw
			// reads the file per-resolution, so a new model's key works without
			// a restart (the config hot-reloads; the supervisor wrote the key).
			pv["apiKey"] = map[string]any{
				"source":   "file",
				"provider": k8s.CredProviderName,
				"id":       "/" + p.APIKey,
			}
		} else {
			pv["apiKey"] = PublicModelAPIKey
		}
		providersOut[p.Key] = pv
	}
	// Sorted for byte-stable output (TestRenderDeterministic); the map above
	// marshals sorted by key already, but this array does not.
	sort.Strings(allowOut)
```

Then, inside the `cfg` literal, keep the existing `"model": map[string]any{"primary": primary}` line and add a sibling:

```go
				"model":       map[string]any{"primary": primary},
				"models":      modelsOut,
				// The allowlist is stated explicitly rather than left to the
				// legacy reading of the agents.defaults.models keys, which stops
				// applying once OpenClaw stamps meta.migrations.modelPolicyAllowlist.
				"modelPolicy": map[string]any{"allow": allowOut},
```

Add `"sort"` to the import block.

- [ ] **Step 8: Update the existing render tests to the new field**

In `internal/gateway/render_test.go`, every `[]Provider` literal changes `Model: x` to `Models: []string{x}`:

- `TestRender` line 11-12: `Models: []string{"deepseek-v4-flash"}` and `Models: []string{"qwen2.5-72b"}`.
- `TestRender` line 23-27: the decoded `Providers` struct already has `Models []struct{...}`, so `d.Models[0].ID` still compiles.
- `TestRender` lines 101-111: the comment says the allowlist lives at `agents.defaults.models`. Update it and add the `modelPolicy.allow` arm. The decoded struct gains `ModelPolicy struct { Allow []string \`json:"allow"\` } \`json:"modelPolicy"\`` next to `Models`.
- `TestRenderPublicModelRemoteEndpoint` line 121: `Models: []string{"pub"}`.
- `TestRenderDeterministic` line 158: `Models: []string{"a"}` and `Models: []string{"b"}`.

- [ ] **Step 9: Run the gateway tests**

Run: `go test ./internal/gateway/`
Expected: PASS (all of them, including the three new ones).

- [ ] **Step 10: Point the controller at the new field**

`internal/controller/openclawconfig_controller.go` still builds `gateway.Provider{... Model: m.Name}`, so the package does not compile yet. Replace the loop at lines 45-49 and the primary fallback at line 68-70:

```go
		for _, m := range t.Spec.Models {
			if m.Endpoint == "" {
				continue
			}
			// One CR entry is still one provider here; Task 2 turns the loop
			// into a per-provider fan-out.
			p := gateway.Provider{Key: m.Name, BaseURL: m.Endpoint, Models: []string{m.Name}}
```

and

```go
	if primary == "" && len(providers) > 0 {
		primary = gateway.ModelKey(providers[0].Key, providers[0].Models[0])
	}
```

The `primary` assignment inside the loop is unchanged: it already builds `m.Name + "/" + m.Name`, which `gateway.ModelKey` produces identically for that input. Leave it, and change it to `gateway.ModelKey(m.Name, m.Name)` for consistency with the fallback.

- [ ] **Step 11: Update the controller test's expectation**

`internal/controller/openclawconfig_controller_test.go:41` uses `k8s.EnvNameForModel("deepseek-v4-flash")`; leave it for now (env naming moves in Task 2). The primary-ref assertion at :33 (`"deepseek-v4-flash/deepseek-v4-flash"`) still holds because the builtin template still has one model named after itself.

- [ ] **Step 12: Run the full suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/gateway/ internal/controller/openclawconfig_controller.go
git commit -s -m "$(cat <<'EOF'
feat(gateway): render every model a provider serves (issue #189)

A provider's models are an array in OpenClaw's config and an id may itself
contain a slash, so the renderer no longer has to assume one model per
provider. ModelKey mirrors OpenClaw's canonical ref rule, and the allowlist is
stated as agents.defaults.modelPolicy.allow instead of relying on the legacy
reading of the agents.defaults.models keys.

The template still supplies one model per provider; the CRD change follows.

Assisted-by: Claude Code
EOF
)"
```

---

### Task 2: The CRD models providers, and every reader follows

**Files:**
- Modify: `internal/api/v1alpha1/agenttemplate_types.go:39-71` and `:164-175`
- Modify: `internal/api/v1alpha1/agenttemplate_types_test.go:70-90`
- Modify: `internal/api/v1alpha1/agentinstance_types.go:25-41`
- Regenerate: `internal/api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`, `deploy/charts/cubepilot/crds/*.yaml`
- Modify: `internal/k8s/client.go:162-190`, `internal/k8s/client_test.go`
- Modify: `internal/controller/builtin.go:64-116`, `internal/controller/builtin_test.go`
- Modify: `internal/controller/openclawconfig_controller.go:41-70`
- Modify: `internal/controller/agentinstance_controller.go:279-305`
- Modify: `internal/resolver/resolver.go:166-209` and `:275-288`
- Modify: `internal/server/handlers_agent.go:140-157`
- Modify: `internal/server/handlers_llms.go:102-133`, `:207-249`, `:339-407`
- Modify (tests): `internal/api/v1alpha1/agenttemplate_types_test.go`, `internal/instances/manager_test.go`, `internal/controller/agentinstance_controller_test.go`, `internal/controller/builtin_test.go`, `internal/controller/openclawconfig_controller_test.go`, `internal/resolver/resolver_test.go`, `internal/server/handlers_llms_test.go`, `internal/server/internal_api_test.go`, `test/e2e/bootstrap_test.go`, `test/e2e/gateway_config_test.go`

**Interfaces:**
- Consumes: `gateway.ModelKey`, `gateway.Provider.Models` from Task 1.
- Produces: `v1alpha1.TemplateProviderSpec{Name, Endpoint string; CredentialRef *corev1.LocalObjectReference; Models []string}` with `Validate() error`; `AgentTemplateSpec.Providers []TemplateProviderSpec`; `AgentTemplateSpec.DefaultModel` now holds a `<provider>/<modelId>` ref; `k8s.EnvNameForProvider(name string) string`; `controller.BuiltinProviderName`.

**Behaviour is preserved for the one-model-per-provider case.** A provider is named after the model it serves here, so `defaultModel` and `selectedModel` stay `<name>/<name>`. Task 3 gives the API a real provider name.

- [ ] **Step 1: Confirm the codegen tool is present**

Run: `controller-gen --version`
Expected: `Version: v0.19.0`. If it is missing: `GOBIN="$(go env GOPATH)/bin" go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0`.

- [ ] **Step 2: Write the failing validation test**

In `internal/api/v1alpha1/agenttemplate_types_test.go`, replace `TestTemplateModelValidate` (lines 70-90) with:

```go
func TestTemplateProviderValidate(t *testing.T) {
	ok := []TemplateProviderSpec{
		{Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1", Models: []string{"qwen3-32b"}},
		{Name: "openrouter", Endpoint: "https://openrouter.ai/api/v1", Models: []string{"anthropic/claude-sonnet-4.5", "openai/gpt-5-mini"}},
		{Name: "a", Endpoint: "https://x", Models: []string{"m"}},
	}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", p.Name, err)
		}
	}
	bad := []TemplateProviderSpec{
		{Name: "", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "Upper", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "has/slash", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "-leading", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "trailing-", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "no-endpoint", Models: []string{"m"}},
		{Name: "no-models", Endpoint: "https://x"},
		{Name: "empty-id", Endpoint: "https://x", Models: []string{""}},
		{Name: "wildcard", Endpoint: "https://x", Models: []string{"*"}},
		{Name: "double-slash", Endpoint: "https://x", Models: []string{"a//b"}},
		{Name: "leading-slash", Endpoint: "https://x", Models: []string{"/a"}},
		{Name: "trailing-slash", Endpoint: "https://x", Models: []string{"a/"}},
		{Name: "space", Endpoint: "https://x", Models: []string{"a b"}},
		{Name: "bad-cred", Endpoint: "https://x", Models: []string{"m"}, CredentialRef: &corev1.LocalObjectReference{}},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", p)
		}
	}
}
```

Also update `TestAgentTemplateSerializationRoundTrip` (lines 13-43) and `TestAgentTemplateRevision`'s fixtures to use `Providers: []TemplateProviderSpec{{Name: "deepseek", Endpoint: ..., CredentialRef: ..., Models: []string{"deepseek-v4-flash"}}}` in place of `Models: []TemplateModelSpec{...}`.

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/api/v1alpha1/ -run TestTemplateProviderValidate`
Expected: FAIL -- `undefined: TemplateProviderSpec`.

- [ ] **Step 4: Replace the type**

In `internal/api/v1alpha1/agenttemplate_types.go`, replace `TemplateModelSpec` and its `Validate` (lines 39-71) with:

```go
// TemplateProviderSpec is one OpenAI-compatible LLM provider of an
// AgentTemplate (design §3.3: models are inlined -- no standalone Model CRD).
// A provider owns the endpoint and the credential once, and lists the backend
// model ids reachable through it, so a gateway that serves many models behind
// one base URL and one key is described once rather than once per model.
type TemplateProviderSpec struct {
	// Name is the provider key: the OpenClaw models.providers key, the prefix
	// of every model ref (<name>/<modelId>) and the suffix of the credential
	// Secret name llm-<name>. A DNS-1123 label, because it is both a ref
	// segment (refs split on the first "/" and have no escaping) and a
	// resource-name segment.
	//
	// A name that exactly matches an OpenClaw built-in provider key
	// (anthropic, nvidia, xai, google, ...) inherits that provider's model-id
	// normalization, which can rewrite the id sent to the endpoint. Prefer a
	// distinct name such as nvidia-proxy unless that rewrite is intended.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL.
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`
	// CredentialRef optionally references a platform-managed Secret (name)
	// holding the apiKey; a provider that needs no credentials omits it (nil).
	// References only -- never the key itself (design §4.4).
	// +optional
	CredentialRef *corev1.LocalObjectReference `json:"credentialRef,omitempty"`
	// Models are the backend model ids served through this endpoint. Each id is
	// sent to the endpoint verbatim and may itself contain "/" (OpenRouter's
	// "anthropic/claude-sonnet-4.5").
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	// +listType=set
	Models []string `json:"models"`
}
```

The `MaxItems` and `MaxLength` bounds are not decoration. Kubernetes estimates a CEL rule's cost statically, and an unbounded array is assumed to be large: without them, the validation rules on `Providers` exceed the API server's cost budget and the **CRD becomes uninstallable** -- `kubectl apply` rejects it outright, which `make test` cannot see because it never talks to an API server. Confirmed with `kubectl apply --dry-run=server`, which reports the offending rules by index and names the remedy ("adding maxItems, maxProperties, and maxLength where arrays, maps, and strings are declared"). The numbers are a guess at a sane ceiling, not a measured threshold; Step 7 verifies they are enough and says what to do if they are not.

```go

// providerNameRE is the DNS-1123 label grammar, mirroring the Pattern marker on
// TemplateProviderSpec.Name.
var providerNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate enforces the provider invariants. The same rules are enforced on the
// API server by the markers on the type and the CEL XValidations on Providers.
func (p TemplateProviderSpec) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("provider name is required")
	}
	if len(p.Name) > 63 || !providerNameRE.MatchString(p.Name) {
		return fmt.Errorf("provider %q must be a lowercase DNS-1123 label of at most 63 characters", p.Name)
	}
	if p.Endpoint == "" {
		return fmt.Errorf("provider %q requires an endpoint", p.Name)
	}
	if p.CredentialRef != nil && p.CredentialRef.Name == "" {
		return fmt.Errorf("provider %q credentialRef must reference a Secret name", p.Name)
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("provider %q requires at least one model", p.Name)
	}
	for _, id := range p.Models {
		if err := validateModelID(id); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
	}
	return nil
}

// validateModelID rejects the ids that would not survive being used as a model
// ref. Everything else is data sent to the endpoint, so the grammar is
// deliberately loose: ids routinely contain "/".
func validateModelID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("model id must not be empty")
	case id == "*":
		return fmt.Errorf("model id %q is reserved for allowlist wildcards", id)
	case strings.TrimSpace(id) != id || strings.ContainsAny(id, " \t\n\r"):
		return fmt.Errorf("model id %q must not contain whitespace", id)
	case strings.HasPrefix(id, "/"), strings.HasSuffix(id, "/"), strings.Contains(id, "//"):
		return fmt.Errorf("model id %q must not contain an empty path segment", id)
	}
	return nil
}
```

Add `"regexp"` and `"strings"` to the import block.

- [ ] **Step 5: Replace the spec fields and CEL rules**

In the same file, replace `DefaultModel` and `Models` (lines 164-175) with:

```go
	// DefaultModel is the model ref (<provider>/<modelId>) used when an
	// instance does not select one explicitly. Empty = no default / runtime
	// default.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	DefaultModel string `json:"defaultModel,omitempty"`
	// Providers is the inline provider list (design §3.3: models are inlined in
	// the template -- no standalone Model CRD). Each provider declares an
	// endpoint, an optional credential and the model ids it serves; an instance
	// selects a <provider>/<modelId> ref within this list.
	// +kubebuilder:validation:XValidation:rule="self.all(p, !has(p.credentialRef) || has(p.credentialRef.name))",message="credentialRef must reference a Secret name"
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.models.all(m, m != '' && m != '*' && !m.contains('//') && !m.startsWith('/') && !m.endsWith('/')))",message="every model id must be non-empty, without an empty path segment, and not the wildcard"
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Providers []TemplateProviderSpec `json:"providers,omitempty"`
```

Then add a cross-field rule above the `AgentTemplateSpec` type declaration, alongside the existing type-level comment:

```go
// +kubebuilder:validation:XValidation:rule="self.defaultModel == \"\" || self.providers.exists(p, p.models.exists(m, (m.startsWith(p.name + '/') ? m : p.name + '/' + m) == self.defaultModel))",message="defaultModel must name a provider/model listed in providers"
type AgentTemplateSpec struct {
```

Two things about that rule are load-bearing:

- The ternary mirrors `gateway.ModelKey`. Plain `p.name + '/' + m` is wrong: an id that already names its provider (`vllm` serving `vllm/qwen3-32b`) is its own ref, and the naive concatenation would reject the only ref the renderer accepts.
- The empty string is written `\"\"`, not `''`. gofmt rewrites `''` inside a doc comment (and a kubebuilder marker lives in one) into a typographic quote, which breaks the rule and fails the format gate.

Residual difference from `ModelKey`, worth knowing rather than fixing: `ModelKey` compares the self-prefix case-insensitively, CEL's `startsWith` does not, so an id like `VLLM/x` under provider `vllm` passes Go validation and is rejected by CEL. Provider names are constrained to lowercase, so reaching it needs a deliberately odd model id.

- [ ] **Step 6: Remove the dead instance fields**

In `internal/api/v1alpha1/agentinstance_types.go`, delete `ModelRef` and `Endpoint` from `CredentialSpec` (lines 32-40). They are documented as the model-to-credential binding but no code reads them: `CredentialFor` matches `Target` only.

- [ ] **Step 7: Regenerate the deepcopy and the CRDs**

Run:

```bash
controller-gen object:headerFile=hack/boilerplate.go.txt paths="./internal/api/v1alpha1/..."
controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases
cp config/crd/bases/ai.cubestack.io_agenttemplates.yaml deploy/charts/cubepilot/crds/
```

Then verify the copies match and no unrelated churn crept in:

```bash
diff -q config/crd/bases/ai.cubestack.io_agenttemplates.yaml deploy/charts/cubepilot/crds/ai.cubestack.io_agenttemplates.yaml
git status --short config/crd deploy/charts/cubepilot/crds
```

Expected: no diff output for the pair, and only `ai.cubestack.io_agenttemplates.yaml` (and the regenerated deepcopy) modified. If controller-gen reformatted other CRD files, restore them with `git checkout -- <file>`.

Also confirm the CEL rules landed:

```bash
grep -n "defaultModel must name\|empty path segment\|x-kubernetes-list-map-keys\|x-kubernetes-list-type" config/crd/bases/ai.cubestack.io_agenttemplates.yaml
```

Expected: the `defaultModel must name a provider/model` message (on the spec), the model-id rule message and `x-kubernetes-list-type: set` (on the provider's models array), and `x-kubernetes-list-map-keys` with `name` plus `x-kubernetes-list-type: map` (on providers).

Then **apply it against a real API server**. This is the only check that validates the CEL rules at all -- `make test` never talks to one, so a rule the API server refuses passes every local gate:

```bash
kubectl apply --dry-run=server -f deploy/charts/cubepilot/crds/ai.cubestack.io_agenttemplates.yaml
```

Expected: `customresourcedefinition.apiextensions.k8s.io/agenttemplates.ai.cubestack.io configured (server dry run)`. The same command on `main` succeeds, so any failure here is this change's.

If it reports `estimated rule cost exceeds budget`, the bounds are not tight enough -- Kubernetes sizes an unbounded array optimistically, and the nested `exists` over providers x models is what blows the static budget. Tighten rather than drop: first lower `Providers` to `MaxItems=16` and `Models` to `MaxItems=32`; if a rule is still over, replace the `defaultModel` rule with the cheaper prefix check

```
// +kubebuilder:validation:XValidation:rule="self.defaultModel == \"\" || self.providers.exists(p, self.defaultModel.startsWith(p.name + '/'))",message="defaultModel must name a provider/model listed in providers"
```

which no longer verifies the id itself -- `resolveModel`'s fail-closed check and Go `Validate()` are then the only gates on it. That is a real weakening, so record in the report which form you landed and why the next reviewer should see it.

- [ ] **Step 8: Rename the credential key derivation**

`internal/k8s/client.go`: rename `EnvNameForModel` to `EnvNameForProvider` and update its doc comment to say the argument is now a provider name (the algorithm -- sanitize plus a short hash -- is unchanged). `internal/k8s/client_test.go`: rename `TestEnvNameForModelNoCollision` to `TestEnvNameForProviderNoCollision` and update the calls.

- [ ] **Step 9: Update the builtin template**

`internal/controller/builtin.go` -- add the provider name constant next to `BuiltinAgentName`:

```go
// BuiltinProviderName is the provider key of the platform's own LLM. It is a
// name, not a model id: the platform default is one provider serving one model,
// and an admin can add more model ids to it or add providers beside it.
const BuiltinProviderName = "platform"
```

Replace `BuiltinModels` (lines 64-78) with:

```go
// BuiltinProviders returns the preset provider for the builtin AgentTemplate
// (design §3.3: models are inlined in the template -- no standalone Model CRD).
// The platform default provider references the cubepilot-llm credential Secret
// created by setup.sh; its endpoint and model name come from the operator
// config (config.LLMEndpoint / config.LLMModel) and can be edited on the CR
// after install.
func BuiltinProviders(endpoint, modelName string) []v1alpha1.TemplateProviderSpec {
	return []v1alpha1.TemplateProviderSpec{
		{
			Name:          BuiltinProviderName,
			Endpoint:      endpoint,
			CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
			Models:        []string{modelName},
		},
	}
}
```

In `BuiltinAgentTemplate` (lines 93-98): `DefaultModel: gateway.ModelKey(BuiltinProviderName, modelName)` and `Providers: BuiltinProviders(endpoint, modelName)`. Add the `gateway` import.

At line 194 (`agent.Spec.Models = nil`) set `agent.Spec.Providers = nil`, and at line 195 leave `agent.Spec.DefaultModel = ""`.

- [ ] **Step 10: Fan the render loop out per provider**

`internal/controller/openclawconfig_controller.go` lines 45-66 become:

```go
		for _, pr := range t.Spec.Providers {
			if pr.Endpoint == "" || len(pr.Models) == 0 {
				continue
			}
			p := gateway.Provider{Key: pr.Name, BaseURL: pr.Endpoint, Models: pr.Models}
			if pr.CredentialRef != nil && pr.CredentialRef.Name != "" {
				var sec corev1.Secret
				if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: pr.CredentialRef.Name}, &sec); err != nil {
					log.Printf("openclaw-config: provider %q credential %q not ready (%v), skipping", pr.Name, pr.CredentialRef.Name, err)
					continue
				}
				// Reference the credential by name only: the rendered config
				// carries a file SecretRef into the emptyDir keys.json the
				// supervisor writes from the Secret. The literal key never lands
				// in the config or the PVC.
				p.APIKey = k8s.EnvNameForProvider(pr.Name)
			}
			if primary == "" {
				for _, id := range pr.Models {
					if gateway.ModelKey(pr.Name, id) == t.Spec.DefaultModel {
						primary = t.Spec.DefaultModel
						break
					}
				}
			}
			providers = append(providers, p)
		}
```

and the fallback at lines 68-70:

```go
	// The Models guard is not redundant with the loop's skip: the skip makes it
	// unreachable today, but the field is a slice now, so an index without the
	// guard is a panic waiting for the next caller.
	if primary == "" && len(providers) > 0 && len(providers[0].Models) > 0 {
		primary = gateway.ModelKey(providers[0].Key, providers[0].Models[0])
	}
```

The `apiKey` reference is now per provider, so every model of a skipped provider is skipped with it.

- [ ] **Step 11: Update the instance controller's availability check**

`internal/controller/agentinstance_controller.go`, `modelAvailable` (lines 279-305) -- replace the loop body:

```go
	for i := range agent.Spec.Providers {
		p := &agent.Spec.Providers[i]
		if p.Endpoint == "" || len(p.Models) == 0 {
			continue
		}
		if p.CredentialRef == nil || p.CredentialRef.Name == "" {
			return true, nil
		}
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: p.CredentialRef.Name}, &sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue // keyed provider whose credential is not created yet
			}
			return false, err
		}
		return true, nil
	}
```

Update its doc comment to say "at least one usable provider". The reconciler's `Watches` on Secrets is unchanged.

- [ ] **Step 12: Update the resolver**

`internal/resolver/resolver.go`:

(a) The credential fan-out at lines 169-181 becomes one entry per provider:

```go
				// Credential mapping for the gateway's file secret provider:
				// the supervisor reads these Secrets and writes keys.json into
				// the pod's emptyDir (design §6). One entry per provider -- the
				// credential is the provider's, not the model's.
				for _, pr := range def.Spec.Providers {
					// Match the renderer's eligibility rule: providers with an
					// empty endpoint (or no models) are dropped from the
					// rendered config, so their credentials must not appear
					// either (a missing Secret on an ineligible provider would
					// otherwise block valid ones).
					if pr.Endpoint == "" || len(pr.Models) == 0 ||
						pr.CredentialRef == nil || pr.CredentialRef.Name == "" {
						continue
					}
					cfg.Credentials = append(cfg.Credentials, ResolvedCredential{
						Env:        k8s.EnvNameForProvider(pr.Name),
						SecretName: pr.CredentialRef.Name,
					})
				}
```

(b) `resolveModel` (lines 275-288) becomes:

```go
// resolveModel validates the selection ref against the template's providers and
// returns the effective override ref. Fail-closed: an unknown provider, or an
// id the named provider does not serve, is an error. The returned ref is the
// same <provider>/<modelId> string the renderer puts in the allowlist, so the
// override always matches.
func (r *Resolver) resolveModel(selected string, def v1alpha1.AgentTemplate) (string, error) {
	for _, pr := range def.Spec.Providers {
		for _, id := range pr.Models {
			if ref := gateway.ModelKey(pr.Name, id); ref == selected {
				return ref, nil
			}
		}
	}
	return "", fmt.Errorf("model %q is not available in template %q (add it under Agent config -> LLM Config, then select it again)", selected, def.Name)
}
```

The `ModelName` display field keeps `def.Spec.DefaultModel` as its value (line 208), which is now a ref. Update the field's doc comment at line 57-60 to say "the model ref" rather than "the catalog model name".

Add the `gateway` import. Note that `gateway` must not import `resolver` -- it does not.

- [ ] **Step 13: Update the agent-config model check**

`internal/server/handlers_agent.go:151`:

```go
	for _, pr := range tmpl.Spec.Providers {
		for _, id := range pr.Models {
			if gateway.ModelKey(pr.Name, id) == model {
				return true, nil
			}
		}
	}
```

Add the `gateway` import. Update the function's doc comment (line 136-139) to say the argument is a `<provider>/<modelId>` ref.

- [ ] **Step 14: Make the LLM handler write a provider**

`internal/server/handlers_llms.go` -- in Task 2 the handler keeps its one-entry-at-a-time shape; the provider is named after the posted name and serves exactly that id. Replace lines 107-133:

```go
	for _, pr := range tmpl.Spec.Providers {
		if pr.Name == name {
			writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("provider %q already exists", name)})
			return
		}
	}

	// Commit the provider to the template BEFORE creating the credential
	// Secret: a failed template update leaves no orphaned key Secret, and a
	// re-add with a new key never keeps the old one (the operator skips a
	// provider whose Secret is missing and re-renders once it appears).
	provider := v1alpha1.TemplateProviderSpec{Name: name, Endpoint: endpoint, Models: []string{name}}
	if body.APIKey != "" {
		provider.CredentialRef = &corev1.LocalObjectReference{Name: llmCredentialName(name)}
	}
	tmpl.Spec.Providers = append(tmpl.Spec.Providers, provider)
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	if body.APIKey != "" {
		if err := upsertLLMCredential(r.Context(), s, llmCredentialName(name), body.APIKey); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("create credential Secret: %v", err)})
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"provider": provider})
```

`handleUpdateLLM` (lines 207-249) indexes `tmpl.Spec.Providers` instead of `tmpl.Spec.Models`, and `current.CredentialRef` reads the same field on the provider -- the body of the function is otherwise unchanged.

`handleDeleteLLM` (lines 339-407) indexes `tmpl.Spec.Providers`. Its 409 guard has to move with it, and leaving that to the later API task is not safe: `instancesSelecting` compares `inst.Spec.SelectedModel` against the value it is handed, `SelectedModel` is now a `<provider>/<modelId>` ref, and the handler still hands it the provider name -- so the guard would silently stop matching and a provider an instance still selects would become deletable. Make `instancesSelecting` take the ref set:

```go
// instancesSelecting lists the AgentInstances of the builtin template that
// explicitly select one of the given model refs. Instances bound to another
// template are ignored: their selection resolves against that template, so this
// one cannot break it.
func (s *Server) instancesSelecting(ctx context.Context, refs map[string]bool) ([]modelInstanceRef, error) {
	var list v1alpha1.AgentInstanceList
	if err := s.cr.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := []modelInstanceRef{}
	for _, inst := range list.Items {
		if inst.Spec.TemplateRef == v1alpha1.DefaultAgentName && refs[inst.Spec.SelectedModel] {
			out = append(out, modelInstanceRef{Name: inst.Name, Owner: inst.Spec.Owner})
		}
	}
	return out, nil
}
```

and build the set from the provider being removed, so both the guard and the `DefaultModel` clearing at line 388 test the same refs:

```go
	removed := tmpl.Spec.Providers[idx]
	refs := make(map[string]bool, len(removed.Models))
	for _, id := range removed.Models {
		refs[gateway.ModelKey(removed.Name, id)] = true
	}
	selecting, err := s.instancesSelecting(r.Context(), refs)
	// ... the existing 409 arm, unchanged ...
	tmpl.Spec.Providers = append(tmpl.Spec.Providers[:idx], tmpl.Spec.Providers[idx+1:]...)
	if tmpl.Spec.DefaultModel != "" && refs[tmpl.Spec.DefaultModel] {
		tmpl.Spec.DefaultModel = ""
	}
```

- [ ] **Step 15: Run the suite and fix the test fixtures**

Run: `make test`

Every test that builds `v1alpha1.TemplateModelSpec` fails to compile. Update them mechanically:

| File | Change |
|---|---|
| `internal/instances/manager_test.go:60,83,107` | `Models: []TemplateModelSpec{{Name: "deepseek-v4-flash", ...}}` -> `Providers: []TemplateProviderSpec{{Name: "platform", Models: []string{"deepseek-v4-flash"}, ...}}`; `DefaultModel: "deepseek-v4-flash"` -> `"platform/deepseek-v4-flash"` |
| `internal/server/internal_api_test.go:31,339,386` | same shape |
| `internal/controller/builtin_test.go:48-63,174-175,227-228` | assert on `agent.Spec.Providers[0].Name == controller.BuiltinProviderName`, `.Models[0] == "deepseek-v4-flash"`, `.Endpoint == config.DefaultLLMEndpoint`; `DefaultModel == "platform/deepseek-v4-flash"`; the no-LLM case asserts `len(agent.Spec.Providers) == 0` |
| `internal/controller/agentinstance_controller_test.go:236,306` | template fixtures -> providers; the `ModelConfigured` subtests keep their names and semantics (a keyed provider whose Secret is missing is still not-configured) |
| `internal/controller/openclawconfig_controller_test.go:41` | `k8s.EnvNameForProvider(controller.BuiltinProviderName)` |
| `internal/resolver/resolver_test.go:95,148,170,240` | fixtures -> providers; line 147's expected override ref becomes `"platform/deepseek-chat"` |
| `internal/server/handlers_llms_test.go` | `keyedModel`/`llmTestServer` build providers; `templateModels` becomes `templateProviders` returning `[]v1alpha1.TemplateProviderSpec`; `shareCredentialWith` mutates `Spec.Providers[i].CredentialRef` |
| `test/e2e/bootstrap_test.go:44-45` | `tpl.Spec.DefaultModel` is non-empty; `tpl.Spec.Providers` is not empty |
| `test/e2e/gateway_config_test.go:30,67-68` | the expected primary is `gateway.ModelKey(providerName, modelID)` from the live template; the provider key looked up in `models.providers` is the provider's name, not the model id |

Re-run `make test` until it passes. Then run `go vet ./...` (it covers `test/e2e` too, which `make test`'s `go test` skips).

- [ ] **Step 16: Commit**

```bash
git add -A
git commit -s -m "$(cat <<'EOF'
feat(api): model LLM providers with a list of model ids (issue #189)

AgentTemplate.spec.models[] becomes spec.providers[], where each provider owns
one endpoint, one credential and the backend model ids it serves. A provider
name is a DNS-1123 label because it is both a ref segment and the credential
Secret name suffix; model ids stay free-form and may contain "/".

The one-to-one shape was ours alone -- OpenClaw's provider models are already an
array and a ref splits on the first "/" -- so this removes the duplication and
the invalid Secret name that an id like "anthropic/claude-sonnet-4.5" produced.

Behaviour is unchanged for one model per provider: the write API still names the
provider after the model until the follow-up reworks it.

Assisted-by: Claude Code
EOF
)"
```

---

### Task 3: The write API serves multiple models per provider

**Files:**
- Modify: `internal/server/handlers_llms.go` (`llmRequest` at :25-34, `handleAddLLM`, `handleUpdateLLM`, `handleDeleteLLM`, `removeModelCredential` at :277-299)
- Modify: `internal/server/handlers_llms_test.go`
- Modify: `docs/cubepilot/api.md` (section 6.3)
- Modify: `docs/bruno/` collection entries for `/api/v1/llms` (the collection added for the /api/v1 surface)

**Interfaces:**
- Consumes: `v1alpha1.TemplateProviderSpec` from Task 2.
- Produces: `llmRequest{Name, Endpoint, APIKey string; Public bool; Models []string}`; `POST /api/v1/llms` creates a provider; `PUT /api/v1/llms/{name}` replaces endpoint, credential and the model list; `DELETE /api/v1/llms/{name}` removes the provider and its credential. Routes are unchanged, so `apidoc_test.go`'s path assertions still hold -- but the documented request shape must be updated.

- [ ] **Step 1: Write the failing tests**

Add to `internal/server/handlers_llms_test.go`:

```go
// TestHandleAddLLMProviderWithModels: one endpoint and one key serving several
// model ids is the case the flat list could not express. The credential is
// created once, for the provider, not once per id.
func TestHandleAddLLMProviderWithModels(t *testing.T) {
	s := llmTestServer(t)
	body := map[string]any{
		"name":     "vllm",
		"endpoint": "http://vllm.ai.svc:8000/v1",
		"apiKey":   "sk-vllm",
		"models":   []string{"qwen3-32b", "deepseek-v4-flash"},
	}
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/v1/llms", "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	providers := templateProviders(t, s)
	last := providers[len(providers)-1]
	if last.Name != "vllm" || len(last.Models) != 2 {
		t.Fatalf("provider = %+v, want vllm with two ids", last)
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-vllm"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	// One Secret for the provider, none per model.
	for _, id := range last.Models {
		if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-" + id}, &corev1.Secret{}); err == nil {
			t.Errorf("model %q should not get its own credential Secret", id)
		}
	}
}

// TestHandleUpdateLLMReplacesModels: adding and removing a single id is a PUT
// with the full list, which keeps the route surface unchanged.
func TestHandleUpdateLLMReplacesModels(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b"},
	})
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/llms/vllm", "", map[string]any{
		"endpoint": "http://vllm.ai.svc:8000/v1",
		"models":   []string{"qwen3-32b", "qwen3-8b"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, p := range templateProviders(t, s) {
		if p.Name == "vllm" && (len(p.Models) != 2 || p.Models[1] != "qwen3-8b") {
			t.Errorf("models = %v, want [qwen3-32b qwen3-8b]", p.Models)
		}
	}
}

// TestHandleUpdateLLMRefusesEmptyModels: a provider with no model ids renders
// nothing and is unselectable, so the list can never be emptied.
func TestHandleUpdateLLMRefusesEmptyModels(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b"},
	})
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/llms/vllm", "", map[string]any{
		"endpoint": "http://vllm.ai.svc:8000/v1",
		"models":   []string{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleDeleteLLMRefusesWhileAnyModelIsSelected: the refusal covers every id
// the provider serves, not just one.
func TestHandleDeleteLLMRefusesWhileAnyModelIsSelected(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b", "qwen3-8b"},
	})
	seedInstanceSelecting(t, s, "vllm/qwen3-8b") // the second id, not the first
	rec := doReq(t, s.Handler(), http.MethodDelete, "/api/v1/llms/vllm", "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
```

`seedInstanceSelecting` is a small helper: create an `AgentInstance` in namespace `cubepilot` named `k8s.InstanceName("zhang.wei", v1alpha1.DefaultAgentName)` with `Spec.Owner` and `Spec.SelectedModel` set to the given ref. If `handlers_llms_test.go` already has such a fixture in `TestHandleDeleteLLMRefusesSelectedModel`, reuse it instead of adding a second one.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server/ -run 'TestHandleAddLLMProviderWithModels|TestHandleUpdateLLMReplacesModels|TestHandleUpdateLLMRefusesEmptyModels|TestHandleDeleteLLMRefusesWhileAnyModelIsSelected'`
Expected: FAIL -- the request body's `models` field is ignored, so the provider gets one id and the assertions on the list fail.

- [ ] **Step 3: Add the field and the validation**

In `internal/server/handlers_llms.go`, `llmRequest` (lines 25-34) becomes:

```go
type llmRequest struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
	// Models are the backend model ids this provider serves. Name is the
	// provider name -- the ref prefix and the credential Secret suffix -- and
	// has nothing to do with the ids, which are sent to the endpoint verbatim.
	Models []string `json:"models"`
	// Public declares that the endpoint requires no credentials. A provider
	// without a credential is only ever stored when the request says so:
	// otherwise a forgotten apiKey would save a provider that can be selected
	// and fails every turn with "No API key resolved" (gateway.PublicModelAPIKey).
	Public bool `json:"public"`
}
```

Add the shared validation, next to `credentialChoiceError`:

```go
// normalizeModels trims and de-duplicates the requested model ids. The grammar
// is deliberately not restated here: the handler validates the assembled
// TemplateProviderSpec with its own Validate, so the HTTP path and a
// hand-edited CR are held to exactly the same rules by one implementation.
func normalizeModels(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
```

The empty-list rule, the id grammar and the provider-name grammar all arrive through `TemplateProviderSpec.Validate()` (Task 2, Step 4) rather than being written a second time. Trim-then-validate matters: `Validate` rejects an id with surrounding whitespace rather than silently trimming it, so the trim has to happen first and only trailing blank lines from the web form are absorbed.

- [ ] **Step 4: Wire it into add and update**

`handleAddLLM`: after the `credentialChoiceError` check, build the provider and validate it before anything is written:

```go
	provider := v1alpha1.TemplateProviderSpec{
		Name:     name,
		Endpoint: endpoint,
		Models:   normalizeModels(body.Models),
	}
	if body.APIKey != "" {
		provider.CredentialRef = &corev1.LocalObjectReference{Name: llmCredentialName(name)}
	}
	// One validator for the HTTP path and the CRD: an empty model list, a bad id
	// or a bad provider name is refused with the same message either way.
	if err := provider.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
```

Then the duplicate-provider check runs against `tmpl.Spec.Providers`, the provider is appended and the credential Secret is created as below. The provider `Name` stays the sanitized posted name.

`handleUpdateLLM`: after the `normalizeEndpoint` call, apply the new list to the stored provider and validate the result before writing:

```go
	provider := current
	provider.Endpoint = endpoint
	provider.Models = normalizeModels(body.Models)
	// ... the existing apiKey / public credential decision, unchanged ...
	if err := provider.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
```

and then store it: `tmpl.Spec.Providers[idx] = provider`. A PUT always carries the full list, so an absent or empty `models` is a client error rather than "keep the current ones" -- the web form always sends it, and "keep" would be the one way to reach a provider with no ids.

**This changes the PUT contract**, so the existing update tests fail and must be updated in this step: every `doReq(..., http.MethodPut, "/api/v1/llms/...", ...)` body in `internal/server/handlers_llms_test.go` gains `"models": []string{<the provider's ids>}`. Grep for them with `grep -n "http.MethodPut, \"/api/v1/llms" internal/server/handlers_llms_test.go` and fix each one; the tests whose subject is the credential decision (`TestHandleUpdateLLMKeepsKey`, `...PromotesPublic`, `...KeepsSharedCredential`) pass the single id the fixture provider serves.

- [ ] **Step 5: Scope the delete refusal and the credential deletion**

The `instancesSelecting` signature change and the delete handler's ref set are already in place from Task 2, Step 14 -- they could not wait until this task, because the guard would have silently stopped matching in between. Start this step by confirming they are there, then do the credential half.

`instancesSelecting` (lines 325-337) takes a set of refs:

```go
// instancesSelecting lists the AgentInstances of the builtin template that
// explicitly select one of the given model refs. Instances bound to another
// template are ignored: their selection resolves against that template, so this
// one cannot break it.
func (s *Server) instancesSelecting(ctx context.Context, refs map[string]bool) ([]modelInstanceRef, error) {
	var list v1alpha1.AgentInstanceList
	if err := s.cr.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := []modelInstanceRef{}
	for _, inst := range list.Items {
		if inst.Spec.TemplateRef == v1alpha1.DefaultAgentName && refs[inst.Spec.SelectedModel] {
			out = append(out, modelInstanceRef{Name: inst.Name, Owner: inst.Spec.Owner})
		}
	}
	return out, nil
}
```

`handleDeleteLLM` lines 350-406 become:

```go
	idx := -1
	for i := range tmpl.Spec.Providers {
		if tmpl.Spec.Providers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("provider %q not found", name)})
		return
	}

	// The refusal covers every id the provider serves: deleting the provider
	// deletes all of them, and SelectedModelFor is fail-closed, so a user still
	// selecting any one of them would start failing with a resolver error
	// instead of falling back.
	removed := tmpl.Spec.Providers[idx]
	refs := make(map[string]bool, len(removed.Models))
	for _, id := range removed.Models {
		refs[gateway.ModelKey(removed.Name, id)] = true
	}
	selecting, err := s.instancesSelecting(r.Context(), refs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(selecting) > 0 {
		// The Portal shows this message verbatim, so it names who is blocking
		// the delete rather than only counting them.
		who := make([]string, 0, len(selecting))
		for _, sel := range selecting {
			if sel.Owner != "" {
				who = append(who, sel.Owner)
			} else {
				who = append(who, sel.Name)
			}
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("provider %q serves a model selected by %s; select another model there first",
				name, strings.Join(who, ", ")),
			"instances": selecting,
		})
		return
	}

	tmpl.Spec.Providers = append(tmpl.Spec.Providers[:idx], tmpl.Spec.Providers[idx+1:]...)
	if tmpl.Spec.DefaultModel != "" && refs[tmpl.Spec.DefaultModel] {
		// The removed provider may have served the gateway's primary. Clearing
		// the ref is defined: the renderer falls back to the first remaining
		// provider. A dangling ref would leave the CR referencing a model that
		// is gone.
		tmpl.Spec.DefaultModel = ""
	}
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	warning := ""
	if w := removeProviderCredential(r.Context(), s, removed.Name, removed.CredentialRef); w != "" {
		warning = "provider removed, but its " + w
	}
	resp := map[string]any{"removed": removed.Name}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
```

`removeModelCredential` (lines 287-299) is renamed, since the Secret now belongs to the provider rather than to a model. The body is otherwise unchanged -- in particular the ownership check keeps its job of never deleting a Secret this API did not name:

```go
// removeProviderCredential deletes the credential Secret a provider owns, and
// returns a warning for the response ("" when there is nothing to report). It is
// called after the provider change is already committed, so a failure is
// reported rather than raised -- an error response would read as "the change
// failed" when it did not.
//
// Only the Secret this API names after the provider (llm-<name>) is removed. A
// CR hand-edited to point a provider at a Secret it shares with another
// provider (the builtin's cubepilot-llm, say) must not lose that Secret when
// this provider goes: the other provider would silently lose its credential.
// Removing a single model id never reaches here -- that is an update, not a
// delete, so a provider's credential always outlives its model list.
func removeProviderCredential(ctx context.Context, s *Server, providerName string, ref *corev1.LocalObjectReference) string {
	if ref == nil || ref.Name == "" {
		return ""
	}
	owned := llmCredentialName(providerName)
	if ref.Name != owned {
		return fmt.Sprintf("credential Secret %q is not the platform-managed %q and was left in place", ref.Name, owned)
	}
	if err := deleteLLMCredential(ctx, s, ref.Name); err != nil {
		return fmt.Sprintf("credential Secret %q could not be deleted: %v", ref.Name, err)
	}
	return ""
}
```

`llmCredentialName` (lines 273-275) keeps its body but its doc comment changes from "the credential Secret name for a model" to "the credential Secret name for a provider: derived from the provider name, which is immutable, so it never drifts".

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/server/`
Expected: PASS.

- [ ] **Step 7: Update the API docs and the request collection**

`docs/cubepilot/api.md` section 6.3 (about line 569): the `POST /api/v1/llms` and `PUT /api/v1/llms/{name}` bodies gain `models` (array of backend model ids, at least one, ids may contain `/`), `name` is documented as the provider name (a DNS-1123 label, the ref prefix and the credential Secret suffix), and the DELETE 409 now covers every id the provider serves. Keep the path table exactly as it is -- `apidoc_test.go` parses `server.go` and fails on a stale or missing path.

Update the Bruno collection entries for `/api/v1/llms` to match the new bodies.

- [ ] **Step 8: Run the doc test and commit**

Run: `go test ./internal/server/ -run TestAPIDoc`
Expected: PASS.

```bash
git add internal/server/ docs/
git commit -s -m "$(cat <<'EOF'
feat(api): add, edit and remove a provider's model ids (issue #189)

POST /api/v1/llms now creates a provider that serves the requested model ids
rather than one id named after itself, and PUT /api/v1/llms/{name} replaces the
list, so adding or removing a single model is an edit of the provider. The
credential is created once per provider.

DELETE refuses while any instance selects any id the provider serves, and only
ever removes the credential Secret this API named -- a hand-edited shared
credential stays put.

Assisted-by: Claude Code
EOF
)"
```

---

### Task 4: The web card becomes a provider card

**Files:**
- Modify: `web/src/views/AgentView.tsx` (`TemplateModel` at :9-13, state at :33-39, `loadTemplate` at :47-62, model select at :352-368, LLM card at :405-458, handlers at :254-327)
- Modify: `web/src/api/index.ts:151-165`
- Modify: `web/src/api/types.ts` if the request types are declared there

**Interfaces:**
- Consumes: the Task 3 API -- `POST /api/v1/llms` with `{name, endpoint, apiKey?, public, models[]}`, `PUT /api/v1/llms/{name}`, `DELETE /api/v1/llms/{name}`; templates carry `spec.providers[]` and a `spec.defaultModel` ref.
- Produces: nothing consumed by later tasks. Verification is `npm run build` (there are no frontend tests) plus a manual pass against a running stack.

- [ ] **Step 1: Replace the template model shape**

In `web/src/views/AgentView.tsx`:

```ts
interface TemplateProvider {
  name: string
  endpoint: string
  credentialRef?: { name: string }
  models: string[]
}
```

State: `templateModels` becomes `templateProviders: TemplateProvider[]`, and the form state becomes

```ts
const [llmForm, setLLMForm] = useState({ name: '', endpoint: '', apiKey: '', public: false, models: '' })
const [editingProvider, setEditingProvider] = useState<string | null>(null)
```

`models` is a single text field holding one id per line -- the form is shared by add and edit, and a repeatable sub-form would make the common case (adding one id to an existing provider) the slow path.

`loadTemplate` maps `tmpl.spec?.providers` into `TemplateProvider[]`, defaulting `models` to `[]`.

- [ ] **Step 2: Make adding a model the cheap path**

Add a per-provider inline input that appends to an existing provider's list without opening the form:

```tsx
async function addModelToProvider(p: TemplateProvider, id: string) {
  const trimmed = id.trim()
  if (!trimmed || p.models.includes(trimmed)) return
  setLLMBusy(true)
  try {
    await api.updateLLM(p.name, { endpoint: p.endpoint, models: [...p.models, trimmed] })
    showToast(`Model "${trimmed}" added`)
    await loadTemplate()
  } catch (e) {
    showToast('Add model failed: ' + (e instanceof Error ? e.message : String(e)))
  } finally {
    setLLMBusy(false)
  }
}

async function removeModelFromProvider(p: TemplateProvider, id: string) {
  if (p.models.length === 1) {
    showToast('A provider needs at least one model -- remove the provider instead')
    return
  }
  setLLMBusy(true)
  try {
    await api.updateLLM(p.name, { endpoint: p.endpoint, models: p.models.filter((m) => m !== id) })
    showToast(`Model "${id}" removed`)
    await loadTemplate()
  } catch (e) {
    showToast('Remove model failed: ' + (e instanceof Error ? e.message : String(e)))
  } finally {
    setLLMBusy(false)
  }
}
```

- [ ] **Step 3: Render the provider card**

Add the per-provider "new model id" draft state next to the other LLM state:

```ts
// One draft model id per provider, for the inline "Add model" field. Adding an
// id is the most frequent operation, so it must not go through the full form.
const [newModel, setNewModel] = useState<Record<string, string>>({})
```

Add the two provider-level handlers next to `startEditLLM` / `removeLLM` (lines 259-283), which are replaced by these and by `submitLLM`'s edit branch below:

```tsx
function startEditProvider(p: TemplateProvider) {
  // The stored key is never sent to the browser, so the field starts blank --
  // and a blank key on edit means "keep the current credential".
  setLLMForm({
    name: p.name,
    endpoint: p.endpoint,
    apiKey: '',
    public: !p.credentialRef,
    models: p.models.join('\n'),
  })
  setEditingProvider(p.name)
}

async function removeProvider(p: TemplateProvider) {
  if (llmBusy) return
  // window.confirm: the `confirm` state in this view is the confirmation
  // posture, not the browser dialog.
  const n = p.models.length
  if (!window.confirm(`Remove provider "${p.name}" and its ${n} model${n === 1 ? '' : 's'}? Its credential is deleted too.`)) return
  setLLMBusy(true)
  try {
    const res = await api.deleteLLM(p.name)
    showToast(res.warning || `Provider "${p.name}" removed`)
    if (editingProvider === p.name) resetLLMForm()
    await loadTemplate()
  } catch (e) {
    // A provider any of whose models an instance selects is refused with the
    // instances named.
    showToast('Remove failed: ' + (e instanceof Error ? e.message : String(e)))
  } finally {
    setLLMBusy(false)
  }
}
```

Replace the card body at lines 405-458 with:

```tsx
{templateProviders.length === 0 && <div className="muted" style={{ fontSize: 13 }}>No providers yet.</div>}
{templateProviders.map((p) => (
  <div key={p.name} style={{ border: '1px solid var(--border)', borderRadius: 6, padding: 10, marginBottom: 10 }}>
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 }}>
      <span className="mono" style={{ fontSize: 13 }}>{p.name}</span>
      <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
        <span className="pill neutral">{p.credentialRef ? 'keyed' : 'public'}</span>
        <button className="btn sm ghost" disabled={llmBusy} onClick={() => startEditProvider(p)}>Edit</button>
        <button className="btn sm ghost" disabled={llmBusy} onClick={() => removeProvider(p)}>Remove provider</button>
      </span>
    </div>
    <div className="muted" style={{ fontSize: 12, margin: '4px 0 8px', wordBreak: 'break-all' }}>{p.endpoint}</div>
    {p.models.map((id) => (
      <div key={id} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8, fontSize: 13 }}>
        <span className="mono">{id}</span>
        <button
          className="btn sm ghost"
          disabled={llmBusy || p.models.length === 1}
          title={p.models.length === 1 ? 'A provider needs at least one model -- remove the provider instead' : undefined}
          onClick={() => removeModelFromProvider(p, id)}
        >Remove</button>
      </div>
    ))}
    <div style={{ display: 'flex', gap: 6, marginTop: 8 }}>
      <input
        className="input"
        placeholder="Model id (sent to the endpoint)"
        value={newModel[p.name] ?? ''}
        onChange={(e) => setNewModel((m) => ({ ...m, [p.name]: e.target.value }))}
        onKeyDown={(e) => { if (e.key === 'Enter') void addModelToProvider(p, newModel[p.name] ?? '') }}
      />
      <button className="btn sm" disabled={llmBusy} onClick={() => addModelToProvider(p, newModel[p.name] ?? '')}>Add model</button>
    </div>
  </div>
))}
```

`submitLLM` (lines 288-327) gains the model list. Its body becomes: for an edit, send `{ endpoint, apiKey: llmForm.apiKey || undefined, public: llmForm.public, models: splitModels(llmForm.models) }`; for an add, send `{ name, endpoint, apiKey: llmForm.apiKey || undefined, public: llmForm.public, models: splitModels(llmForm.models) }`. Add the parser and the add-time guard next to the handlers:

```ts
// splitModels reads the textarea/input as one id per line and drops blanks, so
// a trailing newline does not become an empty model id.
function splitModels(raw: string): string[] {
  return raw.split('\n').map((s) => s.trim()).filter(Boolean)
}
```

In the add branch only, refuse an empty list before the request:

```ts
    if (!editingProvider && splitModels(llmForm.models).length === 0) {
      showToast('Enter at least one model id')
      return
    }
```

The form's own inputs are unchanged except that the name field's placeholder becomes `Provider name (a short label, e.g. vllm)` -- it is no longer the model id -- and a `models` textarea/input is added next to the endpoint field, labelled one id per line.

The model select at lines 352-368 groups its options:

```tsx
<select className="input" aria-label="Select model" value={cfg.selectedModel || ''}
  onChange={(e) => setCfg((c) => ({ ...c, selectedModel: e.target.value }))}>
  <option value="" disabled>-- Select a model --</option>
  {templateProviders.map((p) => (
    <optgroup key={p.name} label={p.name}>
      {p.models.map((id) => {
        const ref = `${p.name}/${id}`
        return <option key={ref} value={ref}>{id}</option>
      })}
    </optgroup>
  ))}
</select>
```

- [ ] **Step 4: Update the API client**

`web/src/api/index.ts`: `addLLM` and `updateLLM` take the `models: string[]` field. `deleteLLM` keeps its path. No route changes.

- [ ] **Step 5: Build and check the copy rules**

Run: `make web`
Expected: `tsc -b` clean, vite build succeeds.

Then confirm the UI strings carry no internal references:

```bash
grep -nE '#[0-9]{2,}|FR-M|issue|M[0-9]' web/src/views/AgentView.tsx
```

Expected: no match in any user-visible string.

- [ ] **Step 6: Commit**

```bash
git add web/
git commit -s -m "$(cat <<'EOF'
feat(web): group models under their provider (issue #189)

The LLM card lists providers, each with the model ids its endpoint serves.
Adding an id is a one-field action on the card, and removing an id is distinct
from removing the provider -- only the latter takes the credential with it.

Assisted-by: Claude Code
EOF
)"
```

---

### Task 5: Documentation

**Files:**
- Modify: `README.md:122-128,150-175`
- Modify: `docs/cubepilot/cubepilot-design.md` (§3.3 at lines 161-168, §3.1 YAML at 106-131, §6 at 357-368)
- Modify: `deploy/charts/cubepilot/templates/rbac.yaml:49` (a comment naming `AgentTemplate.spec.models`)
- Modify: `deploy/charts/cubepilot/values.yaml:25-36` if the LLM values' comments describe the model entry shape

**Interfaces:**
- Consumes: the final shape from Tasks 1-4.
- Produces: nothing.

- [ ] **Step 1: Update the design doc**

`docs/cubepilot/cubepilot-design.md` §3.3 currently states the identity that this change breaks: "name is the catalog name, the selection key (`selectedModel`), the gateway provider key and the backend model id". Replace it with the two-level description: a provider owns the endpoint and the credential, a model id is what is sent to it, and a selection is the `<provider>/<modelId>` ref. Update the §3.1 YAML example to the `providers` shape.

- [ ] **Step 2: Update the README**

The "LLM providers (declarative)" section and the YAML at lines 122-128 show `spec.models`. Show `spec.providers` with a provider serving more than one id, since that is the case the old shape could not express.

- [ ] **Step 3: Sweep the remaining references**

Run: `grep -rn "spec\.models\|spec/models\|TemplateModelSpec\|llm-<model>\|EnvNameForModel" --include='*.md' --include='*.yaml' --include='*.go' . | grep -v node_modules | grep -v '^./deploy/charts/cubepilot/crds/'`

Expected after the earlier tasks: only historical documents under `docs/superpowers/specs/` and `docs/superpowers/plans/` (which record what was true when they were written and must not be rewritten) plus `test/e2e/framework/testdata/`. Fix anything else.

- [ ] **Step 4: Run the full local check**

Run: `make test && make web`
Expected: both pass.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -s -m "$(cat <<'EOF'
docs: describe LLM providers instead of one model per endpoint (issue #189)

Assisted-by: Claude Code
EOF
)"
```

---

### Task 6: Open the pull request

**Files:** none.

- [ ] **Step 1: Push and open the PR**

```bash
git push -u origin HEAD
gh pr create -R suanova/cubepilot --base main --head "zhujian7:feat/issue189-llm-provider-models" \
  --title "feat(api): model LLM providers with a list of model ids (issue #189)" \
  --body "Closes #189.

The design doc is at docs/superpowers/specs/2026-09-15-llm-provider-models-design.md
and lands in this PR."
```

The PR body must stand on its own for a reviewer who has not read the issue -- summarise the problem (an endpoint and a key serving N models could only be written as N entries with N Secrets, and an id containing \`/\` produced an invalid Secret name) and the change.

- [ ] **Step 2: Drive CI to green**

Run: `gh pr checks --watch`. On failure: `gh pr checks --log-failed`, fix in this worktree, commit, push.

- [ ] **Step 3: Answer every review comment**

Every inline comment gets a reply -- either \`Fixed in <sha>. Thanks.\` or an explicit reason for declining. Bot comments included. Then re-run:

```bash
n=$(gh pr view --json number -q .number)
gh api repos/suanova/cubepilot/pulls/$n/comments --jq '.[] | "\(.id)\t\(.path):\(.line)\n  \(.body)\n"'
```

Mergify auto-merges approved + green PRs (squash); do not merge by hand.

- [ ] **Step 4: Clean up after the merge**

Run from the main clone, not this worktree:

```bash
git worktree remove .claude/worktrees/feat-issue189-llm-provider-models && git branch -D feat/issue189-llm-provider-models
git fetch upstream && git checkout main && git rebase upstream/main && git push origin main
```
