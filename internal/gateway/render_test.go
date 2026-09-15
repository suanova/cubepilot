package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRender(t *testing.T) {
	providers := []Provider{
		{Key: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com", APIKey: "CUBEPILOT_LLM_DEEPSEEK_V4_FLASH", Models: []string{"deepseek-v4-flash"}},
		{Key: "qwen", BaseURL: "http://localhost:11434/v1", Models: []string{"qwen2.5-72b"}}, // public, no key
	}
	b, err := Render("tok", "deepseek-v4-flash/deepseek-v4-flash", providers)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Models struct {
			Providers map[string]struct {
				API    string          `json:"api"`
				APIKey json.RawMessage `json:"apiKey"`
				Models []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
		Agents struct {
			Defaults struct {
				Model       map[string]any `json:"model"`
				Models      map[string]any `json:"models"`
				ModelPolicy struct {
					Allow []string `json:"allow"`
				} `json:"modelPolicy"`
			} `json:"defaults"`
		} `json:"agents"`
		Secrets struct {
			Providers map[string]struct {
				Source string `json:"source"`
				Path   string `json:"path"`
				Mode   string `json:"mode"`
			} `json:"providers"`
		} `json:"secrets"`
		Memory struct {
			Search struct {
				Enabled bool `json:"enabled"`
				Store   struct {
					FTS struct {
						Tokenizer string `json:"tokenizer"`
					} `json:"fts"`
					Vector struct {
						Enabled *bool `json:"enabled"`
					} `json:"vector"`
				} `json:"store"`
			} `json:"search"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := cfg.Models.Providers["deepseek-v4-flash"]
	// The credential is a file SecretRef into the supervisor-written keys.json,
	// never a literal key.
	var keyRef struct {
		Source   string `json:"source"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := json.Unmarshal(d.APIKey, &keyRef); err != nil {
		t.Fatalf("apiKey should be a file SecretRef object: %s", d.APIKey)
	}
	if d.API != "openai-completions" || keyRef.Source != "file" || keyRef.Provider != "cubepilot-keys" ||
		keyRef.ID != "/CUBEPILOT_LLM_DEEPSEEK_V4_FLASH" || d.Models[0].ID != "deepseek-v4-flash" {
		t.Errorf("deepseek provider wrong: %+v (apiKey=%s)", d, d.APIKey)
	}
	// A credential-less model still needs a resolvable apiKey: OpenClaw fails
	// every turn with "No API key resolved" for a provider whose credential it
	// cannot resolve. The rendered value is a placeholder, never a secret.
	q := cfg.Models.Providers["qwen"]
	var qKey string
	if err := json.Unmarshal(q.APIKey, &qKey); err != nil {
		t.Fatalf("public provider apiKey should be a literal string: %s", q.APIKey)
	}
	if qKey != "cubepilot-no-auth" || q.Models[0].ID != "qwen2.5-72b" {
		t.Errorf("public provider should carry the no-auth placeholder: %+v", q)
	}
	// The file-secret provider must point at the emptyDir keys.json.
	sp := cfg.Secrets.Providers["cubepilot-keys"]
	if sp.Source != "file" || sp.Path != "/mnt/cubepilot-keys/keys.json" || sp.Mode != "json" {
		t.Errorf("secrets.providers.cubepilot-keys wrong: %+v", sp)
	}
	// Memory is keyword-only (issue #163): semantic vector search is off (no
	// embedding provider), FTS5 keeps keyword search with the CJK tokenizer.
	if !cfg.Memory.Search.Enabled {
		t.Error("memory.search.enabled should be true (keyword search stays on)")
	}
	if cfg.Memory.Search.Store.Vector.Enabled == nil || *cfg.Memory.Search.Store.Vector.Enabled {
		t.Errorf("memory.search.store.vector.enabled should be false, got %v", cfg.Memory.Search.Store.Vector.Enabled)
	}
	if cfg.Memory.Search.Store.FTS.Tokenizer != "trigram" {
		t.Errorf("memory.search.store.fts.tokenizer = %q, want trigram", cfg.Memory.Search.Store.FTS.Tokenizer)
	}
	// model = {primary}, models = the per-ref entries, and modelPolicy.allow is
	// the explicit allowlist -- siblings, matching the OpenClaw
	// agents.defaults schema.
	if cfg.Agents.Defaults.Model["primary"] != "deepseek-v4-flash/deepseek-v4-flash" {
		t.Errorf("primary = %v", cfg.Agents.Defaults.Model["primary"])
	}
	if _, ok := cfg.Agents.Defaults.Models["deepseek-v4-flash/deepseek-v4-flash"]; !ok {
		t.Error("allowlist missing primary ref")
	}
	if _, ok := cfg.Agents.Defaults.Models["qwen/qwen2.5-72b"]; !ok {
		t.Error("allowlist missing public ref")
	}
	if len(cfg.Agents.Defaults.ModelPolicy.Allow) != 2 ||
		cfg.Agents.Defaults.ModelPolicy.Allow[0] != "deepseek-v4-flash/deepseek-v4-flash" ||
		cfg.Agents.Defaults.ModelPolicy.Allow[1] != "qwen/qwen2.5-72b" {
		t.Errorf("modelPolicy.allow = %v, want both refs sorted", cfg.Agents.Defaults.ModelPolicy.Allow)
	}
}

// TestRenderPublicModelRemoteEndpoint covers the reported failure: a model with
// no credential at a non-local endpoint. OpenClaw synthesizes a no-auth
// placeholder only for local base URLs, so a keyless provider anywhere else
// resolves no credential at all and every turn fails before a request is sent.
// The renderer must give such a provider a value it can resolve.
func TestRenderPublicModelRemoteEndpoint(t *testing.T) {
	b, err := Render("tok", "pub/pub", []Provider{
		{Key: "pub", BaseURL: "http://106.75.230.113:15910/v1", Models: []string{"pub"}},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Models struct {
			Providers map[string]struct {
				APIKey json.RawMessage `json:"apiKey"`
			} `json:"providers"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var key string
	if err := json.Unmarshal(cfg.Models.Providers["pub"].APIKey, &key); err != nil {
		t.Fatalf("public provider apiKey should be a literal string: %s", cfg.Models.Providers["pub"].APIKey)
	}
	if key != "cubepilot-no-auth" {
		t.Errorf("apiKey = %q, want the no-auth placeholder", key)
	}
}

// TestRenderProviderWithNoModels covers a Provider with an empty Models slice.
// The input is unreachable through the API -- spec.providers requires at least
// one id -- so this guards the renderer against it rather than defining a
// product rule. Render builds its entries by iterating the ids and never
// indexes them, so it writes the provider through with an empty models array
// and contributes no ref: models.providers carries the entry (OpenClaw's schema
// requires a custom provider to declare models, and an empty array satisfies
// that -- it checks the value is an array, not that it is non-empty), while
// agents.defaults.models and modelPolicy.allow are both built from the ids and
// stay empty.
func TestRenderProviderWithNoModels(t *testing.T) {
	b, err := Render("tok", "", []Provider{
		{Key: "empty", BaseURL: "http://vllm.ai.svc:8000/v1", APIKey: "CUBEPILOT_LLM_EMPTY"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Models struct {
			Providers map[string]struct {
				Models json.RawMessage `json:"models"`
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
	pv, ok := cfg.Models.Providers["empty"]
	if !ok {
		t.Fatal("models.providers.empty missing")
	}
	if got := string(pv.Models); got != "[]" {
		t.Errorf("models = %s, want an empty array", got)
	}
	if len(cfg.Agents.Defaults.Models) != 0 || len(cfg.Agents.Defaults.ModelPolicy.Allow) != 0 {
		t.Errorf("an id-less provider must contribute no ref: models = %v, allow = %v",
			cfg.Agents.Defaults.Models, cfg.Agents.Defaults.ModelPolicy.Allow)
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

// TestRenderDeterministic verifies the render output is byte-stable for the
// same input (sorted keys) and still human-readable (indented).
func TestRenderDeterministic(t *testing.T) {
	providers := []Provider{{Key: "a", BaseURL: "https://x", Models: []string{"a"}}, {Key: "b", BaseURL: "https://y", Models: []string{"b"}, APIKey: "CUBEPILOT_LLM_B"}}
	b1, err := Render("tok", "a/a", providers)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b2, err := Render("tok", "a/a", providers)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("Render not deterministic:\n%s\n---\n%s", b1, b2)
	}
	if !bytes.Contains(b1, []byte("\n  ")) {
		t.Errorf("Render should be indented JSON for humans: %s", b1)
	}
}
