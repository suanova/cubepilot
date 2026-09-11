package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRender(t *testing.T) {
	providers := []Provider{
		{Key: "deepseek-v4-flash", BaseURL: "https://api.deepseek.com", APIKey: "CUBEPILOT_LLM_DEEPSEEK_V4_FLASH", Model: "deepseek-v4-flash"},
		{Key: "qwen", BaseURL: "http://localhost:11434/v1", Model: "qwen2.5-72b"}, // public, no key
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
				Model  map[string]any `json:"model"`
				Models map[string]any `json:"models"`
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
	// model = {primary} and the allowlist lives at agents.defaults.models
	// (siblings) -- this is the OpenClaw schema the old jq produced.
	if cfg.Agents.Defaults.Model["primary"] != "deepseek-v4-flash/deepseek-v4-flash" {
		t.Errorf("primary = %v", cfg.Agents.Defaults.Model["primary"])
	}
	if _, ok := cfg.Agents.Defaults.Models["deepseek-v4-flash/deepseek-v4-flash"]; !ok {
		t.Error("allowlist missing primary ref")
	}
	if _, ok := cfg.Agents.Defaults.Models["qwen/qwen2.5-72b"]; !ok {
		t.Error("allowlist missing public ref")
	}
}

// TestRenderPublicModelRemoteEndpoint covers the reported failure: a model with
// no credential at a non-local endpoint. OpenClaw synthesizes a no-auth
// placeholder only for local base URLs, so a keyless provider anywhere else
// resolves no credential at all and every turn fails before a request is sent.
// The renderer must give such a provider a value it can resolve.
func TestRenderPublicModelRemoteEndpoint(t *testing.T) {
	b, err := Render("tok", "pub/pub", []Provider{
		{Key: "pub", BaseURL: "http://106.75.230.113:15910/v1", Model: "pub"},
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

func TestRenderEmpty(t *testing.T) {
	b, err := Render("tok", "", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty render")
	}
}

// TestRenderDeterministic verifies the render output is byte-stable for the
// same input (sorted keys) and still human-readable (indented).
func TestRenderDeterministic(t *testing.T) {
	providers := []Provider{{Key: "a", BaseURL: "https://x", Model: "a"}, {Key: "b", BaseURL: "https://y", Model: "b", APIKey: "CUBEPILOT_LLM_B"}}
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
