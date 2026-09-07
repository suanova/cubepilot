package store

import "testing"

// TestDefaultAgentConfigNoPlatformModel locks the model-less default (issue
// #117): a fresh install must not present a DeepSeek model that is absent from
// the (model-less) agent-for-cloud template, or saving Agent Config would set a
// stale selectedModel and brick the instance (fail-closed resolver).
func TestDefaultAgentConfigNoPlatformModel(t *testing.T) {
	if got := DefaultAgentConfig().Model; got != "" {
		t.Fatalf("DefaultAgentConfig().Model = %q, want empty (no platform default LLM)", got)
	}
	st, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg, err := st.GetAgentConfig()
	if err != nil {
		t.Fatalf("GetAgentConfig: %v", err)
	}
	if cfg.Model != "" {
		t.Fatalf("fresh store default model = %q, want empty", cfg.Model)
	}
}
