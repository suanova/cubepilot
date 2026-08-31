package gateway

import (
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
				API    string `json:"api"`
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
	q := cfg.Models.Providers["qwen"]
	if len(q.APIKey) != 0 || q.Models[0].ID != "qwen2.5-72b" {
		t.Errorf("public provider should be keyless: %+v", q)
	}
	// The file-secret provider must point at the emptyDir keys.json.
	sp := cfg.Secrets.Providers["cubepilot-keys"]
	if sp.Source != "file" || sp.Path != "/mnt/cubepilot-keys/keys.json" || sp.Mode != "json" {
		t.Errorf("secrets.providers.cubepilot-keys wrong: %+v", sp)
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

func TestRenderEmpty(t *testing.T) {
	b, err := Render("tok", "", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty render")
	}
}
