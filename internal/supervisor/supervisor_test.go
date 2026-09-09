package supervisor

import (
	"bytes"
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

	"github.com/suanova/cubepilot/internal/instructions"
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

// seedTar packs a single SKILL.md into the repo and returns its sha256.
func seedTar(t *testing.T, repo *skill.PathRepository, relPath, body string) string {
	t.Helper()
	data, err := skill.Pack(fstest.MapFS{"SKILL.md": {Data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	sha, err := repo.WriteBytes(t.Context(), relPath, data)
	if err != nil {
		t.Fatal(err)
	}
	return sha
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
	sha1 := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Cluster Intelligent Inspection\n\nRead-only inspection.")

	// The test server serves the tars; the supervisor pulls from cfg.APIURL.
	srv := testAPI(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "abc123",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha1, Revision: "rev1"},
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

// TestSyncSkillRejectsShaMismatch verifies a downloaded tar whose content does
// not match the CRD's source.sha256 is rejected (and nothing is installed).
func TestSyncSkillRejectsShaMismatch(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	_ = seedTar(t, repo, "bad/v1.tar.gz", "# good content\n")
	srv := testAPI(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "r",
		Skills: []resolver.ResolvedSkill{
			{Name: "bad", Path: "bad/v1.tar.gz", Sha256: "deadbeef", Revision: "rev1"}, // wrong digest
		},
	}
	if err := s.syncSkills(context.Background(), cfg); err == nil {
		t.Fatal("sha256 mismatch should error")
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "bad")); !os.IsNotExist(err) {
		t.Errorf("skill dir should not be installed on mismatch: %v", err)
	}
}

// TestSyncSkillPreservesOldOnFailure verifies a failed fetch/extract leaves
// the previously installed skill directory intact (stage-then-swap).
func TestSyncSkillPreservesOldOnFailure(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "skills", "good")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sha256"), []byte("rev-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The repo has no tar for "good" -> the API 404s the fetch.
	srv := testAPI(t, nil, "", t.TempDir())
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "r",
		Skills: []resolver.ResolvedSkill{
			{Name: "good", Path: "good/v1.tar.gz", Revision: "rev-new"},
		},
	}
	if err := s.syncSkills(context.Background(), cfg); err == nil {
		t.Fatal("missing tar should error")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err != nil || string(b) != "old content" {
		t.Errorf("old skill not preserved: %q, %v", b, err)
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
	_ = seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nVersion 1.")

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

// TestReconcileInstructions covers the pure AGENTS.md block rewrite: write on
// first sync, update in place, remove when empty, preserve persona content
// outside the markers, and tolerate an unterminated block.
func TestReconcileInstructions(t *testing.T) {
	const persona = "# CubePilot 操作约定\n\n## 执行原则\n- 先查后答\n"

	t.Run("writes block when none present", func(t *testing.T) {
		got := string(reconcileInstructions([]byte(persona), "Use kubectl read-only."))
		if !strings.Contains(got, systemPromptStart) || !strings.Contains(got, "Use kubectl read-only.") {
			t.Fatalf("block missing:\n%s", got)
		}
		if !strings.HasPrefix(got, persona) {
			t.Errorf("persona prefix lost:\n%s", got)
		}
	})

	t.Run("updates existing block in place", func(t *testing.T) {
		once := reconcileInstructions([]byte(persona), "old instructions")
		twice := reconcileInstructions(once, "new instructions")
		if strings.Contains(string(twice), "old instructions") {
			t.Error("stale instructions survived update")
		}
		if !strings.Contains(string(twice), "new instructions") {
			t.Error("updated instructions missing")
		}
		if !strings.HasPrefix(string(twice), persona) {
			t.Error("persona prefix lost on update")
		}
		// Exactly one block after the update.
		if got := strings.Count(string(twice), systemPromptStart); got != 1 {
			t.Errorf("block count = %d, want 1", got)
		}
	})

	t.Run("removes block when instructions empty", func(t *testing.T) {
		with := reconcileInstructions([]byte(persona), "to be removed")
		without := reconcileInstructions(with, "")
		if strings.Contains(string(without), systemPromptStart) {
			t.Errorf("marker survived removal:\n%s", without)
		}
		if string(without) != persona {
			t.Errorf("persona not restored exactly:\n%q\nwant:\n%q", without, persona)
		}
	})

	t.Run("preserves content after the end marker", func(t *testing.T) {
		// Simulate agent-authored content appended after a stale block.
		with := reconcileInstructions([]byte(persona), "OLD-STALE-TEXT")
		withAgent := append(with, []byte("\n\n# Agent note\nkeep me")...)
		got := reconcileInstructions(withAgent, "new instructions")
		if !strings.Contains(string(got), "keep me") {
			t.Errorf("agent content after markers lost:\n%s", got)
		}
		if strings.Contains(string(got), "OLD-STALE-TEXT") {
			t.Error("old block content leaked")
		}
		if !strings.Contains(string(got), "new instructions") {
			t.Error("updated instructions missing")
		}
		// The managed block is spliced back between the persona prefix and the
		// agent-authored suffix, so the suffix stays after the instructions
		// (regression: it used to be moved before the replacement block).
		ib, kb := bytes.Index(got, []byte("new instructions")), bytes.Index(got, []byte("# Agent note"))
		if ib < 0 || kb < 0 || ib > kb {
			t.Errorf("agent content not kept after the managed block (instructions at %d, note at %d):\n%s", ib, kb, got)
		}
	})

	t.Run("tolerates unterminated block", func(t *testing.T) {
		broken := []byte(persona + "\n\n" + systemPromptStart + "\n## orphaned\nno end marker")
		got := reconcileInstructions(broken, "clean instructions")
		if strings.Contains(string(got), "orphaned") {
			t.Errorf("unterminated block content survived:\n%s", got)
		}
		if strings.Count(string(got), systemPromptStart) != 1 {
			t.Errorf("duplicate start markers after repair:\n%s", got)
		}
	})

	t.Run("empty file with empty instructions yields nil", func(t *testing.T) {
		if got := reconcileInstructions(nil, ""); got != nil {
			t.Errorf("nil+empty = %q, want nil (no file to write)", got)
		}
	})
}

// TestSyncInstructions verifies the supervisor writes the managed block into
// the workspace AGENTS.md, no-ops on an unchanged file, and refuses an
// oversized instruction set (keeps the last-good file).
func TestSyncInstructions(t *testing.T) {
	ws := t.TempDir()
	// Seed a persona like the image's workspace/AGENTS.md.
	persona := "# CubePilot 操作约定\n\n## 执行原则\n- 先查后答\n"
	agentsPath := filepath.Join(ws, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(persona), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Workspace: ws})

	// First sync writes the block.
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	raw, _ := os.ReadFile(agentsPath)
	if !strings.Contains(string(raw), "Answer in Chinese.") {
		t.Fatalf("instructions not written:\n%s", raw)
	}

	// Same instructions again -> content unchanged (no rewrite).
	before := raw
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	after, _ := os.ReadFile(agentsPath)
	if !bytes.Equal(before, after) {
		t.Error("unchanged instructions rewrote the file")
	}

	// Empty instructions -> block removed, persona intact.
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	raw, _ = os.ReadFile(agentsPath)
	if strings.Contains(string(raw), systemPromptStart) {
		t.Errorf("block not removed on empty instructions:\n%s", raw)
	}
	if string(raw) != persona {
		t.Errorf("persona not restored after clear:\n%q", raw)
	}

	// Oversized instructions are refused (no write, file keeps last-good).
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{Instructions: strings.Repeat("x", instructions.MaxChars+1)}); err != nil {
		t.Fatalf("oversize: %v", err)
	}
	raw, _ = os.ReadFile(agentsPath)
	if strings.Contains(string(raw), systemPromptStart) {
		t.Error("oversized instructions were written")
	}

	// Instructions containing either reserved marker are refused: an embedded
	// end marker would be mistaken for the block terminator, preserve the
	// suffix, and grow the file on every poll. Regression: sync the same
	// malicious text repeatedly and assert the file never gains a managed block
	// (and so cannot grow unbounded).
	malicious := "prompt " + systemPromptEnd + " with a marker"
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{Instructions: malicious}); err != nil {
		t.Fatalf("marker sync: %v", err)
	}
	raw, _ = os.ReadFile(agentsPath)
	if strings.Contains(string(raw), systemPromptStart) {
		t.Error("instructions with an embedded end marker were written")
	}
	// A second poll with the same text must not grow the file (would append a
	// fresh block each time if the marker were not rejected).
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{Instructions: malicious}); err != nil {
		t.Fatalf("marker re-sync: %v", err)
	}
	raw2, _ := os.ReadFile(agentsPath)
	if len(raw2) != len(raw) {
		t.Errorf("file grew across marker syncs: %d -> %d bytes", len(raw), len(raw2))
	}

	// A nil config (no instance) is a no-op too.
	if err := s.syncInstructions(nil); err != nil {
		t.Fatalf("nil cfg: %v", err)
	}

	// No leftover temp files (the atomic write uses an exclusive random-named
	// temp in the workspace dir and renames it away on success).
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+agentsFileName+".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestSyncInstructionsSymlinkAGENTS verifies a symlinked AGENTS.md is treated as
// absent and replaced by a regular file on the next sync -- the supervisor and
// the gateway share the pod uid, so a workspace AGENTS.md symlink must not be
// followed to an arbitrary path (regression for the no-follow read + exclusive
// temp write).
func TestSyncInstructionsSymlinkAGENTS(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not read"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(ws, "AGENTS.md")
	if err := os.Symlink(outside, agentsPath); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Workspace: ws})
	if err := s.syncInstructions(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("sync over symlink: %v", err)
	}
	fi, err := os.Lstat(agentsPath)
	if err != nil {
		t.Fatalf("lstat AGENTS.md: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("AGENTS.md still a symlink after sync")
	}
	raw, _ := os.ReadFile(agentsPath)
	if !strings.Contains(string(raw), "Answer in Chinese.") {
		t.Errorf("instructions not written over the symlink:\n%s", raw)
	}
	if strings.Contains(string(raw), "do not read") {
		t.Errorf("symlink target content was read and copied:\n%s", raw)
	}
}
