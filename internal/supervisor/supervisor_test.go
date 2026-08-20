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

// TestRenderSkills verifies the supervisor renders resolved capabilities
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
		Capabilities: []resolver.ResolvedCapability{
			{Name: "cluster-inspection", Title: "集群智能巡检", Description: "巡检集群", Instructions: "只读巡检。", Revision: "rev1"},
		},
	}
	if err := s.renderSkills(context.Background(), cfg); err != nil {
		t.Fatalf("renderSkills: %v", err)
	}

	skill, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if !strings.Contains(string(skill), "name: cluster-inspection") || !strings.Contains(string(skill), "只读巡检。") {
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
	// First poll: applies the config for the first time ("" → fixed) —
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
	// Second poll: same revision → no-op.
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
		Capabilities: []resolver.ResolvedCapability{
			{Name: "cluster-inspection", Title: "巡检", Description: "d", Instructions: "第一版", Revision: "r1"},
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
		Capabilities: []resolver.ResolvedCapability{
			{Name: "cluster-inspection", Title: "巡检", Description: "d", Instructions: "第二版：加一条检查", Revision: "r2"},
		},
	}
	if err := s.renderSkills(context.Background(), cfg2); err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "第二版") {
		t.Errorf("skill not re-rendered: %s", skill)
	}
}
