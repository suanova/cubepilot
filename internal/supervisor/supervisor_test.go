package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/suanova/cubepilot/internal/instructions"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/resolver"
	"github.com/suanova/cubepilot/internal/skill"
)

// testAPI is testAPIWithCounter without the counter, for tests that do not care.
func testAPI(t *testing.T, cfg *resolver.ResolvedAgentConfig, user, skillsDir string) *httptest.Server {
	t.Helper()
	srv, _ := testAPIWithCounter(t, cfg, user, skillsDir)
	return srv
}

// testAPIWithCounter serves the same routes as before plus a count of
// skill-tar requests, so a test can assert the verification path did (or did
// not) go back to the API.
func testAPIWithCounter(t *testing.T, cfg *resolver.ResolvedAgentConfig, user, skillsDir string) (*httptest.Server, func() int) {
	t.Helper()
	var n int64
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/"+user+"/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Enveloped, like the real API: serving a bare ResolvedAgentConfig here
		// would let the fake disagree with production and hide a client that
		// stopped unwrapping.
		if err := json.NewEncoder(w).Encode(map[string]any{"config": cfg}); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	mux.HandleFunc("/internal/skills/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
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
	return srv, func() int { return int(atomic.LoadInt64(&n)) }
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
// API, extracts it into workspace/skills/<name>/, writes the .cubepilot.json
// marker, clears the stale entries it rendered itself, and verifies -- without
// re-pulling -- a tree it installed itself.
func TestSyncSkills(t *testing.T) {
	ws := t.TempDir()
	// Pre-existing stale skill dir (platform-rendered: it carries the marker) that
	// must be cleared once its name leaves the resolved set.
	staleDir := filepath.Join(ws, "skills", "stale")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "SKILL.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, skillMarker),
		[]byte(`{"skill":"stale","revision":"r0","tree":"x"}`), 0o644); err != nil {
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
	raw, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", skillMarker))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var marker skillMarkerFile
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if marker.Skill != "cluster-inspection" || marker.Revision != "rev1" || len(marker.Tree) != 64 {
		t.Errorf("marker = %+v; want skill cluster-inspection, revision rev1, 64-char tree", marker)
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "stale")); !os.IsNotExist(err) {
		t.Errorf("stale skill dir not cleared")
	}

	// Same resolved identity -> the tree this process installed verifies and the
	// skill is not re-pulled.
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
}

// TestSyncSkillsKeepsAgentAuthoredSkill verifies a skill directory the platform
// did not render is left alone: no marker, no ownership, no deletion.
func TestSyncSkillsKeepsAgentAuthoredSkill(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	// The agent's own skill, authored through skill_workshop: no marker.
	own := filepath.Join(ws, "skills", "my-own-skill")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("# Mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("syncSkills: %v", err)
	}
	// The platform skill was really installed, not merely skipped.
	if got := requests(); got != 1 {
		t.Fatalf("install fetches = %d, want 1", got)
	}
	if b, err := os.ReadFile(filepath.Join(own, "SKILL.md")); err != nil || string(b) != "# Mine\n" {
		t.Errorf("agent-authored skill was touched: %q, %v", b, err)
	}
	// A later revision change (the cleanup runs on every sync) must still leave it.
	cfg.Revision = "rev2"
	cfg.Skills[0].Revision = "rev2"
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("syncSkills after revision change: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("revision change did not re-fetch (fetches = %d, want 2)", got)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("agent-authored skill removed on a revision change: %v", err)
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
		Agent:    "cubepilot",
		Instance: "li-ming-cubepilot",
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

// TestSkillSyncFailureDoesNotGateGateway pins the rule that a skill-sync failure
// must not keep the gateway down. The supervisor waits for the initial config
// before it starts the gateway, so returning a skill error from the poll would
// leave the Pod Running with the gateway never started: a Skill CR pointing at a
// missing tar, or an unreachable repository, would look like a hung pod with no
// Failed signal for the operator to act on. Whether a skill tar is reachable does
// not change whether the gateway can run, so the failure is logged and retried on
// the next poll.
func TestSkillSyncFailureDoesNotGateGateway(t *testing.T) {
	ws := t.TempDir()
	const user = "li.ming"
	cfg := &resolver.ResolvedAgentConfig{
		Revision: "rev1",
		Agent:    "cubepilot",
		Instance: "li-ming-cubepilot",
		// The resolved set names a skill whose tar the repository cannot serve.
		Skills: []resolver.ResolvedSkill{
			{Name: "gone", Path: "gone/v1.tar.gz", Sha256: "deadbeef", Revision: "rev1"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/"+user+"/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"config": cfg}); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	// Every skill tar fetch fails -- an unreachable repository, not a missing one.
	mux.HandleFunc("/internal/skills/", http.NotFound)
	mux.HandleFunc("/internal/gateway/config/"+user, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gateway":{"mode":"local"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := New(Config{
		APIURL:     srv.URL,
		User:       user,
		Workspace:  ws,
		ConfigPath: filepath.Join(t.TempDir(), "openclaw.json"),
	})
	s.http = srv.Client()

	// The skill failure is not a poll failure ...
	changed, err := s.poll(context.Background())
	if err != nil {
		t.Fatalf("poll surfaced a skill-sync failure: %v", err)
	}
	if !changed {
		t.Error("poll did not report the first application of the revision")
	}
	// ... and the revision was still recorded, so the config side is not gated
	// either.
	if s.current != "rev1" {
		t.Errorf("current = %q, want rev1", s.current)
	}
	// ... nor does it stop the boot path from returning. A context deadline here
	// is the failure mode the fix removes: the wait loop retrying a skill error
	// forever while the gateway is never started.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.waitForInitialConfig(ctx); err != nil {
		t.Fatalf("a skill-sync failure gated the gateway: %v", err)
	}
	// The gateway config really landed, so the return above is not vacuous.
	if b, err := os.ReadFile(s.cfg.ConfigPath); err != nil || len(b) == 0 {
		t.Errorf("gateway config not written: %q, %v", b, err)
	}
}

// TestPollSyncsOnChange verifies a skill content revision change re-pulls the
// tar (the marker advances to the new revision).
func TestPollSyncsOnChange(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	_ = seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nVersion 1.")

	srv := testAPI(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()

	cfg1 := &resolver.ResolvedAgentConfig{
		Revision: "rev-1",
		Agent:    "cubepilot",
		Instance: "li-ming-cubepilot",
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
		Agent:    "cubepilot",
		Instance: "li-ming-cubepilot",
		Skills: []resolver.ResolvedSkill{
			{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Revision: "r2"},
		},
	}
	if err := s.syncSkills(context.Background(), cfg2); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", skillMarker))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var marker skillMarkerFile
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if marker.Revision != "r2" {
		t.Errorf("marker revision = %q, want r2", marker.Revision)
	}
}

// TestSyncSkillRestoresDrift verifies an edit to an installed platform skill is
// corrected on the next sync, and that a clean tree is not re-fetched.
func TestSyncSkillRestoresDrift(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("install fetches = %d, want 1", got)
	}
	// A clean tree verifies without fetching.
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := requests(); got != 1 {
		t.Errorf("a clean tree re-fetched (fetches = %d, want 1)", got)
	}
	// Drift: the agent edits the installed SKILL.md.
	body := filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md")
	if err := os.WriteFile(body, []byte("# Inspection\n\nTampered."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("drift did not re-fetch (fetches = %d, want 2)", got)
	}
	restored, err := os.ReadFile(body)
	if err != nil || !strings.Contains(string(restored), "Original content.") {
		t.Errorf("drift not corrected: %q, %v", restored, err)
	}
}

// TestSyncSkillReinstallsWhenDirectoryIsGone covers the unreadable-tree arm: a
// wanted skill whose directory has been removed is reinstalled rather than
// reported as an error. Deleting the directory is the cheapest drift the agent
// can cause, and the same arm is what lets a skill that left the resolved set and
// returned land again.
func TestSyncSkillReinstallsWhenDirectoryIsGone(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("install fetches = %d, want 1", got)
	}
	if err := os.RemoveAll(filepath.Join(ws, "skills", "cluster-inspection")); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("a removed skill directory did not trigger a re-fetch (fetches = %d, want 2)", got)
	}
	body, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil || !strings.Contains(string(body), "Original content.") {
		t.Errorf("skill not reinstalled: %q, %v", body, err)
	}
}

// TestSyncSkillRestoresDeletedMarker verifies a deleted ownership marker is
// treated as drift. The marker is excluded from the tree hash (its name is the
// hash's skip argument), so an agent that deletes it from an otherwise intact
// platform skill directory leaves the content hash matching -- yet the marker is
// the directory's only ownership record: cleanup removes a directory only when the
// marker is there, so without this the withdrawn skill could never be swept and
// the directory would be indistinguishable from one the agent authored.
func TestSyncSkillRestoresDeletedMarker(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := filepath.Join(ws, "skills", "cluster-inspection")
	before, err := skill.TreeHash(dir, skillMarker)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, skillMarker)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	// The premise of the fix, asserted rather than assumed: deleting the marker
	// leaves the content hash unchanged, so the hash alone cannot see this drift.
	if after, err := skill.TreeHash(dir, skillMarker); err != nil || after != before {
		t.Fatalf("removing the marker changed the tree hash (%q -> %q, err %v); the test no longer covers the gap it exists for", before, after, err)
	}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("a deleted ownership marker did not trigger a re-fetch (fetches = %d, want 2)", got)
	}
	if !markerPresent(dir) {
		t.Error("ownership marker not restored")
	}
	body, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || !strings.Contains(string(body), "Original content.") {
		t.Errorf("content not restored: %q, %v", body, err)
	}
}

// TestSyncSkillDetectsForgedMarker verifies the marker cannot be used to make
// drift permanent: an agent that rewrites the skill and recomputes the marker's
// tree, leaving the revision alone, is still corrected.
func TestSyncSkillDetectsForgedMarker(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, _ := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := filepath.Join(ws, "skills", "cluster-inspection")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Inspection\n\nTampered."), 0o644); err != nil {
		t.Fatal(err)
	}
	forged, err := skill.TreeHash(dir, skillMarker)
	if err != nil {
		t.Fatal(err)
	}
	forgedMarker, err := json.Marshal(skillMarkerFile{Skill: "cluster-inspection", Revision: "rev1", Tree: forged})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillMarker), forgedMarker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("heal: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || !strings.Contains(string(restored), "Original content.") {
		t.Errorf("forged marker defeated the check: %q, %v", restored, err)
	}
}

// TestSyncSkillRefetchesOnNewRevision verifies new platform content lands even
// though the installed tree matched the previous expectation.
func TestSyncSkillRefetchesOnNewRevision(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha1 := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nVersion 1.")
	srv, _ := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg1 := &resolver.ResolvedAgentConfig{Revision: "rev-1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha1, Revision: "r1"},
	}}
	if err := s.syncSkills(context.Background(), cfg1); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The platform publishes v2 at the same path; the resolved identity changes.
	sha2 := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nVersion 2.")
	cfg2 := &resolver.ResolvedAgentConfig{Revision: "rev-2", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha2, Revision: "r2"},
	}}
	if err := s.syncSkills(context.Background(), cfg2); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil || !strings.Contains(string(got), "Version 2.") {
		t.Errorf("new revision not installed: %q, %v", got, err)
	}
}

// TestSyncSkillsVerifiesAtPodStart verifies a fresh supervisor process does not
// trust the marker it finds on the PVC: it re-fetches and re-verifies.
func TestSyncSkillsVerifiesAtPodStart(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	first := New(Config{Workspace: ws, APIURL: srv.URL})
	first.http = srv.Client()
	if err := first.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	// A new process: same workspace, empty expectations.
	second := New(Config{Workspace: ws, APIURL: srv.URL})
	second.http = srv.Client()
	if err := second.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("pod start: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("a pod start did not re-fetch (fetches = %d, want 2)", got)
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

	// A hand-edited file is rewritten: the comparison is against the bytes on
	// disk, so a write the supervisor did not make is visible.
	if err := os.WriteFile(path, []byte(`{"models":{"providers":{"rogue":{}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	want := `{"models":{"providers":{"my-glm":{"api":"openai-completions"}}}}`
	changed, err = s.applyGatewayConfig([]byte(want))
	if err != nil {
		t.Fatalf("apply after edit: %v", err)
	}
	if !changed {
		t.Error("a hand-edited file should be rewritten")
	}
	if got, _ = os.ReadFile(path); string(got) != want {
		t.Errorf("edited file not restored: %q, want %q", got, want)
	}
}

// TestLoadFromEnvHasNoAmbientAPIURL pins issue #172: the supervisor must never
// invent an API URL. Any built-in default is a namespace the supervisor cannot
// know it belongs to -- the previous one named `cubepilot`, so every install
// elsewhere dialed a namespace it did not own and hung indefinitely. The agent
// Pod supplies the URL (k8s.APIURLEnv); with it unset the config stays empty
// and Run fails loudly instead.
func TestLoadFromEnvHasNoAmbientAPIURL(t *testing.T) {
	t.Setenv(k8s.APIURLEnv, "")
	if got := LoadFromEnv().APIURL; got != "" {
		t.Errorf("LoadFromEnv APIURL = %q with %s unset, want no ambient default", got, k8s.APIURLEnv)
	}
}

// TestRunRequiresAPIURL verifies a missing API URL fails fast with a
// configuration error. The reported symptom (issue #172) was a Pod stuck 0/1
// Ready forever because the supervisor retried an unreachable URL; a loud
// failure at startup is diagnosable, an endless retry loop is not.
func TestRunRequiresAPIURL(t *testing.T) {
	s := New(Config{User: "alice"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := s.Run(ctx)
	if err == nil {
		t.Fatal("Run with no APIURL returned nil, want a configuration error")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("Run with no APIURL retried until the context expired (%v), want an immediate configuration error", err)
	}
	if !strings.Contains(err.Error(), k8s.APIURLEnv) {
		t.Errorf("Run error = %v, want it to name %s", err, k8s.APIURLEnv)
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

// TestManagedBlockBody covers the composed platform text: the persona always, the
// instructions section only when there is one.
func TestManagedBlockBody(t *testing.T) {
	body := managedBlockBody("")
	if !strings.Contains(body, "CubePilot 操作约定") || strings.Contains(body, systemPromptHeader) {
		t.Errorf("empty instructions should yield the persona alone:\n%s", body)
	}
	body = managedBlockBody("Answer in Chinese.")
	if !strings.Contains(body, "CubePilot 操作约定") ||
		!strings.Contains(body, systemPromptHeader) ||
		!strings.Contains(body, "Answer in Chinese.") {
		t.Errorf("persona and instructions should both be present:\n%s", body)
	}
}

// TestReconcileManagedBlock covers the block rewrite: written on first sync,
// replaced in place, and agent-authored content outside it preserved verbatim.
func TestReconcileManagedBlock(t *testing.T) {
	t.Run("writes the block into an empty file", func(t *testing.T) {
		got := string(reconcileManagedBlock(nil, "persona text"))
		if !strings.Contains(got, systemPromptStart) || !strings.Contains(got, "persona text") {
			t.Fatalf("block missing:\n%s", got)
		}
	})
	t.Run("replaces the block and keeps content around it", func(t *testing.T) {
		agentNotes := "## 我的笔记\n\n记住昨天那个 Pod 的问题。\n"
		first := reconcileManagedBlock([]byte(agentNotes), "old")
		second := string(reconcileManagedBlock(first, "new"))
		if strings.Contains(second, "old") || !strings.Contains(second, "new") {
			t.Errorf("block not replaced:\n%s", second)
		}
		if !strings.Contains(second, agentNotes) {
			t.Errorf("agent content lost:\n%s", second)
		}
	})
	t.Run("keeps content written after the block after it", func(t *testing.T) {
		// Content the agent writes *below* the block must stay below it. The
		// suffix is a distinct section, not part of the block: splicing it back
		// out of order silently reorders the file under the agent, which is what
		// the dropped guard caught once before.
		notes := "## 我的笔记\n\n这个必须留在块后面。"
		file := append(append([]byte{}, reconcileManagedBlock(nil, "old")...), []byte("\n\n"+notes+"\n")...)
		got := string(reconcileManagedBlock(file, "new"))
		if !strings.Contains(got, notes) {
			t.Fatalf("agent content after the block was lost:\n%s", got)
		}
		end := strings.Index(got, systemPromptEnd)
		notesIdx := strings.Index(got, notes)
		if end < 0 || notesIdx < end {
			t.Errorf("agent content after the block was moved before it (end=%d, notes=%d):\n%s", end, notesIdx, got)
		}
	})
	t.Run("removes the block when the desired body is empty", func(t *testing.T) {
		if got := reconcileManagedBlock(nil, ""); got != nil {
			t.Errorf("reconcileManagedBlock(nil, \"\") = %q, want nil (nothing to write)", got)
		}
		notes := "## 我的笔记\n\n块移除后也要留下。"
		file := append(append([]byte{}, reconcileManagedBlock(nil, "old")...), []byte("\n\n"+notes+"\n")...)
		got := string(reconcileManagedBlock(file, ""))
		if !strings.Contains(got, notes) {
			t.Errorf("agent content lost when the block is removed:\n%s", got)
		}
		if strings.Contains(got, systemPromptStart) || strings.Contains(got, systemPromptEnd) {
			t.Errorf("block not removed (markers still present):\n%s", got)
		}
	})
	t.Run("tolerates an unterminated block", func(t *testing.T) {
		broken := systemPromptStart + "\nleft over\n"
		got := string(reconcileManagedBlock([]byte(broken), "fresh"))
		if strings.Count(got, systemPromptStart) != 1 || !strings.Contains(got, "fresh") {
			t.Errorf("unterminated block not repaired:\n%s", got)
		}
	})
}

// TestSyncAgentsFile verifies the file is written from the persona and the
// resolved instructions, that an unchanged file is left alone, and that a
// rejected instruction set keeps the last-good file instead of replacing the
// persona with a broken block.
func TestSyncAgentsFile(t *testing.T) {
	ws := t.TempDir()
	s := New(Config{Workspace: ws})
	agentsPath := filepath.Join(ws, agentsFileName)

	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	raw, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "CubePilot 操作约定") || !strings.Contains(string(raw), "Answer in Chinese.") {
		t.Fatalf("persona and instructions not written:\n%s", raw)
	}
	// Exactly one instructions heading: the body composer emits it around the
	// instructions, and reconcileManagedBlock must not add a second one.
	if n := strings.Count(string(raw), systemPromptHeader); n != 1 {
		t.Errorf("configured file should carry exactly one %q heading; count = %d:\n%s",
			systemPromptHeader, n, raw)
	}
	// The write path is atomic, so it must not litter the workspace the agent
	// reads with the temp file it renamed away.
	temps, err := filepath.Glob(filepath.Join(ws, "."+agentsFileName+".tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(temps) != 0 {
		t.Errorf("temp files left behind: %v", temps)
	}
	// A second sync with the same config is a real no-rewrite, not an identical
	// write. The rename replaces the file, so a rewrite changes the inode and
	// os.SameFile turns false -- a bytes-only comparison would pass even if the
	// poll rewrote the file every 10 seconds, which is exactly the churn the
	// content-hash guard exists to prevent.
	beforeStat, err := os.Stat(agentsPath)
	if err != nil {
		t.Fatalf("stat before idempotent sync: %v", err)
	}
	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("idempotent sync: %v", err)
	}
	repeat, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read after idempotent sync: %v", err)
	}
	if !bytes.Equal(repeat, raw) {
		t.Error("an unchanged sync rewrote the file")
	}
	afterStat, err := os.Stat(agentsPath)
	if err != nil {
		t.Fatalf("stat after idempotent sync: %v", err)
	}
	if !os.SameFile(beforeStat, afterStat) {
		t.Error("an unchanged sync replaced the file (inode changed)")
	}
	// Agent content outside the block survives a resync.
	appended := string(raw) + "\n## 我的笔记\n\nkeep me\n"
	if err := os.WriteFile(agentsPath, []byte(appended), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("resync: %v", err)
	}
	after, _ := os.ReadFile(agentsPath)
	if !strings.Contains(string(after), "keep me") {
		t.Errorf("agent content outside the block was lost:\n%s", after)
	}
	// A hand-edited block is restored.
	if err := os.WriteFile(agentsPath, []byte(systemPromptStart+"\nrogue\n"+systemPromptEnd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("heal: %v", err)
	}
	healed, _ := os.ReadFile(agentsPath)
	if strings.Contains(string(healed), "rogue") || !strings.Contains(string(healed), "CubePilot 操作约定") {
		t.Errorf("edited block not restored:\n%s", healed)
	}
	// An oversized instruction set is skipped (last-good file kept).
	before := healed
	oversized := &resolver.ResolvedAgentConfig{Instructions: strings.Repeat("x", instructions.MaxChars+1)}
	if err := s.syncAgentsFile(oversized); err != nil {
		t.Fatalf("oversized: %v", err)
	}
	kept, _ := os.ReadFile(agentsPath)
	if !bytes.Equal(kept, before) {
		t.Error("an oversized instruction set should leave the last-good file in place")
	}
	// Instructions carrying a reserved marker are refused for the same reason:
	// an embedded end marker would be mistaken for the block terminator,
	// preserve the trailing suffix as content, and grow the file on every poll.
	// The managed block is always present now (it carries the persona), so this
	// asserts the file is byte-identical to the last-good content rather than
	// asserting the block is absent.
	markerBefore := kept
	malicious := "prompt " + systemPromptEnd + " with a marker"
	for i := 1; i <= 3; i++ {
		if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: malicious}); err != nil {
			t.Fatalf("marker sync #%d: %v", i, err)
		}
		markerAfter, err := os.ReadFile(agentsPath)
		if err != nil {
			t.Fatalf("read after marker sync #%d: %v", i, err)
		}
		if !bytes.Equal(markerAfter, markerBefore) {
			t.Errorf("marker sync #%d grew the file: %d -> %d bytes", i, len(markerBefore), len(markerAfter))
		}
	}
}

// TestSyncAgentsFileNilConfig covers the nil-config path through the sync: a
// failed poll (or an instance with no resolved config) used to leave the file
// untouched, and now reconciles it to the persona alone. The persona is the part
// of the managed block that depends on no config, so it must land either way.
func TestSyncAgentsFileNilConfig(t *testing.T) {
	ws := t.TempDir()
	s := New(Config{Workspace: ws})
	agentsPath := filepath.Join(ws, agentsFileName)

	if err := s.syncAgentsFile(nil); err != nil {
		t.Fatalf("syncAgentsFile(nil): %v", err)
	}
	raw, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read %s: %v", agentsPath, err)
	}
	if !strings.Contains(string(raw), "CubePilot 操作约定") {
		t.Errorf("persona not written on the nil-config path:\n%s", raw)
	}
	// No user-configured instructions section: the header count is the observable
	// for it. Only the body composer emits the heading, and only around
	// instructions, so a persona-only file must carry none -- a count of one would
	// mean reconcileManagedBlock is labelling the platform persona as
	// user-configured again.
	if n := strings.Count(string(raw), systemPromptHeader); n != 0 {
		t.Errorf("nil config should yield no %q section; header count = %d, want 0:\n%s",
			systemPromptHeader, n, raw)
	}
	// The same mislabel, asserted structurally: the persona is the block's first
	// line after the start marker. A heading there means the persona renders under
	// a label claiming it is user-configured.
	lines := strings.Split(string(raw), "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != systemPromptStart || i+1 >= len(lines) {
			continue
		}
		if strings.TrimSpace(lines[i+1]) == systemPromptHeader {
			t.Errorf("persona is preceded by %q in a file with no instructions:\n%s", systemPromptHeader, raw)
		}
	}
}

// TestSyncAgentsFileSymlink verifies a symlinked AGENTS.md is treated as
// absent and replaced by a regular file on the next sync -- the supervisor and
// the gateway share the pod uid, so a workspace AGENTS.md symlink must not be
// followed to an arbitrary path (regression for the no-follow read + exclusive
// temp write).
func TestSyncAgentsFileSymlink(t *testing.T) {
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
	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
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

// TestFetchConfigWireShape pins how the internal config endpoint's payload is
// read. Three shapes must be told apart, and conflating them is silent rather
// than loud: a config decoded from the wrong shape is a zero value, and a zero
// config skips the device pairing that gates the approval channel -- surfacing
// later as an unrelated NOT_PAIRED on the first gated turn.
func TestFetchConfigWireShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		// wantErr is set for shapes that are a contract mismatch.
		wantErr bool
		// wantRevision is checked when wantErr is false.
		wantRevision string
	}{
		{
			name:         "enveloped",
			body:         `{"config":{"revision":"rev-1","instance":"li-ming-cubepilot","devicePublicKey":"PUBKEY"}}`,
			wantRevision: "rev-1",
		},
		{
			// The endpoint answers null for a user whose instance has no resolved
			// config yet, and poll() has a branch for that state. It is not an
			// error and must not become one.
			name:         "null config is a valid empty config",
			body:         `{"config":null}`,
			wantRevision: "",
		},
		{
			name:    "missing config key is a contract mismatch",
			body:    `{}`,
			wantErr: true,
		},
		{
			name:    "bare config (the pre-envelope shape) is a contract mismatch",
			body:    `{"revision":"rev-1","instance":"li-ming-cubepilot"}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			s := New(Config{APIURL: srv.URL, User: "li.ming"})
			s.http = srv.Client()

			cfg, err := s.fetchConfig(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("fetchConfig accepted %s as %+v; want a contract error", tc.body, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetchConfig(%s): %v", tc.body, err)
			}
			if cfg.Revision != tc.wantRevision {
				t.Fatalf("revision = %q, want %q", cfg.Revision, tc.wantRevision)
			}
		})
	}
}

// TestFetchConfigKeepsDeviceKey guards the field whose loss is silent: pairing
// no-ops on an empty key, so nothing complains until a gated turn fails.
func TestFetchConfigKeepsDeviceKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"config":{"revision":"rev-1","devicePublicKey":"PUBKEY"}}`))
	}))
	defer srv.Close()

	s := New(Config{APIURL: srv.URL, User: "li.ming"})
	s.http = srv.Client()

	cfg, err := s.fetchConfig(context.Background())
	if err != nil {
		t.Fatalf("fetchConfig: %v", err)
	}
	if cfg.DevicePublicKey != "PUBKEY" {
		t.Fatalf("devicePublicKey = %q, want PUBKEY", cfg.DevicePublicKey)
	}
}

func TestRevisionLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		want string
	}{
		{"first sync has no previous revision", "", "(initial)"},
		{"later syncs name the revision being replaced", "42c94f84db82", "42c94f84db82"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := revisionLabel(tc.from); got != tc.want {
				t.Errorf("revisionLabel(%q) = %q, want %q", tc.from, got, tc.want)
			}
		})
	}
}
