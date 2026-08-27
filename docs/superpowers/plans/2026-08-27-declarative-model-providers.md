# Declarative model providers (issue #6) + portal LLM management — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generate the gateway `openclaw.json` declaratively from `AgentTemplate` models; retire `CUBEPILOT_MODEL_PROVIDERS`; let the platform admin add an LLM from the Portal.

**Architecture:** `TemplateModelSpec` becomes `{name, endpoint (required), credentialRef? (LocalObjectReference)}`. A new `internal/gateway` package renders `openclaw.json` from template models + credential Secrets and owns the gateway token (`EnsureGatewayToken`). A new operator controller reconciles the `openclaw-config` Secret. The API exposes `POST /api/llms` and the web adds an "LLM 配置" panel. The builtin template carries one working model referencing the `cubepilot-llm` Secret created by setup.sh.

**Tech Stack:** Go (controller-runtime), `controller-gen` (at `/home/zhujian/go/bin/controller-gen`), Helm, React (web), bash/jq.

## Global Constraints

- Work on branch `feat/issue6-declarative-model-providers` (already checked out). Commits use `git commit -s` and end with `Assisted-by: Claude Code`.
- Comments/naming in English.
- k8s API conventions: Secret refs are `corev1.LocalObjectReference` (never bare strings); JSON camelCase; no `provider`/`modelId` fields on `TemplateModelSpec`.
- Every template model must have a non-empty `endpoint`; `credentialRef` is optional (public models omit it).
- CRD copies at `config/crd/bases/` and `deploy/charts/cubepilot/crds/` must stay identical (sync after regen).
- `controller-gen` binary: `/home/zhujian/go/bin/controller-gen`.

---

### Task 1: Simplify TemplateModelSpec + builtin template + resolver override ref

**Files:**
- Modify: `internal/api/v1alpha1/agenttemplate_types.go`
- Modify: `internal/controller/builtin.go`
- Modify: `internal/resolver/resolver.go`
- Modify: `internal/api/v1alpha1/agenttemplate_types_test.go`
- Modify: `internal/controller/builtin_test.go`
- Modify: `internal/resolver/resolver_test.go`
- Modify: `internal/instances/manager_test.go`
- Modify: `internal/server/internal_api_test.go`
- Modify: `docs/cubepilot/api.md`

**Interfaces:**
- Produces: `TemplateModelSpec{Name string, Endpoint string, CredentialRef corev1.LocalObjectReference}` (no `Provider`, no `ModelID`); `BuiltinModels() []v1alpha1.TemplateModelSpec` (single entry); `Resolver.resolveModel(selected, def) (string, error)` returns `<name>/<name>` for an explicit selection.

- [ ] **Step 1: Change the type.**

In `internal/api/v1alpha1/agenttemplate_types.go`:

- Remove the `ModelProvider` type and the `ModelProviderPlatform` / `ModelProviderExternal` constants.
- Replace `TemplateModelSpec` with:

```go
// TemplateModelSpec is one entry of the inline model list of an AgentTemplate
// (design §3.3: models are inlined -- no standalone Model CRD). Name is the
// catalog name, the selection key (selectedModel), the gateway provider key
// and the backend model id sent to the endpoint. Every model is a concrete
// OpenAI-compatible endpoint; a public model simply omits CredentialRef.
type TemplateModelSpec struct {
	// Name is the model name: the key for selectedModel on instances, the
	// gateway provider key, and the model id passed to the LLM endpoint.
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL; required for every model.
	Endpoint string `json:"endpoint"`
	// CredentialRef optionally references a platform-managed Secret (name)
	// holding the apiKey; public models omit it. References only -- never the
	// key itself (design §4.4).
	// +optional
	CredentialRef corev1.LocalObjectReference `json:"credentialRef,omitempty"`
}

// Validate enforces the inline-model invariants (design §3.3): every model
// needs an endpoint; a present credentialRef must carry a name. The same rules
// are enforced on the API server by the CEL XValidations on Models.
func (m TemplateModelSpec) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("model name is required")
	}
	if m.Endpoint == "" {
		return fmt.Errorf("model %q requires an endpoint", m.Name)
	}
	return nil
}
```

- Update the `Models` field comment and its CEL markers in `AgentTemplateSpec`:

```go
	// Models is the inline model list (design §3.3: models are inlined in the
	// template -- no standalone Model CRD). The first entry is the primary;
	// instances select within this list. Every model requires an endpoint;
	// credentialRef is optional (a public model has none).
	// +kubebuilder:validation:XValidation:rule="self.all(m, has(m.endpoint))",message="every model requires an endpoint"
	// +kubebuilder:validation:XValidation:rule="self.all(m, !has(m.credentialRef) || has(m.credentialRef.name))",message="credentialRef must reference a Secret name"
	// +optional
	Models []TemplateModelSpec `json:"models,omitempty"`
```

- Add `corev1 "k8s.io/api/core/v1"` to the imports.

- [ ] **Step 2: Update the builtin template.**

In `internal/controller/builtin.go`, replace `BuiltinModels()`:

```go
// BuiltinModels returns the preset inline model entries for the builtin
// AgentTemplate (design §3.3: models are inlined in the template -- no
// standalone Model CRD). The platform default model references the
// cubepilot-llm credential Secret created by setup.sh; its endpoint can be
// edited on the CR after install.
func BuiltinModels() []v1alpha1.TemplateModelSpec {
	return []v1alpha1.TemplateModelSpec{
		{
			Name:          "deepseek-v4-flash",
			Endpoint:      "https://api.deepseek.com",
			CredentialRef: corev1.LocalObjectReference{Name: "cubepilot-llm"},
		},
	}
}
```

Add `corev1 "k8s.io/api/core/v1"` to the imports. `DefaultModel` stays `"deepseek-v4-flash"`.

- [ ] **Step 3: Update the resolver override ref.**

In `internal/resolver/resolver.go`, replace `resolveModel`:

```go
// resolveModel validates the selection against the template's inline models
// list and returns the effective override ref. Fail-closed: not in models
// list -> error. The returned ref is the full gateway ref `<name>/<name>` --
// the same string the gateway renderer puts in the allowlist, so the override
// always matches (the coupling issue #6 removes).
func (r *Resolver) resolveModel(selected string, def v1alpha1.AgentTemplate) (string, error) {
	for _, m := range def.Spec.Models {
		if m.Name == selected {
			return m.Name + "/" + m.Name, nil
		}
	}
	return "", fmt.Errorf("model %q not in template %q models", selected, def.Name)
}
```

- [ ] **Step 4: Update test fixtures for the new shape (mechanical).**

Every `TemplateModelSpec` literal that sets `Provider:` and/or `ModelID:` must drop those fields. The old builtin-style literal:

```go
{Name: "deepseek-v4-flash", Provider: v1alpha1.ModelProviderPlatform, ModelID: "cuberouter/deepseek-v4-flash-0731"},
```

becomes:

```go
{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com", CredentialRef: corev1.LocalObjectReference{Name: "cubepilot-llm"}},
```

Apply to:
- `internal/api/v1alpha1/agenttemplate_types_test.go` (line ~20 and the invalid-combination cases around lines 69-95: drop `provider`, drop the `no-endpoint`/`no-cred` External cases; replace with "missing endpoint" and "credentialRef without name").
- `internal/resolver/resolver_test.go` (fixtures at lines ~88, ~139, ~161).
- `internal/instances/manager_test.go` (fixtures at lines ~60, ~83, ~107).
- `internal/server/internal_api_test.go` (fixture at lines ~18-20).

Where a test currently asserts `cfg.SelectedModel` equals the old full ref, update to the composed `<name>/<name>` (e.g. selecting `deepseek-v4-flash` → `SelectedModel == "deepseek-v4-flash/deepseek-v4-flash"`).

- [ ] **Step 5: Update the builtin shape test.**

In `internal/controller/builtin_test.go`, replace the models assertions in `TestBuiltinAgentShape`:

```go
	if len(agent.Spec.Models) != 1 || agent.Spec.Models[0].Name != "deepseek-v4-flash" {
		t.Errorf("inline models = %v, want [deepseek-v4-flash]", agent.Spec.Models)
	}
	if agent.Spec.Models[0].Endpoint == "" {
		t.Errorf("builtin model missing endpoint: %+v", agent.Spec.Models[0])
	}
	if agent.Spec.Models[0].CredentialRef.Name != "cubepilot-llm" {
		t.Errorf("builtin model credentialRef = %q, want cubepilot-llm", agent.Spec.Models[0].CredentialRef.Name)
	}
```

(Remove the old Models[1] External check and the empty-ModelID regression check.)

- [ ] **Step 6: Update Validate tests.**

In `internal/api/v1alpha1/agenttemplate_types_test.go`, replace the "external requires endpoint + credentialRef" cases with:

```go
	if err := (TemplateModelSpec{Name: "x"}).Validate(); err == nil {
		t.Error("model without endpoint should be rejected")
	}
	if err := (TemplateModelSpec{Name: "x", Endpoint: "https://x"}).Validate(); err != nil {
		t.Errorf("valid model rejected: %v", err)
	}
```

- [ ] **Step 7: Regenerate CRDs and sync the chart copy.**

Run:

```bash
cd /home/zhujian/code/github.com/suanova/cubepilot
/home/zhujian/go/bin/controller-gen object:headerFile=hack/boilerplate.go.txt paths="./internal/api/v1alpha1/..."
/home/zhujian/go/bin/controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases
cp config/crd/bases/ai.cubestack.io_agenttemplates.yaml deploy/charts/cubepilot/crds/
```

Expected: `git diff config/crd/bases/ai.cubestack.io_agenttemplates.yaml` touches only the `models` schema (removes `provider`/`modelId`, makes `endpoint` required, `credentialRef` becomes an object, the two XValidations change) and the `Models` description. If controller-gen reformats unrelated sections or other CRD files, restore them with `git checkout -- <file>` and hand-edit the two agenttemplates CRD copies to match (keep them identical).

- [ ] **Step 8: Update `docs/cubepilot/api.md` sample.**

In the AgentTemplate sample (around lines 188-192), replace:

```json
    "models": [
      { "name": "deepseek-v4-flash", "provider": "Platform" },
      { "name": "qwen2.5-72b", "provider": "External", "endpoint": "https://api.example.com/v1", "credentialRef": { "name": "cred-llm-org", "namespace": "cubepilot" } }
    ],
```

with:

```json
    "models": [
      { "name": "deepseek-v4-flash", "endpoint": "https://api.deepseek.com", "credentialRef": { "name": "cubepilot-llm" } }
    ],
```

- [ ] **Step 9: Build and test.**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 10: Commit.**

```bash
git add internal/api/v1alpha1/agenttemplate_types.go internal/controller/builtin.go internal/resolver/resolver.go \
  internal/api/v1alpha1/agenttemplate_types_test.go internal/controller/builtin_test.go internal/resolver/resolver_test.go \
  internal/instances/manager_test.go internal/server/internal_api_test.go \
  config/crd/bases/ai.cubestack.io_agenttemplates.yaml deploy/charts/cubepilot/crds/ai.cubestack.io_agenttemplates.yaml docs/cubepilot/api.md
git commit -s -m "$(cat <<'EOF'
feat(api): simplify TemplateModelSpec — drop provider/modelId, endpoint required

TemplateModelSpec is now {name, endpoint (required), credentialRef?}. Every
model is a concrete OpenAI-compatible endpoint; credentialRef (LocalObjectReference)
is optional for public models. The resolver sends the full gateway ref
<name>/<name> for an explicit selection so it always matches the allowlist the
gateway renderer generates (issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

### Task 2: internal/gateway renderer + EnsureGatewayToken

**Files:**
- Create: `internal/gateway/render.go`
- Create: `internal/gateway/render_test.go`
- Create: `internal/gateway/token.go`
- Create: `internal/gateway/token_test.go`

**Interfaces:**
- Consumes: nothing (pure stdlib + controller-runtime client).
- Produces: `gateway.Provider{Key, BaseURL, APIKey, Model string}`; `gateway.Render(token, primary string, providers []Provider) ([]byte, error)`; `gateway.EnsureGatewayToken(ctx, cl client.Client, ns string) (string, error)`.

- [ ] **Step 1: Write the renderer test (failing).**

Create `internal/gateway/render_test.go`:

```go
package gateway

import (
	"encoding/json"
	"testing"
)

func TestRender(t *testing.T) {
	providers := []Provider{
		{Key: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com", APIKey: "sk-1", Model: "deepseek-v4-flash"},
		{Key: "qwen", BaseURL: "http://localhost:11434/v1", Model: "qwen2.5-72b"}, // public, no key
	}
	b, err := Render("tok", "deepseek-v4-flash/deepseek-v4-flash", providers)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Models struct {
			Providers map[string]struct {
				API    string   `json:"api"`
				APIKey string   `json:"apiKey"`
				Models []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
		Agents struct {
			Defaults struct {
				Model struct {
					Primary string          `json:"primary"`
					Models  map[string]any  `json:"models"`
				} `json:"model"`
			} `json:"defaults"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := cfg.Models.Providers["deepseek-v4-flash"]
	if d.API != "openai-completions" || d.APIKey != "sk-1" || d.Models[0].ID != "deepseek-v4-flash" {
		t.Errorf("deepseek provider wrong: %+v", d)
	}
	q := cfg.Models.Providers["qwen"]
	if q.APIKey != "" || q.Models[0].ID != "qwen2.5-72b" {
		t.Errorf("public provider should be keyless: %+v", q)
	}
	if cfg.Agents.Defaults.Model.Primary != "deepseek-v4-flash/deepseek-v4-flash" {
		t.Errorf("primary = %q", cfg.Agents.Defaults.Model.Primary)
	}
	if _, ok := cfg.Agents.Defaults.Model.Models["deepseek-v4-flash/deepseek-v4-flash"]; !ok {
		t.Error("allowlist missing primary ref")
	}
	if _, ok := cfg.Agents.Defaults.Model.Models["qwen/qwen2.5-72b"]; !ok {
		t.Error("allowlist missing public ref")
	}
}

func TestRenderEmpty(t *testing.T) {
	b, err := Render("tok", "", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty render")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL (no such package).**

Run: `go test ./internal/gateway/...`
Expected: FAIL "no such file or directory / package not found".

- [ ] **Step 3: Implement the renderer.**

Create `internal/gateway/render.go`:

```go
// Package gateway renders the shared openclaw.json gateway config from
// AgentTemplate model declarations (design §3.3 / issue #6). It replaces the
// install-time deploy/openclaw-config.jq renderer.
package gateway

import (
	"encoding/json"
	"fmt"
)

// Provider is one OpenClaw models.providers entry derived from a template model.
type Provider struct {
	Key     string // provider key = model Name
	BaseURL string // endpoint
	APIKey  string // from the credentialRef Secret when present; "" for public models
	Model   string // model name = backend id
}

// Render builds the openclaw.json bytes for the given providers. Static
// scaffolding (workspace, sandbox, gateway port/auth, tools, sessions) mirrors
// the defaults the old jq renderer used.
func Render(token, primary string, providers []Provider) ([]byte, error) {
	providersOut := map[string]any{}
	modelsOut := map[string]any{}
	for _, p := range providers {
		pv := map[string]any{
			"api":     "openai-completions",
			"baseUrl": p.BaseURL,
			"models":  []map[string]string{{"id": p.Model, "name": p.Model}},
		}
		if p.APIKey != "" {
			pv["apiKey"] = p.APIKey
		}
		providersOut[p.Key] = pv
		modelsOut[p.Key+"/"+p.Model] = map[string]string{"alias": p.Model}
	}
	cfg := map[string]any{
		"models": map[string]any{"providers": providersOut},
		"agents": map[string]any{
			"defaults": map[string]any{
				"workspace": "/home/node/.openclaw/workspace",
				"model": map[string]any{
					"primary": primary,
					"models":  modelsOut,
				},
				"sandbox": map[string]any{"mode": "off"},
			},
		},
		"gateway": map[string]any{
			"mode": "local",
			"port": 18789,
			"bind": "lan",
			"auth": map[string]any{"mode": "token", "token": token},
			"http": map[string]any{"endpoints": map[string]any{"chatCompletions": map[string]any{"enabled": true}}},
		},
		"tools": map[string]any{
			"exec":     map[string]any{"security": "full", "ask": "off"},
			"sessions": map[string]any{"visibility": "all"},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render openclaw.json: %w", err)
	}
	return b, nil
}
```

- [ ] **Step 4: Write the token test.**

Create `internal/gateway/token_test.go`:

```go
package gateway

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureGatewayToken(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	tok, err := EnsureGatewayToken(ctx, cl, "cubepilot")
	if err != nil {
		t.Fatalf("EnsureGatewayToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}

	// Second call reuses the persisted token (never regenerates).
	tok2, err := EnsureGatewayToken(ctx, cl, "cubepilot")
	if err != nil {
		t.Fatalf("EnsureGatewayToken #2: %v", err)
	}
	if tok2 != tok {
		t.Errorf("token changed: %q != %q", tok2, tok)
	}

	// A pre-existing token is preserved.
	sec := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-config", Namespace: "cubepilot"}}
	sec.Data = map[string][]byte{"gatewayToken": []byte("pre-existing")}
	if err := cl.Update(ctx, &sec); err != nil {
		t.Fatalf("update: %v", err)
	}
	tok3, err := EnsureGatewayToken(ctx, cl, "cubepilot")
	if err != nil {
		t.Fatalf("EnsureGatewayToken #3: %v", err)
	}
	if tok3 != "pre-existing" {
		t.Errorf("did not reuse existing token: %q", tok3)
	}
}
```

(Add `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` to the imports.)

- [ ] **Step 5: Implement EnsureGatewayToken.**

Create `internal/gateway/token.go`:

```go
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/k8s"
)

// EnsureGatewayToken reads the gatewayToken from the openclaw-config Secret
// (k8s.ConfigSecretName), generating and persisting one if absent. Idempotent
// and concurrency-safe: callers race to create; the loser re-reads the
// winner's token.
func EnsureGatewayToken(ctx context.Context, cl client.Client, ns string) (string, error) {
	key := types.NamespacedName{Namespace: ns, Name: k8s.ConfigSecretName}
	var sec corev1.Secret
	if err := cl.Get(ctx, key, &sec); err == nil {
		if tok := string(sec.Data["gatewayToken"]); tok != "" {
			return tok, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName, Namespace: ns},
		Data:       map[string][]byte{"gatewayToken": []byte(token)},
	}
	if err := cl.Create(ctx, &sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := cl.Get(ctx, key, &sec); err != nil {
				return "", err
			}
			return string(sec.Data["gatewayToken"]), nil
		}
		return "", err
	}
	return token, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
```

- [ ] **Step 6: Run tests.**

Run: `go test ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 7: Commit.**

```bash
git add internal/gateway/
git commit -s -m "$(cat <<'EOF'
feat(gateway): render openclaw.json from model declarations + own the gateway token

New internal/gateway package: Render() builds the allowlist openclaw.json from
template models (replacing deploy/openclaw-config.jq), and EnsureGatewayToken()
generates/persists the gateway token once in the openclaw-config Secret
(issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

### Task 3: OpenClawConfigReconciler + operator wiring

**Files:**
- Create: `internal/controller/openclawconfig_controller.go`
- Create: `internal/controller/openclawconfig_controller_test.go`
- Modify: `cmd/cubepilot-operator/main.go`

**Interfaces:**
- Consumes: `gateway.Render`, `gateway.EnsureGatewayToken`, `k8s.ConfigSecretName`.
- Produces: `controller.OpenClawConfigReconciler{Client, Scheme, Cfg}` with `SetupWithManager(mgr)`.

- [ ] **Step 1: Write the reconciler test (failing).**

Create `internal/controller/openclawconfig_controller_test.go`:

```go
package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

func TestOpenClawConfigReconcile(t *testing.T) {
	scheme := testScheme(t)
	builtin := BuiltinAgentTemplate()
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-llm", Namespace: "cubepilot"},
		StringData: map[string]string{"apiKey": "sk-real"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(builtin, cred).Build()
	r := &OpenClawConfigReconciler{Client: cl, Scheme: scheme, Cfg: config.Config{Namespace: "cubepilot"}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sec corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: k8s.ConfigSecretName}, &sec); err != nil {
		t.Fatalf("openclaw-config not created: %v", err)
	}
	jsonData := string(sec.Data["openclaw.json"])
	if !strings.Contains(jsonData, `"deepseek-v4-flash/deepseek-v4-flash"`) {
		t.Errorf("openclaw.json missing primary ref: %s", jsonData)
	}
	if !strings.Contains(jsonData, `"apiKey": "sk-real"`) {
		t.Errorf("openclaw.json missing apiKey: %s", jsonData)
	}
	if tok := string(sec.Data["gatewayToken"]); len(tok) != 64 {
		t.Errorf("gatewayToken length = %d, want 64", len(tok))
	}
}

func TestOpenClawConfigReconcileSkipsMissingCredential(t *testing.T) {
	scheme := testScheme(t)
	builtin := BuiltinAgentTemplate() // references cubepilot-llm, which is absent
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(builtin).Build()
	r := &OpenClawConfigReconciler{Client: cl, Scheme: scheme, Cfg: config.Config{Namespace: "cubepilot"}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Missing credential: the Secret is still created (no providers), and no
	// error is returned -- the controller keeps requeueing.
	var sec corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: k8s.ConfigSecretName}, &sec); err != nil {
		t.Fatalf("openclaw-config not created: %v", err)
	}
	if strings.Contains(string(sec.Data["openclaw.json"]), "deepseek-v4-flash") {
		t.Errorf("model with missing credential should be skipped")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL (reconciler not defined).**

Run: `go test ./internal/controller/...`
Expected: FAIL "undefined: OpenClawConfigReconciler".

- [ ] **Step 3: Implement the reconciler.**

Create `internal/controller/openclawconfig_controller.go`:

```go
package controller

import (
	"context"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/gateway"
	"github.com/suanova/cubepilot/internal/k8s"
)

// OpenClawConfigReconciler renders the shared openclaw.json from the
// AgentTemplate inline models (+ referenced credential Secrets) and reconciles
// it into the openclaw-config Secret, preserving the gateway token (issue #6).
type OpenClawConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
}

// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agenttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile renders and reconciles the openclaw-config Secret.
func (r *OpenClawConfigReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	var tpls v1alpha1.AgentTemplateList
	if err := r.List(ctx, &tpls); err != nil {
		return ctrl.Result{}, err
	}
	var providers []gateway.Provider
	var primary string
	for i := range tpls.Items {
		t := &tpls.Items[i]
		for _, m := range t.Spec.Models {
			if m.Endpoint == "" {
				continue
			}
			p := gateway.Provider{Key: m.Name, BaseURL: m.Endpoint, Model: m.Name}
			if m.CredentialRef.Name != "" {
				var sec corev1.Secret
				if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: m.CredentialRef.Name}, &sec); err != nil {
					log.Printf("openclaw-config: model %q credential %q not ready (%v), skipping", m.Name, m.CredentialRef.Name, err)
					continue
				}
				p.APIKey = string(sec.Data["apiKey"])
			}
			if t.Spec.DefaultModel == m.Name && primary == "" {
				primary = m.Name + "/" + m.Name
			}
			providers = append(providers, p)
		}
	}
	if primary == "" && len(providers) > 0 {
		primary = providers[0].Key + "/" + providers[0].Model
	}

	token, err := gateway.EnsureGatewayToken(ctx, r.Client, r.Cfg.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	jsonBytes, err := gateway.Render(token, primary, providers)
	if err != nil {
		return ctrl.Result{}, err
	}

	key := types.NamespacedName{Namespace: r.Cfg.Namespace, Name: k8s.ConfigSecretName}
	var sec corev1.Secret
	err = r.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName, Namespace: r.Cfg.Namespace},
			Data:       map[string][]byte{"gatewayToken": []byte(token), "openclaw.json": jsonBytes},
		}
		return ctrl.Result{RequeueAfter: time.Minute}, r.Create(ctx, &sec)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if string(sec.Data["openclaw.json"]) != string(jsonBytes) {
		sec.Data["openclaw.json"] = jsonBytes
		if err := r.Update(ctx, &sec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// SetupWithManager registers the reconciler on AgentTemplate + Secret events.
func (r *OpenClawConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("openclaw-config").
		For(&v1alpha1.AgentTemplate{}).
		Watches(&corev1.Secret{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
```

- [ ] **Step 4: Wire the reconciler + token bootstrap into the operator.**

In `cmd/cubepilot-operator/main.go`:

- Add imports: `"sigs.k8s.io/controller-runtime/pkg/client"` and `"github.com/suanova/cubepilot/internal/gateway"`.
- Just before `mgrInstances := instances.New(mgr.GetClient(), cfg)` (currently at line 87), insert:

```go
	// Ensure the gateway token exists before building the Runner: the operator
	// generates it once and persists it in the openclaw-config Secret (the API
	// process reads the same token). A direct client is used because the
	// manager's cache is not up before Start.
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("direct client: %v", err)
	}
	token, err := gateway.EnsureGatewayToken(ctx, direct, cfg.Namespace)
	if err != nil {
		log.Fatalf("ensure gateway token: %v", err)
	}
	cfg.GatewayToken = token
```

- After the `BuiltinBootstrapReconciler` setup block, register the new controller:

```go
	if err := (&controller.OpenClawConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("openclaw-config controller: %v", err)
	}
```

- [ ] **Step 5: Run tests.**

Run: `go build ./... && go test ./internal/controller/... ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/controller/openclawconfig_controller.go internal/controller/openclawconfig_controller_test.go cmd/cubepilot-operator/main.go
git commit -s -m "$(cat <<'EOF'
feat(operator): OpenClawConfigReconciler renders the gateway config

New reconciler watches AgentTemplates + Secrets and reconciles the
openclaw-config Secret (providers + allowlist + primary from template models,
preserving the gateway token). Operator main bootstraps the token via
EnsureGatewayToken before building the Runner (issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

### Task 4: API POST /api/llms + token bootstrap

**Files:**
- Create: `internal/server/handlers_llms.go`
- Create: `internal/server/handlers_llms_test.go`
- Modify: `internal/server/server.go`
- Modify: `cmd/cubepilot-api/main.go`

**Interfaces:**
- Consumes: `gateway.EnsureGatewayToken`, `k8s.Sanitize`, `s.cr` (client), `s.cfg.Namespace`.
- Produces: `POST /api/llms` body `{name, endpoint, apiKey?}` -> `{model}`.

- [ ] **Step 1: Write the handler test (failing).**

Create `internal/server/handlers_llms_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/controller"
)

func addLLMTestServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Server{cfg: config.Config{Namespace: "cubepilot"}, cr: cl}
}

func TestHandleAddLLM(t *testing.T) {
	s := addLLMTestServer(t, controller.BuiltinAgentTemplate())

	body := bytes.NewBufferString(`{"name":"My Qwen","endpoint":"https://api.example.com/v1","apiKey":"sk-2"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// Model appended to the builtin template.
	var tmpl v1alpha1.AgentTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if len(tmpl.Spec.Models) != 2 || tmpl.Spec.Models[1].Name != "my-qwen" {
		t.Fatalf("models = %+v", tmpl.Spec.Models)
	}
	if tmpl.Spec.Models[1].CredentialRef.Name != "llm-my-qwen" {
		t.Errorf("credentialRef = %q", tmpl.Spec.Models[1].CredentialRef.Name)
	}
	// Credential Secret created.
	var sec corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	if string(sec.Data["apiKey"]) != "sk-2" {
		t.Errorf("apiKey = %q", sec.Data["apiKey"])
	}
}

func TestHandleAddLLMPublicNoKey(t *testing.T) {
	s := addLLMTestServer(t, controller.BuiltinAgentTemplate())

	body := bytes.NewBufferString(`{"name":"local-ollama","endpoint":"http://localhost:11434/v1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	m := tmpl.Spec.Models[len(tmpl.Spec.Models)-1]
	if m.CredentialRef.Name != "" {
		t.Errorf("public model should carry no credentialRef: %+v", m)
	}
	// No Secret created for a public model.
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-local-ollama"}, &corev1.Secret{}); err == nil {
		t.Error("public model should not create a Secret")
	}
}
```

- [ ] **Step 2: Run it — expect FAIL (handler not defined).**

Run: `go test ./internal/server/...`
Expected: FAIL "undefined: s.handleAddLLM".

- [ ] **Step 3: Implement the handler.**

Create `internal/server/handlers_llms.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// handleAddLLM serves POST /api/llms -- the platform admin adds an LLM by
// giving a name, an OpenAI-compatible endpoint and (for non-public models) an
// apiKey. The handler appends an External model to the builtin AgentTemplate
// and creates a credential Secret when keyed; the operator renders it into the
// gateway config (issue #6). No credentials are ever stored in the CR.
func (s *Server) handleAddLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	var body struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		APIKey   string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
		return
	}
	name := k8s.Sanitize(strings.TrimSpace(body.Name))
	endpoint := strings.TrimSpace(body.Endpoint)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "endpoint must be a valid URL"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "k8s client unavailable"})
		return
	}

	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("builtin template: %v", err)})
		return
	}
	for _, m := range tmpl.Spec.Models {
		if m.Name == name {
			writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("model %q already exists", name)})
			return
		}
	}

	model := v1alpha1.TemplateModelSpec{Name: name, Endpoint: endpoint}
	if body.APIKey != "" {
		secretName := "llm-" + name
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: s.cfg.Namespace, Name: secretName},
			StringData: map[string]string{"apiKey": body.APIKey},
		}
		if err := s.cr.Create(r.Context(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("create credential Secret: %v", err)})
			return
		}
		model.CredentialRef = corev1.LocalObjectReference{Name: secretName}
	}

	tmpl.Spec.Models = append(tmpl.Spec.Models, model)
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model})
}
```

- [ ] **Step 4: Register the route + bootstrap the token in api main.**

In `internal/server/server.go`, add to `Handler()`:

```go
	mux.HandleFunc("/api/llms", s.handleAddLLM)
```

In `cmd/cubepilot-api/main.go`:

- Add import `"github.com/suanova/cubepilot/internal/gateway"`.
- After `ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)` (and its `defer stop()`), insert:

```go
	// Ensure the gateway token before the server is built: generated once and
	// persisted in the openclaw-config Secret (the operator does the same, so
	// both processes always agree).
	token, err := gateway.EnsureGatewayToken(ctx, cr, cfg.Namespace)
	if err != nil {
		log.Fatalf("ensure gateway token: %v", err)
	}
	cfg.GatewayToken = token
```

- [ ] **Step 5: Run tests.**

Run: `go build ./... && go test ./internal/server/...`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/server/handlers_llms.go internal/server/handlers_llms_test.go internal/server/server.go cmd/cubepilot-api/main.go
git commit -s -m "$(cat <<'EOF'
feat(api): POST /api/llms adds an LLM to the platform catalog

The handler appends a model (name + endpoint, optional apiKey) to the builtin
agent-for-cloud template and creates a credential Secret when keyed; the
operator renders it into the gateway. The API process also bootstraps the
gateway token via EnsureGatewayToken (issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

### Task 5: Web LLM panel

**Files:**
- Modify: `web/src/api/index.ts`
- Modify: `web/src/views/AgentView.tsx`

**Interfaces:**
- Consumes: `POST /api/llms` -> `{model}`.
- Produces: `api.addLLM(body)` and an "LLM 配置" card in the Agent view.

- [ ] **Step 1: Add the API function.**

In `web/src/api/index.ts`, inside the `api` object, after `listAgentTemplates`:

```ts
  addLLM: (body: { name: string; endpoint: string; apiKey?: string }) =>
    apiFetch<{ model: PlatformObject }>('/api/llms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.model),
```

- [ ] **Step 2: Update the template-model mapping in AgentView.**

In `web/src/views/AgentView.tsx`:

- Replace the `TemplateModel` interface:

```ts
interface TemplateModel {
  name: string
  endpoint: string
  credentialRef?: { name: string }
}
```

- Replace the mapping in `loadTemplate`:

```ts
      const models: TemplateModel[] = ((tmpl.spec?.models || []) as Array<Record<string, string | { name: string }>>).map((m) => ({
        name: String(m.name ?? ''),
        endpoint: String(m.endpoint ?? ''),
        credentialRef: (m.credentialRef as { name: string } | undefined) ?? undefined,
      }))
```

- Replace the model-picker `<option>` label to drop the `(External)/(Platform)` suffix:

```tsx
                  {templateModels.map((m) => (
                    <option key={m.name} value={m.name}>
                      {m.name}
                    </option>
                  ))}
```

- [ ] **Step 3: Add the "LLM 配置" card.**

Add a new card at the end of the left column (after the System Prompt card, before the closing `</div>` of the left column). Add the state and handler near the top of the component:

```tsx
  const [llmForm, setLLMForm] = useState({ name: '', endpoint: '', apiKey: '' })
  const [adding, setAdding] = useState(false)

  async function addLLM() {
    if (adding) return
    if (!llmForm.name.trim() || !llmForm.endpoint.trim()) {
      showToast('Name and endpoint are required')
      return
    }
    setAdding(true)
    try {
      await api.addLLM({ name: llmForm.name, endpoint: llmForm.endpoint, apiKey: llmForm.apiKey || undefined })
      showToast('LLM added - the operator will wire it into the gateway')
      setLLMForm({ name: '', endpoint: '', apiKey: '' })
      await loadTemplate()
    } catch (e) {
      showToast('Add LLM failed: ' + e)
    } finally {
      setAdding(false)
    }
  }
```

And the card JSX (place inside the left column after the System Prompt card):

```tsx
          <div className="card">
            <div className="card-head">
              <span className="card-title">LLM 配置</span>
              <span className="card-hint">Add an OpenAI-compatible model to the platform catalog</span>
            </div>
            <div className="card-pad">
              <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 12 }}>
                {templateModels.length === 0 && <div className="muted" style={{ fontSize: 13 }}>No models yet.</div>}
                {templateModels.map((m) => (
                  <div key={m.name} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', fontSize: 13 }}>
                    <span className="mono">{m.name}</span>
                    <span className="pill neutral">{m.credentialRef ? 'keyed' : 'public'}</span>
                  </div>
                ))}
              </div>
              <input
                className="input"
                placeholder="Model name (sent to the endpoint)"
                value={llmForm.name}
                onChange={(e) => setLLMForm((f) => ({ ...f, name: e.target.value }))}
              />
              <input
                className="input"
                placeholder="Endpoint (OpenAI-compatible base URL)"
                value={llmForm.endpoint}
                onChange={(e) => setLLMForm((f) => ({ ...f, endpoint: e.target.value }))}
              />
              <input
                className="input"
                type="password"
                placeholder="apiKey (leave empty for public models)"
                value={llmForm.apiKey}
                onChange={(e) => setLLMForm((f) => ({ ...f, apiKey: e.target.value }))}
              />
              <button className="btn primary" style={{ width: '100%' }} disabled={adding} onClick={addLLM}>
                {adding ? 'Adding...' : 'Add LLM'}
              </button>
            </div>
          </div>
```

- [ ] **Step 4: Type-check the web build.**

Run: `cd web && npm run build`
Expected: PASS (vue-tsc type-check + vite build).

- [ ] **Step 5: Commit.**

```bash
git add web/src/api/index.ts web/src/views/AgentView.tsx
git commit -s -m "$(cat <<'EOF'
feat(web): LLM configuration panel (add an OpenAI-compatible model)

Adds api.addLLM and an "LLM 配置" card in the Agent view listing catalog
models (keyed/public) with an add form. Drops the obsolete provider/modelId
labels from the model picker (issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

### Task 6: setup.sh / e2e / CI / chart

**Files:**
- Modify: `scripts/setup.sh`
- Modify: `scripts/e2e.sh`
- Modify: `.github/workflows/e2e.yaml`
- Modify: `deploy/charts/cubepilot/templates/_helpers.tpl`
- Modify: `deploy/charts/cubepilot/templates/rbac.yaml`
- Delete: `deploy/openclaw-config.jq`
- Delete: `scripts/test-openclaw-config.sh`
- Modify: `Makefile` (if it references the deleted script — it does not; leave as is)

**Interfaces:**
- Consumes: `cubepilot-llm` Secret (apiKey) + operator-generated `openclaw-config`.

- [ ] **Step 1: Rewrite setup.sh inputs and Secret creation.**

In `scripts/setup.sh`:

- Remove the `MODEL_PROVIDERS` line (`MODEL_PROVIDERS="${CUBEPILOT_MODEL_PROVIDERS:-}"`), the `--providers-json` case, the required-check block, and the jq render block.
- Add:

```bash
LLM_APIKEY="${CUBEPILOT_LLM_APIKEY:-}"
...
    --llm-apikey)        LLM_APIKEY="${2:?--llm-apikey requires a value}"; shift 2 ;;
```

- Replace the `[ -n "$MODEL_PROVIDERS" ]` validation with:

```bash
[ -n "$LLM_APIKEY" ] || {
  echo "error: CUBEPILOT_LLM_APIKEY is required (the platform default LLM apiKey). See --help." >&2
  exit 1
}
```

- In the "creating shared secrets" section, after `agent-kubeconfig`, add:

```bash
kubectl -n "$NAMESPACE" create secret generic cubepilot-llm \
  --from-literal=apiKey="$LLM_APIKEY" \
  --dry-run=client -o yaml | kubectl apply -f -
```

- Remove the entire gateway-token reuse/generate block and the `openclaw-config` Secret creation (the operator now owns `openclaw-config`).

- Rewrite the `--help` text: replace the `CUBEPILOT_MODEL_PROVIDERS` block with:

```
Required:
  CUBEPILOT_LLM_APIKEY / --llm-apikey <apiKey>
      apiKey for the platform default LLM (DeepSeek, the builtin model's
      endpoint). Stored in the cubepilot-llm Secret; the operator renders it
      into the gateway config. A running install can add more LLMs from the
      Portal (Agent Config -> LLM 配置).
```

- Remove the `openclaw-config` references from the header comment (lines 6-12).

- [ ] **Step 2: Update e2e.sh.**

In `scripts/e2e.sh`:

- Replace the `CUBEPILOT_MODEL_PROVIDERS` required check with `CUBEPILOT_LLM_APIKEY`:

```bash
[ -n "${CUBEPILOT_LLM_APIKEY:-}" ] || fail "CUBEPILOT_LLM_APIKEY is required"
```

- Replace the "verify shared secrets" step: the `openclaw-config` Secret is now operator-generated, so wait for it after the operator rollout instead of checking it in the secrets step:

```bash
step "verify shared secrets"
kubectl -n "$NAMESPACE" get secret agent-kubeconfig >/dev/null 2>&1 || fail "secret agent-kubeconfig missing"
kubectl -n "$NAMESPACE" get secret cubepilot-llm >/dev/null 2>&1 || fail "secret cubepilot-llm missing"
ok "secrets"
```

- In the "verify deployments ready" step, after the operator rollout, wait for the operator-generated config:

```bash
step "verify operator-generated openclaw-config"
for _ in $(seq 1 30); do
  kubectl -n "$NAMESPACE" get secret openclaw-config >/dev/null 2>&1 && break
  sleep 2
done
kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.gatewayToken}' | base64 -d | grep -q . \
  || fail "openclaw-config: gatewayToken empty"
kubectl -n "$NAMESPACE" get secret openclaw-config -o jsonpath='{.data.openclaw\.json}' | base64 -d \
  | jq -e '.gateway.mode == "local"' >/dev/null 2>&1 || fail "openclaw-config: openclaw.json invalid"
ok "operator-generated config"
```

- Update the header comment example (`CUBEPILOT_MODEL_PROVIDERS` -> `CUBEPILOT_LLM_APIKEY`).

- [ ] **Step 3: Update the CI workflow.**

In `.github/workflows/e2e.yaml`:

- In the `test` job, remove the line `- run: bash scripts/test-openclaw-config.sh`.
- In the `e2e` job, change the secret + invocation:

```yaml
      - name: Run e2e (deploy, or deploy + chat when the provider secret is set)
        env:
          CHAT_KEY: ${{ secrets.CUBEPILOT_LLM_APIKEY }}
        run: |
          set -euo pipefail
          if [ -n "$CHAT_KEY" ]; then
            echo "::group::e2e deploy + chat (real provider)"
            CUBEPILOT_LLM_APIKEY="$CHAT_KEY" CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh
            echo "::endgroup::"
          else
            echo "::group::e2e deploy only (placeholder provider)"
            CUBEPILOT_LLM_APIKEY='sk-placeholder' scripts/e2e.sh
            echo "::endgroup::"
          fi
```

Also update the comment above the step (mention the GitHub secret `CUBEPILOT_LLM_APIKEY`).

- [ ] **Step 4: Delete the jq renderer and its test.**

```bash
git rm deploy/openclaw-config.jq scripts/test-openclaw-config.sh
```

Verify nothing else references them: `grep -rn "openclaw-config.jq\|test-openclaw-config" .github Makefile scripts/ deploy/` should return nothing.

- [ ] **Step 5: Update the chart (token env + RBAC).**

In `deploy/charts/cubepilot/templates/_helpers.tpl`, remove the `CUBEPILOT_GATEWAY_TOKEN` env block:

```yaml
- name: CUBEPILOT_GATEWAY_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secrets.openclawConfig }}
      key: gatewayToken
```

In `deploy/charts/cubepilot/templates/rbac.yaml`:

- Operator namespaced Role: add `create;update;patch` to the secrets rule (so it can reconcile `openclaw-config`):

```yaml
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
```

- Add an API namespaced Role + RoleBinding for credential Secret creation:

```yaml
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ .Values.api.name }}
  namespace: {{ .Release.Namespace }}
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ .Values.api.name }}
  namespace: {{ .Release.Namespace }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ .Values.api.name }}
subjects:
  - kind: ServiceAccount
    name: {{ .Values.serviceAccounts.api }}
    namespace: {{ .Release.Namespace }}
```

- API ClusterRole: add `update` to the `agenttemplates` rule (the add-LLM handler appends models):

```yaml
  - apiGroups: ["ai.cubestack.io"]
    resources: ["agenttemplates", "skills", "tasktemplates", "taskruns"]
    verbs: ["get", "list", "watch"]
```

becomes:

```yaml
  - apiGroups: ["ai.cubestack.io"]
    resources: ["agenttemplates", "skills", "tasktemplates", "taskruns"]
    verbs: ["get", "list", "watch", "update"]
```

- [ ] **Step 6: Lint the chart.**

Run: `make lint`
Expected: PASS.

- [ ] **Step 7: Commit.**

```bash
git add scripts/setup.sh scripts/e2e.sh .github/workflows/e2e.yaml \
  deploy/charts/cubepilot/templates/_helpers.tpl deploy/charts/cubepilot/templates/rbac.yaml
git rm deploy/openclaw-config.jq scripts/test-openclaw-config.sh
git commit -s -m "$(cat <<'EOF'
feat(setup): retire CUBEPILOT_MODEL_PROVIDERS; --llm-apikey seeds the default LLM

setup.sh creates only cubepilot-llm (apiKey) + agent-kubeconfig; the operator
owns openclaw-config (rendered from AgentTemplate) and the gateway token.
Deletes the jq renderer and its test; e2e/CI switch to CUBEPILOT_LLM_APIKEY;
chart drops the token env injection and grants operator/api secret + template
write RBAC (issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

### Task 7: Docs

**Files:**
- Modify: `README.md`
- Modify: `scripts/setup.sh --help` (already rewritten in Task 6; verify)
- Modify: `docs/cubepilot/cubepilot-design.md` §3.3
- Modify: `docs/cubepilot/implementation-status.md`
- Modify: `docs/cubepilot/api.md` (already partly updated in Task 1; add `/api/llms`)

**Interfaces:**
- Consumes: the final CLI (`--llm-apikey`) and API (`POST /api/llms`).

- [ ] **Step 1: Rewrite the README provider section.**

Replace the "Adding or changing providers after install" section (README lines ~133-162) with a declarative-flow section:

```markdown
### LLM providers (declarative)

The gateway config (providers, model allowlist, gateway token) is generated by
the operator from the `AgentTemplate` inline `models` list and referenced
credential Secrets — it lives in the `openclaw-config` Secret and is reconciled
automatically. `setup.sh` only seeds one working LLM:

```bash
CUBEPILOT_LLM_APIKEY='sk-...' scripts/setup.sh   # creates the cubepilot-llm Secret
```

The builtin `agent-for-cloud` template carries the platform default model
(`deepseek-v4-flash`, endpoint `https://api.deepseek.com`, credential
`cubepilot-llm`). To add another LLM after install, use the Portal
(Agent Config -> LLM 配置): give a model name, an OpenAI-compatible endpoint,
and an apiKey (leave empty for a public model). The operator re-renders the
gateway and the model becomes selectable — no hand-edited Secret.
```

Also update the "Prerequisites" bullet (line ~67) from `CUBEPILOT_MODEL_PROVIDERS` to `CUBEPILOT_LLM_APIKEY`, and the e2e invocation examples (lines ~177-182) to `CUBEPILOT_LLM_APIKEY='sk-...'`.

- [ ] **Step 2: Update the design doc §3.3.**

In `docs/cubepilot/cubepilot-design.md`, replace the §3.3 model description with:

```markdown
## 3.3 模型（内联，无独立 CRD）

不单独建 Model CRD：模型配置**内联在 AgentTemplate 的 `models` 列表**，每项含
`name`（目录名 = 选择 key = 网关 provider key = 后端模型名）+ `endpoint`（必填，
OpenAI 兼容 base URL）+ `credentialRef`（可选，`LocalObjectReference`；public 模型没有）。

- 所有模型都是具体端点；`defaultModel` 从 models 里选默认，`AgentInstance.selectedModel` 覆盖。
- 网关配置（providers + allowlist + 网关 token）由 operator 从模板 models + 凭据 Secret
  声明式生成，写入 `openclaw-config` Secret；不再由安装时环境变量决定。
- 加一个 LLM = 往模板加一条模型（+ 非 public 时一个凭据 Secret），Portal「LLM 配置」即可完成。
- 多模型路由/模型目录治理是阶段二的事。
```

- [ ] **Step 3: Update implementation-status.md.**

Add a section noting: `CUBEPILOT_MODEL_PROVIDERS` retired; `TemplateModelSpec` is `{name, endpoint, credentialRef?}` (no `provider`/`modelId`); the operator generates `openclaw-config`; `POST /api/llms` + web "LLM 配置".

- [ ] **Step 4: Add `/api/llms` to api.md.**

Add:

```markdown
### POST /api/llms

Adds an OpenAI-compatible model to the platform catalog (appends to the builtin
`agent-for-cloud` AgentTemplate; creates a credential Secret when `apiKey` is
present). The operator renders it into the gateway config.

Body: `{ "name": "qwen2.5-72b", "endpoint": "https://api.example.com/v1", "apiKey": "sk-..." }`
(omit `apiKey` for a public model). Returns the created model.
```

- [ ] **Step 5: Verify setup.sh --help.**

Run `scripts/setup.sh --help` and confirm it documents `--llm-apikey` and no longer mentions `--providers-json`.

- [ ] **Step 6: Full verification.**

Run: `go build ./... && go test ./... && make lint && cd web && npm run build`
Expected: all PASS.

- [ ] **Step 7: Commit.**

```bash
git add README.md docs/cubepilot/cubepilot-design.md docs/cubepilot/implementation-status.md docs/cubepilot/api.md
git commit -s -m "$(cat <<'EOF'
docs: declarative LLM provider flow + Portal add-LLM

Rewrites the README provider section, design doc §3.3 (no provider/modelId;
endpoint required, credentialRef optional), implementation-status and api.md
for the declarative openclaw-config + /api/llms (issue #6).

Assisted-by: Claude Code
EOF
)"
```

---

## Final verification (run after all tasks)

```bash
cd /home/zhujian/code/github.com/suanova/cubepilot
go build ./... && go test ./... && make lint
grep -rn "CUBEPILOT_MODEL_PROVIDERS" --include="*.sh" --include="*.yaml" --include="*.md" . | grep -v docs/superpowers || echo "CUBEPILOT_MODEL_PROVIDERS fully retired"
```

Expected: build/tests/lint pass; `CUBEPILOT_MODEL_PROVIDERS` appears only in historical docs (`docs/superpowers/plans`, `docs/notes`), not in shipped code/scripts.

Optionally run the kind e2e: `CUBEPILOT_LLM_APIKEY='sk-real' CUBEPILOT_E2E_CHAT=1 scripts/e2e.sh`.

## Self-review notes

- Spec §4 (schema), §5 (renderer), §6 (token), §7 (reconciler), §8 (API), §9 (web), §10 (setup/CI/chart), §11 (docs) map to Tasks 1-7. §12 (testing) is covered by each task's tests. §13 decisions are encoded in Task 1 (schema) and Task 2 (renderer/token).
- Resolver override-ref change (spec Part B "Override ref") is in Task 1 Step 3; the old full-ref resolver tests are rewritten there.
- No placeholders: every code step contains the exact file content.
