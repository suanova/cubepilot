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
