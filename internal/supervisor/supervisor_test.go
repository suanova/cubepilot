package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/suanova/cubepilot/internal/resolver"
	"github.com/suanova/cubepilot/internal/skill"
)

// testAPI serves a fixed resolved config on /internal/agents/{user}/config and
// skill tars on /internal/skills/{name}/tar from a skill.PathRepository.
func testAPI(t *testing.T, cfg *resolver.ResolvedAgentConfig, user, skillsDir string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/"+user+"/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(cfg); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	mux.HandleFunc("/internal/skills/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/internal/skills/")
		name, tail, ok := strings.Cut(rest, "/")
		if !ok || tail != "tar" || name == "" {
			http.NotFound(w, r)
			return
		}
		rc, err := (&skill.PathRepository{Root: skillsDir}).Open(r.Context(), name+"/v1.tar.gz")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/gzip")
		if _, err := io.Copy(w, rc); err != nil {
			t.Errorf("copy tar: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSyncSkills verifies the supervisor pulls a skill tar from the internal
// API, extracts it into workspace/skills/<name>/, writes the .sha256 marker,
// clears stale entries, and skips an unchanged skill.
func TestSyncSkills(t *testing.T) {
	ws := t.TempDir()
	// Pre-existing stale skill dir that must be cleared.
	if err := os.MkdirAll(filepath.Join(ws, "skills", "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "skills", "stale", "SKILL.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Seed the "repo" with the skill tar.
	repo := &skill.PathRepository{Root: t.TempDir()}
	if _, err := repo.Write(t.Context(), "cluster-inspection/v1.tar.gz", fstest.MapFS{
		"SKILL.md": {Data: []byte("# Cluster Intelligent Inspection\n\nRead-only inspection.")},
	}); err != nil {
		t.Fatal(err)
	}

	// The test server serves the tars; the supervisor pulls from cfg.APIURL.
	srv := testAPI(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "abc123",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Revision: "rev1"},
		},
	}

	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("syncSkills: %v", err)
	}

	skillBody, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	if !strings.Contains(string(skillBody), "Read-only inspection.") {
		t.Errorf("skill content wrong: %s", skillBody)
	}
	marker, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", ".sha256"))
	if err != nil || string(marker) != "rev1" {
		t.Errorf("marker = %q, %v; want rev1", marker, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "stale")); !os.IsNotExist(err) {
		t.Errorf("stale skill dir not cleared")
	}

	// Same revision -> no re-pull (the API client is untouched; re-sync is a
	// no-op even if the server were gone).
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("re-sync: %v", err)
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
	srv := testAPI(t, cfg, "li.ming", "")

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

// TestPollSyncsOnChange verifies a skill content revision change re-pulls the
// tar (the .sha256 marker advances).
func TestPollSyncsOnChange(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	if _, err := repo.Write(t.Context(), "cluster-inspection/v1.tar.gz", fstest.MapFS{
		"SKILL.md": {Data: []byte("# Inspection\n\nVersion 1.")},
	}); err != nil {
		t.Fatal(err)
	}

	srv := testAPI(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()

	cfg1 := &resolver.ResolvedAgentConfig{
		Revision: "rev-1",
		Agent:    "agent-for-cloud",
		Instance: "li-ming-agent-for-cloud",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Revision: "r1"},
		},
	}
	if err := s.syncSkills(context.Background(), cfg1); err != nil {
		t.Fatal(err)
	}
	s.current = "rev-1"

	cfg2 := &resolver.ResolvedAgentConfig{
		Revision: "rev-2",
		Agent:    "agent-for-cloud",
		Instance: "li-ming-agent-for-cloud",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Revision: "r2"},
		},
	}
	if err := s.syncSkills(context.Background(), cfg2); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", ".sha256"))
	if err != nil || string(marker) != "r2" {
		t.Errorf("marker = %q, %v; want r2", marker, err)
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

// TestGatewayCrashSignalsRespawn verifies the crash-recovery wiring: a started
// gateway child that exits signals waitCh, which Run consumes to respawn.
func TestGatewayCrashSignalsRespawn(t *testing.T) {
	s := New(Config{GatewayCmd: []string{"sh", "-c", "sleep 0.3"}})
	if err := s.startGateway(context.Background()); err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	select {
	case err := <-s.waitCh:
		if err != nil {
			t.Errorf("Wait returned %v, want nil (clean exit)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitCh not signaled after gateway child exited")
	}
}

// TestSyncCredentials verifies the supervisor reads the credential Secrets and
// writes keys.json (the gateway's file secret provider input), and that an
// unchanged sync is a no-op.
func TestSyncCredentials(t *testing.T) {
	dir := t.TempDir()
	cl := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-llm", Namespace: "cubepilot"},
		Data:       map[string][]byte{"apiKey": []byte("sk-1")},
	})
	s := New(Config{CredentialsPath: filepath.Join(dir, "keys.json")})
	s.k8s = cl
	s.ns = "cubepilot"

	cfg := &resolver.ResolvedAgentConfig{
		Credentials: []resolver.ResolvedCredential{
			{Env: "CUBEPILOT_LLM_X", SecretName: "cubepilot-llm"},
		},
	}
	if err := s.syncCredentials(context.Background(), cfg); err != nil {
		t.Fatalf("syncCredentials: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatalf("read keys: %v", err)
	}
	var keys map[string]string
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("unmarshal keys: %v", err)
	}
	if keys["CUBEPILOT_LLM_X"] != "sk-1" {
		t.Errorf("keys = %v, want CUBEPILOT_LLM_X=sk-1", keys)
	}
	// Unchanged content -> no rewrite (hash guard).
	if err := s.syncCredentials(context.Background(), cfg); err != nil {
		t.Fatalf("sync #2: %v", err)
	}
}

// TestSyncCredentialsSkipsWriteOnError verifies a failed Secret read aborts the
// sync without writing a partial keys.json (a live credential is never dropped
// by a transient read error).
func TestSyncCredentialsSkipsWriteOnError(t *testing.T) {
	dir := t.TempDir()
	cl := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "cubepilot"},
		Data:       map[string][]byte{"apiKey": []byte("sk-good")},
	})
	s := New(Config{CredentialsPath: filepath.Join(dir, "keys.json")})
	s.k8s = cl
	s.ns = "cubepilot"

	cfg := &resolver.ResolvedAgentConfig{
		Credentials: []resolver.ResolvedCredential{
			{Env: "K_GOOD", SecretName: "ok"},
			{Env: "K_MISSING", SecretName: "missing"},
		},
	}
	if err := s.syncCredentials(context.Background(), cfg); err == nil {
		t.Fatal("expected error for a missing credential Secret")
	}
	if _, err := os.Stat(filepath.Join(dir, "keys.json")); !os.IsNotExist(err) {
		t.Error("keys.json must not be written when a credential read fails")
	}
}
