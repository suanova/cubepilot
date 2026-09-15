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
