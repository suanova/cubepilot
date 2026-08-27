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
				API    string `json:"api"`
				APIKey string `json:"apiKey"`
				Models []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
		Agents struct {
			Defaults struct {
				Model struct {
					Primary string         `json:"primary"`
					Models  map[string]any `json:"models"`
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
