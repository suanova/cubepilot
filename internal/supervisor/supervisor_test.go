package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/resolver"
)

// testAPI serves a fixed resolved config on /internal/agents/{user}/config.
func testAPI(t *testing.T, cfg *resolver.ResolvedAgentConfig, user string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/"+user+"/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(cfg); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRenderSkills verifies the supervisor renders resolved skills
// into workspace/skills/<name>/SKILL.md and clears stale entries.
func TestRenderSkills(t *testing.T) {
	ws := t.TempDir()
	// Pre-existing stale skill dir that must be cleared.
	if err := os.MkdirAll(filepath.Join(ws, "skills", "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "skills", "stale", "SKILL.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{Workspace: ws})
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "abc123",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Title: "Cluster Intelligent Inspection", Description: "Inspect the cluster", Instructions: "Read-only inspection.", Revision: "rev1"},
		},
	}
	if err := s.renderSkills(context.Background(), cfg); err != nil {
		t.Fatalf("renderSkills: %v", err)
	}

	skill, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if !strings.Contains(string(skill), "name: cluster-inspection") || !strings.Contains(string(skill), "Read-only inspection.") {
		t.Errorf("skill content wrong: %s", skill)
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "stale")); !os.IsNotExist(err) {
		t.Errorf("stale skill dir not cleared")
	}
}

// TestPollNoChange verifies poll is a no-op when the revision is unchanged.
func TestPollNoChange(t *testing.T) {
	ws := t.TempDir()
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "fixed",
		Agent:    "agent-for-cloud",
		Instance: "li-ming-agent-for-cloud",
	}
	srv := testAPI(t, cfg, "li.ming")

	s := New(Config{APIURL: srv.URL, User: "li.ming", Workspace: ws})
	// First poll: applies the config for the first time ("" -> fixed) --
	// reports changed because the gateway has not booted yet (Run applies
	// before the first start, so no restart actually happens at boot).
	changed, err := s.poll(context.Background())
	if err != nil {
		t.Fatalf("poll #1: %v", err)
	}
	if !changed {
		t.Error("poll #1 should report changed (first application)")
	}
	if s.current != "fixed" {
		t.Errorf("current = %q, want fixed", s.current)
	}
	// Second poll: same revision -> no-op.
	changed, err = s.poll(context.Background())
	if err != nil {
		t.Fatalf("poll #2: %v", err)
	}
	if changed {
		t.Error("poll #2 should be a no-op")
	}
}

// TestPollRendersOnChange verifies a revision change re-renders skills.
func TestPollRendersOnChange(t *testing.T) {
	ws := t.TempDir()
	s := New(Config{Workspace: ws})

	cfg1 := &resolver.ResolvedAgentConfig{
		Revision: "rev-1",
		Agent:    "agent-for-cloud",
		Instance: "li-ming-agent-for-cloud",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Title: "Inspection", Description: "d", Instructions: "Version 1", Revision: "r1"},
		},
	}
	if err := s.renderSkills(context.Background(), cfg1); err != nil {
		t.Fatal(err)
	}
	s.current = "rev-1"

	cfg2 := &resolver.ResolvedAgentConfig{
		Revision: "rev-2",
		Agent:    "agent-for-cloud",
		Instance: "li-ming-agent-for-cloud",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Title: "Inspection", Description: "d", Instructions: "Version 2: added one more check.", Revision: "r2"},
		},
	}
	if err := s.renderSkills(context.Background(), cfg2); err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "Version 2") {
		t.Errorf("skill not re-rendered: %s", skill)
	}
}

// TestApplyGatewayConfig verifies the gateway config change detection: the
// supervisor writes the pulled config when its content changes, reports the
// change once, and converges (no repeated restarts for unchanged content).
func TestApplyGatewayConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway", "openclaw.json")
	s := New(Config{ConfigPath: path})

	changed, err := s.applyGatewayConfig([]byte(`{"models":{"providers":{}}}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !changed {
		t.Error("first apply should report changed")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"models":{"providers":{}}}` {
		t.Errorf("config not written correctly: %q, err=%v", got, err)
	}

	// Same content (e.g. the poll re-pulls) -> no change, no restart.
	changed, err = s.applyGatewayConfig([]byte(`{"models":{"providers":{}}}`))
	if err != nil {
		t.Fatalf("apply same: %v", err)
	}
	if changed {
		t.Error("same content should not report changed")
	}

	// A provider added (e.g. an LLM added on the portal) -> changed.
	changed, err = s.applyGatewayConfig([]byte(`{"models":{"providers":{"my-glm":{"api":"openai-completions"}}}}`))
	if err != nil {
		t.Fatalf("apply new: %v", err)
	}
	if !changed {
		t.Error("changed content should report changed")
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"models":{"providers":{"my-glm":{"api":"openai-completions"}}}}` {
		t.Errorf("config = %q, want the new content", got)
	}
}

// TestApplyGatewayConfigDisabled verifies an empty ConfigPath disables the pull.
func TestApplyGatewayConfigDisabled(t *testing.T) {
	s := New(Config{})
	changed, err := s.applyGatewayConfig([]byte(`{"models":{"providers":{}}}`))
	if err != nil || changed {
		t.Errorf("empty ConfigPath: changed=%v err=%v, want no-op", changed, err)
	}
}

// TestSeedGatewayConfigFallback verifies the cold-start seed: when the internal
// API is unreachable, the mounted read-only config is copied to the writable
// path so the gateway still boots with the operator's config.
func TestSeedGatewayConfigFallback(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seed, []byte(`{"gateway":{"mode":"local"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		ConfigPath: filepath.Join(dir, "gateway", "openclaw.json"),
		SeedPath:   seed,
		APIURL:     "http://127.0.0.1:1", // unreachable -> fall back to seed
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := s.seedGatewayConfig(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(got) != `{"gateway":{"mode":"local"}}` {
		t.Errorf("config = %q, want the seeded content", got)
	}
}
