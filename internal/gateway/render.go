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
