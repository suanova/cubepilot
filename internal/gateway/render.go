// Package gateway renders the shared openclaw.json gateway config from
// AgentTemplate model declarations (design §3.3 / issue #6). It replaces the
// install-time deploy/openclaw-config.jq renderer.
package gateway

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/suanova/cubepilot/internal/k8s"
)

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

// PublicModelAPIKey is the apiKey rendered for a model that has no credential
// (a "public" model). OpenClaw fails every turn with "No API key resolved" for
// a provider it cannot resolve a credential for, and the OpenAI SDK will not
// construct a client without one -- so a keyless provider is not an option.
// OpenClaw synthesizes its own no-auth placeholder, but only for a base URL on
// a loopback/private network (isLocalProviderBaseUrl); rendering ours
// unconditionally keeps a single rule -- every provider in the config has a
// resolvable apiKey -- instead of mirroring OpenClaw's locality check here.
// The value is never a real secret, and an endpoint that needs no
// authentication ignores the Authorization header it produces. It must not
// collide with an OpenClaw marker (custom-local, ollama-local,
// secretref-managed, ...), which are recognized and routed down paths that
// would resolve nothing again.
const PublicModelAPIKey = "cubepilot-no-auth"

// Render builds the openclaw.json bytes for the given providers. Static
// scaffolding (workspace, sandbox, gateway port/auth, tools, sessions) mirrors
// the defaults the old jq renderer used.
func Render(token, primary string, providers []Provider) ([]byte, error) {
	providersOut := map[string]any{}
	modelsOut := map[string]any{}
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
	// A provider serving both a bare id and its own prefixed form ("qwen3-8b"
	// and "vllm/qwen3-8b") collapses both to one ref. modelsOut is a map and
	// absorbs that, but the allow list is an array: without this it would carry
	// the same ref twice. OpenClaw folds allow into a Set and the web client
	// de-duplicates the same case, so an entry repeated here is only noise.
	allowed := map[string]bool{}
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
			if !allowed[key] {
				allowed[key] = true
				allowOut = append(allowOut, key)
			}
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
	cfg := map[string]any{
		"models": map[string]any{"providers": providersOut},
		"agents": map[string]any{
			"defaults": map[string]any{
				"workspace": "/home/node/.openclaw/workspace",
				"model":     map[string]any{"primary": primary},
				"models":    modelsOut,
				// The allowlist is stated explicitly rather than left to the
				// legacy reading of the agents.defaults.models keys, which stops
				// applying once OpenClaw stamps meta.migrations.modelPolicyAllowlist.
				"modelPolicy": map[string]any{"allow": allowOut},
				"sandbox":     map[string]any{"mode": "off"},
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
		// Skill authoring is user-requested only. The runtime defaults this block
		// to autonomous (capture and repair both on), which would let the agent
		// accumulate its own skills in the workspace unasked -- instructions it
		// then follows, invisible to the platform. "off" disables the autonomous
		// path while leaving the explicit create-then-apply flow, which is what a
		// user request uses. approvalPolicy stays "auto" because there is no
		// proposal-review surface here: a "pending" policy would make every apply
		// wait for an approval nobody can give. Rendered rather than left to the
		// default so an upstream default change cannot silently alter platform
		// behaviour.
		"skills": map[string]any{
			"workshop": map[string]any{
				"autonomous":     map[string]any{"mode": "off"},
				"approvalPolicy": "auto",
			},
		},
		// Memory runs FTS5 keyword search only (issue #163): with no embedding
		// provider configured, OpenClaw's default vector search resolves
		// embeddings to an unavailable "openai" adapter, so the vector index was
		// never built and the gateway logged degraded-recall warnings on every
		// start. Disable the semantic index until an embedding model exists; the
		// trigram tokenizer is what makes keyword search work for CJK text (the
		// default unicode61 does not segment it).
		"memory": map[string]any{
			"search": map[string]any{
				"enabled": true,
				"store": map[string]any{
					"vector": map[string]any{"enabled": false},
					"fts":    map[string]any{"tokenizer": "trigram"},
				},
			},
		},
		// The file-secret provider that resolves the per-model apiKey refs from
		// the keys.json the supervisor writes into the emptyDir.
		"secrets": map[string]any{
			"providers": map[string]any{
				k8s.CredProviderName: map[string]any{
					"source": "file",
					"path":   k8s.CredentialsPath,
					"mode":   "json",
				},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render openclaw.json: %w", err)
	}
	return b, nil
}
